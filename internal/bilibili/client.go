// Package bilibili 实现 B 站网页投稿 API 的最小 Go 客户端：
// 登录校验（nav）、封面上传（cover/up）、视频预上传（preupload）+ upos 分片上传、
// 稿件提交（add/v3）。
//
// 协议来自社区逆向成果（参考 biliup-rs 与 bilibili-API-collect 的网页投稿流程），
// 不是官方开放接口：B 站没有对个人开发者开放的投稿 API，这条 Cookie 链路是
// biliup 等主流工具的通行做法。代价是字段/线路可能变动——所有非 2xx 或
// code!=0 的响应都会把响应体尾部带进错误信息，线上排查时先看日志再看这里。
//
// 本包不读配置、不起协程：Cookie 由调用方注入，ctx 决定超时，便于单测与复用。
package bilibili

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultMemberBase = "https://member.bilibili.com"
	defaultAPIBase    = "https://api.bilibili.com"
	// 投稿页框架的 Referer。B 站网页投稿接口对 UA/Referer 有校验，
	// 缺失或异常会直接触发风控（code 601 频控），这两个头不能省。
	uploadReferer = "https://member.bilibili.com/platform/upload/video/frame"
	webUA         = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	// maxRespBody 单个 API 响应的读取上限。防线路异常时把错误页/大文件读进内存。
	maxRespBody = 1 << 20
)

// Client 是绑定单个登录态的投稿客户端。
// memberBase/apiBase 为测试注入留的覆写点；正常使用走 NewClient。
type Client struct {
	http *http.Client

	sessdata   string
	biliJct    string
	dedeUserID string

	memberBase string
	apiBase    string
}

// NewClient 用浏览器 Cookie 三件套创建客户端。
// SESSDATA 保持从浏览器复制出来的原样（本身已是 URL 编码形态）。
func NewClient(sessdata, biliJct, dedeUserID string) *Client {
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		sessdata:   sessdata,
		biliJct:    biliJct,
		dedeUserID: dedeUserID,
		memberBase: defaultMemberBase,
		apiBase:    defaultAPIBase,
	}
}

// APIError 是 B 站返回的业务错误（HTTP 200 但 code != 0）。
// Code 保留原始值：调用方按需区分风控（601）、登录失效（-101）等。
type APIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	API     string `json:"api"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: code=%d %s", e.API, e.Code, e.Message)
}

// LoginInfo 是 nav 接口解析出的登录态摘要。
type LoginInfo struct {
	IsLogin bool   `json:"isLogin"`
	Mid     int64  `json:"mid"`
	Uname   string `json:"uname"`
	Level   int    `json:"level"`
}

// Nav 校验当前 Cookie 是否有效，返回账号摘要。
// 登录失效时 B 站返回 code=-101，这里转成普通 error（IsLogin 拿不到）。
func (c *Client) Nav(ctx context.Context) (LoginInfo, error) {
	var data LoginInfo
	if err := c.getJSON(ctx, c.apiBase+"/x/web-interface/nav", &data); err != nil {
		return LoginInfo{}, err
	}
	return data, nil
}

// cookieHeader 组装 Cookie 头；空字段跳过，避免出现 "SESSDATA=" 这种空值对。
func (c *Client) cookieHeader() string {
	parts := make([]string, 0, 3)
	if c.biliJct != "" {
		parts = append(parts, "bili_jct="+c.biliJct)
	}
	if c.sessdata != "" {
		parts = append(parts, "SESSDATA="+c.sessdata)
	}
	if c.dedeUserID != "" {
		parts = append(parts, "DedeUserID="+c.dedeUserID)
	}
	return strings.Join(parts, "; ")
}

// newRequest 构造带登录态与防风控头的请求。
func (c *Client) newRequest(ctx context.Context, method, u string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", webUA)
	req.Header.Set("Referer", uploadReferer)
	req.Header.Set("Origin", "https://member.bilibili.com")
	if ck := c.cookieHeader(); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	return req, nil
}

// doJSON 执行请求并解析 B 站统一信封 {code, message, data}。
// HTTP 非 2xx 与 code != 0 都转为 APIError（后者保留 code）。
func (c *Client) doJSON(ctx context.Context, api string, req *http.Request, out interface{}) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", api, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if err != nil {
		return fmt.Errorf("%s: 读取响应失败: %w", api, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d | %s", api, resp.StatusCode, tailBody(raw))
	}
	var envelope struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%s: 响应解析失败: %w | %s", api, err, tailBody(raw))
	}
	if envelope.Code != 0 {
		return &APIError{Code: envelope.Code, Message: envelope.Message, API: api}
	}
	if out != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("%s: data 解析失败: %w", api, err)
		}
	}
	return nil
}

// doFlatJSON 执行请求并解析**非信封**结构的响应（preupload / upos 系列是扁平 JSON，
// 没有 {code,message,data} 外壳）。HTTP 非 2xx 仍转为错误。
func (c *Client) doFlatJSON(ctx context.Context, api string, req *http.Request, out interface{}) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", api, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if err != nil {
		return fmt.Errorf("%s: 读取响应失败: %w", api, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d | %s", api, resp.StatusCode, tailBody(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: 响应解析失败: %w | %s", api, err, tailBody(raw))
	}
	return nil
}

func (c *Client) getJSON(ctx context.Context, u string, out interface{}) error {
	req, err := c.newRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	return c.doJSON(ctx, pathOf(u), req, out)
}

// pathOf 取 URL 的 path 部分用于错误信息。
func pathOf(u string) string {
	if parsed, err := url.Parse(u); err == nil && parsed.Path != "" {
		return parsed.Path
	}
	return u
}

// tailBody 取响应体尾部做诊断（压掉换行，截到 400 字符）。
func tailBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= 400 {
		return strings.ReplaceAll(s, "\n", " ")
	}
	return strings.ReplaceAll(s[len(s)-400:], "\n", " ")
}

// UploadCover 上传封面图，返回 B 站封面 URL。
// 表单字段 cover 的值必须是 data URI 形态（网页端就是这么发的）。
func (c *Client) UploadCover(ctx context.Context, imagePath string) (string, error) {
	raw, err := readFileLimited(imagePath, 8<<20)
	if err != nil {
		return "", err
	}
	mime := "image/jpeg"
	if strings.HasSuffix(strings.ToLower(imagePath), ".png") {
		mime = "image/png"
	}
	form := url.Values{}
	form.Set("cover", "data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(raw))
	form.Set("csrf", c.biliJct)

	req, err := c.newRequest(ctx, http.MethodPost, c.memberBase+"/x/vu/web/cover/up?csrf="+url.QueryEscape(c.biliJct), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		URL string `json:"url"`
	}
	if err := c.doJSON(ctx, "/x/vu/web/cover/up", req, &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", fmt.Errorf("cover/up 未返回封面地址")
	}
	return out.URL, nil
}

// ArchiveParams 是一次稿件提交所需的全部参数。
type ArchiveParams struct {
	Title     string
	Desc      string
	Tag       string // 逗号分隔，最多 10 个
	Cover     string // 封面 URL，可空（不传 B 站自动取封面）
	Tid       int    // 分区 ID
	Copyright int    // 1 自制 / 2 转载
	Source    string // 转载来源；Copyright=2 时 B 站校验必填
	Dynamic   string // 同步动态文案
	NoReprint bool   // 声明「未经允许禁止转载」
	// BizID 是视频上传返回的 biz_id，作为分 P 的 cid（现行 add/v3 必填语义）。
	BizID int64
	// VideoFilename 是上传返回的视频文件名（不含扩展名）。
	VideoFilename string
}

// maxTitleRunes B 站稿件标题上限（字符数，按 rune 计）。
const maxTitleRunes = 80

// SubmitArchive 提交稿件，返回 aid 与 bvid。
// 提交策略（对齐 bilibili-API-collect 文档与 biliup 实战流程）：
//  1. 先 GET x/geetest/pre/add 建立投稿会话（biliup 每次提交前的固定动作）；
//  2. POST add/v3（2024 版字段集）；
//  3. 若 add/v3 返回 21150「投稿入口升级中」，回退到老接口 x/vu/web/add 重试——
//     老接口对手动 tid 的兼容性更好（biliup 至今仍在使用）。
func (c *Client) SubmitArchive(ctx context.Context, p ArchiveParams) (int64, string, error) {
	p.Title = truncateRunes(strings.TrimSpace(p.Title), maxTitleRunes)
	filename := p.VideoFilename
	if idx := strings.LastIndexByte(filename, '.'); idx > 0 {
		filename = filename[:idx]
	}
	payload := map[string]interface{}{
		"copyright":          p.Copyright,
		"source":             p.Source,
		"cover":              p.Cover,
		"desc":               p.Desc,
		"desc_format_id":     0,
		"dynamic":            p.Dynamic,
		"interactive":        0,
		"no_reprint":         boolToInt(p.NoReprint),
		"no_disturbance":     0,
		"open_elec":          0,
		"origin_data":        map[string]interface{}{},
		"act_reserve_create": 0,
		"recreate":           -1,
		"is_360":             -1,
		"dolby":              0,
		"lossless_music":     0,
		"web_os":             3,
		"subtitle":           map[string]interface{}{"lan": "", "open": 0},
		"tag":                p.Tag,
		"tid":                p.Tid,
		"title":              p.Title,
		"up_selection_reply": false,
		"up_close_reply":     false,
		"up_close_danmu":     false,
		"videos": []map[string]interface{}{
			{"cid": p.BizID, "filename": filename, "title": "", "desc": ""},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "", err
	}
	u := c.memberBase + "/x/vu/web/add/v3?csrf=" + url.QueryEscape(c.biliJct) +
		"&ts=" + fmt.Sprintf("%d", time.Now().UnixMilli())
	req, err := c.newRequest(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		AID  int64  `json:"aid"`
		BvID string `json:"bvid"`
	}
	err = c.doJSON(ctx, "/x/vu/web/add/v3", req, &out)
	if err == nil {
		return out.AID, out.BvID, nil
	}
	// add/v3 报「投稿入口升级中」时回退老接口 x/vu/web/add（biliup 实战在用的通道）
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 21150 {
		return 0, "", err
	}
	log.Printf("[bilibili] add/v3 返回 21150，回退 x/vu/web/add 重试")
	if err := c.geetestPreAdd(ctx); err != nil {
		log.Printf("[bilibili] geetest/pre/add 预热失败（继续尝试提交）: %v", err)
	}
	req2, err := c.newRequest(ctx, http.MethodPost,
		c.memberBase+"/x/vu/web/add?csrf="+url.QueryEscape(c.biliJct), bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req2.Header.Set("Content-Type", "application/json")
	if err := c.doJSON(ctx, "/x/vu/web/add", req2, &out); err != nil {
		return 0, "", err
	}
	return out.AID, out.BvID, nil
}

// geetestPreAdd 提交前的会话预热（biliup 每次 submit 前的固定动作，返回值无需关心）。
func (c *Client) geetestPreAdd(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, c.memberBase+"/x/geetest/pre/add", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBody))
	return nil
}

// boolToInt 把 bool 转成 B 站接口要的 0/1。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// truncateRunes 按 rune 截断字符串。
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
