package bilibili

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// fallbackChunkSize 预上传未给出分片大小时的兜底值（4MiB）。
const fallbackChunkSize = 4 << 20

// UploadResult 是一次视频上传的产物：提交稿件所需的文件名与业务 ID。
type UploadResult struct {
	// Filename 不含扩展名，直接作 add/v3 的 videos[].filename。
	Filename string
	// BizID 作 add/v3 的 videos[].cid。
	BizID int64
}

// UploadVideo 完整执行一次视频上传：预上传 → upos 初始化 → 分片上传 → 合片。
//
// 协议与 bilibili-API-collect「创作中心上传」文档及 biliup-rs 对齐（2024 版 ugcfx/bup 流程）：
//   - preupload 返回的 auth 为原始字符串，整个作为 X-Upos-Auth 请求头；
//   - 上传地址 = endpoint + upos_uri 去掉 upos:// 前缀（新版 upos_uri 不含主机名）；
//   - init 为 POST（uploads&filesize&partsize&biz_id），分片为 PUT（partNumber/chunk/
//     chunks/size/start/end/total），合片为 POST（submit=add，body 为 parts 数组）。
//
// 分片用 ReadAt 按偏移读入固定缓冲，每个分片只占一块 chunkSize 内存。
// onProgress 每传完一个分片回调一次（可空）。
// 返回 upos 分配的视频文件名（不含扩展名），提交稿件时透传给 add/v3。
func (c *Client) UploadVideo(ctx context.Context, path string, onProgress func(done, total int64)) (UploadResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return UploadResult{}, fmt.Errorf("打开待上传文件失败: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return UploadResult{}, fmt.Errorf("读取文件信息失败: %w", err)
	}
	if info.Size() == 0 {
		return UploadResult{}, fmt.Errorf("待上传文件为空: %s", path)
	}

	pre, err := c.preupload(ctx, path, info.Size())
	if err != nil {
		return UploadResult{}, fmt.Errorf("预上传失败: %w", err)
	}
	base, objPath, err := uposTarget(pre)
	if err != nil {
		return UploadResult{}, err
	}
	chunkSize := pre.ChunkSizeOr(fallbackChunkSize)
	if chunkSize > 64<<20 {
		chunkSize = fallbackChunkSize
	}

	uploadID, err := c.uposInit(ctx, base, objPath, pre, info.Size(), chunkSize)
	if err != nil {
		return UploadResult{}, fmt.Errorf("upos 初始化失败: %w", err)
	}

	totalChunks := (info.Size() + int64(chunkSize) - 1) / int64(chunkSize)
	parts := make([]string, 0, totalChunks)
	done := int64(0)
	for off := int64(0); off < info.Size(); off += int64(chunkSize) {
		select {
		case <-ctx.Done():
			return UploadResult{}, ctx.Err()
		default:
		}
		partSize := info.Size() - off
		if partSize > int64(chunkSize) {
			partSize = int64(chunkSize)
		}
		buf := make([]byte, partSize)
		if _, rerr := f.ReadAt(buf, off); rerr != nil && rerr != io.EOF {
			return UploadResult{}, fmt.Errorf("读取分片(offset=%d)失败: %w", off, rerr)
		}
		if perr := c.uposPutChunk(ctx, base, objPath, uploadID, pre.Auth,
			len(parts)+1, int(off/int64(chunkSize)), off, int(totalChunks), info.Size(), buf); perr != nil {
			return UploadResult{}, fmt.Errorf("分片上传失败(offset=%d): %w", off, perr)
		}
		// upos 分片成功响应为固定文本（MULTIPART_PUT_SUCCESS），合片 body 的
		// eTag 按业界通行实现填固定字符串（与 biliup-rs 一致）。
		parts = append(parts, "etag")
		done += partSize
		if onProgress != nil {
			onProgress(done, info.Size())
		}
	}
	if err := c.uposComplete(ctx, base, objPath, pre, uploadID, parts); err != nil {
		return UploadResult{}, fmt.Errorf("upos 合片失败: %w", err)
	}
	name := pre.VideoName()
	if name == "" {
		return UploadResult{}, fmt.Errorf("预上传未返回视频文件名")
	}
	return UploadResult{Filename: name, BizID: pre.BizID}, nil
}

// preuploadResp 是预上传接口我们关心的字段子集。
// auth 为原始凭证字符串（旧版线路返回过 JSON 形态，upostAuth 兼容两种）。
type preuploadResp struct {
	OK           int    `json:"OK"`
	Ok           int    `json:"ok"`
	BiliFilename string `json:"bili_filename"`
	UposURI      string `json:"upos_uri"`
	Endpoint     string `json:"endpoint"`
	UploadURL    string `json:"upload_url"`
	BizID        int64  `json:"biz_id"`
	ChunkSize    int    `json:"chunk_size"`
	Auth         string `json:"auth"`
}

// okCheck 兼容大小写两种 OK 键。
func (p preuploadResp) okCheck() bool { return p.OK == 1 || p.Ok == 1 }

// ChunkSizeOr 非法值回落默认。
func (p preuploadResp) ChunkSizeOr(def int) int {
	if p.ChunkSize <= 0 {
		return def
	}
	return p.ChunkSize
}

// VideoName 返回提交稿件用的视频文件名（不含扩展名）。
// 新版流程从 upos_uri 取（upos://bucket/xxx.mp4 → xxx）；旧版回退 bili_filename。
func (p preuploadResp) VideoName() string {
	name := p.BiliFilename
	if p.UposURI != "" {
		name = baseName(strings.TrimPrefix(p.UposURI, "upos://"))
	}
	if idx := strings.LastIndexByte(name, '.'); idx > 0 {
		name = name[:idx]
	}
	return name
}

// readFileLimited 读取文件内容，超过 limit 直接报错（封面不需要大图）。
func readFileLimited(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("封面文件过大（%d 字节，上限 %d）", info.Size(), limit)
	}
	return os.ReadFile(path)
}

// baseName 取路径最后的文件名部分（兼容 / \ 两种分隔符）。
func baseName(p string) string {
	if idx := strings.LastIndexAny(p, `/\`); idx >= 0 {
		return p[idx+1:]
	}
	return p
}

// uposAuthString 取作 X-Upos-Auth 的凭证：新流程 auth 即原始字符串；
// 兼容旧版内嵌 JSON {"token":...} 的响应。
func (p preuploadResp) uposAuthString() string {
	raw := strings.TrimSpace(p.Auth)
	if strings.HasPrefix(raw, "{") {
		var a struct {
			Token string `json:"token"`
		}
		if json.Unmarshal([]byte(raw), &a) == nil && a.Token != "" {
			return a.Token
		}
	}
	return raw
}

// uposTarget 由 preupload 响应拼出上传基地址与对象路径。
// 标准形态：endpoint=//upos-cs-upcdntxa.bilivideo.com，upos_uri=upos://bucket/file.mkv；
// 兼容 upos_uri 自带主机（upos://host/path）或 upload_url 兜底的旧响应。
func uposTarget(pre preuploadResp) (base, path string, err error) {
	uri := strings.TrimSpace(pre.UposURI)
	if uri == "" {
		uri = strings.TrimSpace(pre.UploadURL)
	}
	if uri == "" {
		return "", "", fmt.Errorf("预上传响应缺少上传目标（upos_uri 与 upload_url 均为空）")
	}
	uri = strings.TrimPrefix(uri, "upos://")

	// endpoint 优先：//host 或 https://host（显式 http:// 仅本地 mock 用）
	ep := strings.TrimSpace(pre.Endpoint)
	if ep == "" {
		ep = strings.TrimSpace(pre.UploadURL)
	}
	scheme := "https"
	if strings.HasPrefix(ep, "http://") {
		scheme = "http"
	}
	ep = strings.TrimPrefix(ep, "https://")
	ep = strings.TrimPrefix(ep, "http://")
	ep = strings.TrimPrefix(ep, "//")
	ep = strings.TrimSuffix(ep, "/")

	if ep != "" {
		if !strings.HasPrefix(uri, "/") {
			uri = "/" + uri
		}
		return scheme + "://" + ep, uri, nil
	}
	// 旧形态：uri 自带主机（upos://host/path）
	idx := strings.IndexByte(uri, '/')
	if idx <= 0 {
		return "", "", fmt.Errorf("upos 目标格式异常: %q", uri)
	}
	return "https://" + uri[:idx], uri[idx:], nil
}

// preupload 申请上传凭证。profile=ugcfx/bup 为必填（缺失会被 400 parse failed 拒掉）。
func (c *Client) preupload(ctx context.Context, path string, size int64) (preuploadResp, error) {
	q := url.Values{}
	q.Set("name", baseName(path))
	q.Set("size", fmt.Sprintf("%d", size))
	q.Set("r", "upos")
	q.Set("os", "upos")
	q.Set("ssl", "0")
	q.Set("upcdn", "bda2")
	q.Set("profile", "ugcfx/bup")
	q.Set("probe_version", "20221109")
	q.Set("version", "2.14.0.0")
	q.Set("build", "2140000")

	req, err := c.newRequest(ctx, http.MethodGet, c.memberBase+"/preupload?"+q.Encode(), nil)
	if err != nil {
		return preuploadResp{}, err
	}
	var out preuploadResp
	if err := c.doFlatJSON(ctx, "/preupload", req, &out); err != nil {
		return preuploadResp{}, err
	}
	if !out.okCheck() || out.UposURI == "" || out.Auth == "" {
		return preuploadResp{}, fmt.Errorf("响应异常: ok=%d upos_uri=%q auth=%q",
			out.OK|out.Ok, out.UposURI, tailBody([]byte(out.Auth)))
	}
	return out, nil
}

// uposInit 初始化分片会话（POST），返回 uploadId。
// 响应形如 {"OK":1,"upload_id":"...","bucket":"...","key":"..."}。
func (c *Client) uposInit(ctx context.Context, base, path string, pre preuploadResp, size int64, chunkSize int) (string, error) {
	q := url.Values{}
	q.Set("uploads", "")
	q.Set("output", "json")
	q.Set("profile", "ugcfx/bup")
	q.Set("filesize", fmt.Sprintf("%d", size))
	q.Set("partsize", fmt.Sprintf("%d", chunkSize))
	q.Set("biz_id", fmt.Sprintf("%d", pre.BizID))

	req, err := c.newRequest(ctx, http.MethodPost, base+path+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Upos-Auth", pre.uposAuthString())

	var out struct {
		OK        int    `json:"OK"`
		UploadID  string `json:"upload_id"`
		UploadID2 string `json:"uploadid"`
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d | %s", resp.StatusCode, tailBody(raw))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("响应解析失败: %w | %s", err, tailBody(raw))
	}
	id := out.UploadID
	if id == "" {
		id = out.UploadID2
	}
	if out.OK != 1 || id == "" {
		return "", fmt.Errorf("响应异常: %s", tailBody(raw))
	}
	return id, nil
}

// uposPutChunk 分片上传（PUT）。partNumber 从 1 计，chunkIndex 从 0 计，
// start/end 为该分块的起止偏移，total 为文件总大小。
func (c *Client) uposPutChunk(ctx context.Context, base, path, uploadID, auth string,
	partNumber, chunkIndex int, offset int64, totalChunks int, totalSize int64, chunk []byte) error {

	q := url.Values{}
	q.Set("partNumber", fmt.Sprintf("%d", partNumber))
	q.Set("uploadId", uploadID)
	q.Set("chunk", fmt.Sprintf("%d", chunkIndex))
	q.Set("chunks", fmt.Sprintf("%d", totalChunks))
	q.Set("size", fmt.Sprintf("%d", len(chunk)))
	q.Set("start", fmt.Sprintf("%d", offset))
	q.Set("end", fmt.Sprintf("%d", offset+int64(len(chunk))))
	q.Set("total", fmt.Sprintf("%d", totalSize))

	req, err := c.newRequest(ctx, http.MethodPut, base+path+"?"+q.Encode(), strings.NewReader(string(chunk)))
	if err != nil {
		return err
	}
	req.Header.Set("X-Upos-Auth", auth)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(chunk))

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d | %s", resp.StatusCode, tailBody(raw))
	}
	return nil
}

// uposComplete 合片并落库（POST，参数走 query，与 biliup-rs 一致）。
// 请求体 {"parts":[{"partNumber":n,"eTag":"etag"}...]}；成功响应 {"OK":1,...}。
func (c *Client) uposComplete(ctx context.Context, base, path string, pre preuploadResp, uploadID string, parts []string) error {
	q := url.Values{}
	q.Set("output", "json")
	q.Set("name", baseName(pre.VideoName()))
	q.Set("profile", "ugcfx/bup")
	q.Set("submit", "add")
	q.Set("uploadId", uploadID)
	q.Set("biz_id", fmt.Sprintf("%d", pre.BizID))

	body := make([]map[string]interface{}, len(parts))
	for i := range parts {
		body[i] = map[string]interface{}{"partNumber": i + 1, "eTag": parts[i]}
	}
	payload, err := json.Marshal(map[string]interface{}{"parts": body})
	if err != nil {
		return err
	}

	req, err := c.newRequest(ctx, http.MethodPost, base+path+"?"+q.Encode(), strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("X-Upos-Auth", pre.uposAuthString())
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d | %s", resp.StatusCode, tailBody(raw))
	}
	var out struct {
		OK int `json:"OK"`
	}
	if json.Unmarshal(raw, &out) == nil && out.OK == 1 {
		return nil
	}
	return fmt.Errorf("响应异常: %s", tailBody(raw))
}
