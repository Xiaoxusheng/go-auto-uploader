package recorder

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type BilibiliBuiltinPlatform struct{}

// GetPlatformName 提供用于逻辑判断及配置索引的 B 站平台名标识
func (b *BilibiliBuiltinPlatform) GetPlatformName() string { return "Bilibili" }

// GetStreamURL 解析 B 站直播间：走 xlive getRoomPlayInfo v2 接口拼装 FLV 直链
// （host + base_url + extra），画质按 accept_qn 就近匹配；服务端授予的档位低于
// 请求档位（匿名/风控限速）时以授予档为准。未开播返回空 url 且 err 为 nil。
func (b *BilibiliBuiltinPlatform) GetStreamURL(roomID string, quality string) (string, string, string, error) {
	roomID = bilibiliNormalizeRoomID(roomID)
	if roomID == "" {
		return "", "", "", fmt.Errorf("B站房间号为空")
	}

	cookie := bilibiliCookie()

	// 第一次请求 qn=0 只为拿真实房间号、uid、开播状态与可用画质列表
	info, err := b.fetchRoomPlayInfo(roomID, 0, cookie)
	if err != nil {
		return "", roomID, "", err
	}

	// Master/info 补齐主播名与头像（未开播时也返回，供前端展示；失败不致命）
	anchorName, avatar := b.fetchAnchorInfo(info.Data.UID, cookie)
	if info.Data.LiveStatus != 1 {
		// live_status: 0 未开播，2 轮播（录播轮放，不当作真实开播）
		return "", anchorName, avatar, nil
	}

	codec := pickBilibiliCodec(info.Data.PlayurlInfo.Playurl.Stream)
	if codec == nil {
		return "", anchorName, avatar, errors.New("已开播但未取到可用流（付费房间或需要登录 Cookie）")
	}

	if target := pickBilibiliQN(codec.AcceptQN, quality); target != 0 && target != codec.CurrentQN {
		// 当前档位与目标不符时二次请求指定画质；二次失败则退回已拿到的流
		if detail, derr := b.fetchRoomPlayInfo(fmt.Sprint(info.Data.RoomID), target, cookie); derr == nil && detail.Data.LiveStatus == 1 {
			if dc := pickBilibiliCodec(detail.Data.PlayurlInfo.Playurl.Stream); dc != nil {
				codec = dc
			}
		}
	}

	streamURL := buildBilibiliStreamURL(codec)
	if streamURL == "" {
		return "", anchorName, avatar, errors.New("已开播但流地址拼装失败")
	}
	return streamURL, anchorName, avatar, nil
}

// ---------------- 接口数据结构 ----------------

type bilibiliRoomInfo struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		RoomID      int64 `json:"room_id"`
		ShortID     int64 `json:"short_id"`
		UID         int64 `json:"uid"`
		LiveStatus  int   `json:"live_status"`
		PlayurlInfo struct {
			Playurl struct {
				Stream []bilibiliStream `json:"stream"`
			} `json:"playurl"`
		} `json:"playurl_info"`
	} `json:"data"`
}

type bilibiliStream struct {
	ProtocolName string                `json:"protocol_name"`
	Format       []bilibiliFormatGroup `json:"format"`
}

type bilibiliFormatGroup struct {
	FormatName string          `json:"format_name"`
	Codec      []bilibiliCodec `json:"codec"`
}

type bilibiliCodec struct {
	CodecName string `json:"codec_name"`
	CurrentQN int    `json:"current_qn"`
	AcceptQN  []int  `json:"accept_qn"`
	BaseURL   string `json:"base_url"`
	URLInfo   []struct {
		Host  string `json:"host"`
		Extra string `json:"extra"`
	} `json:"url_info"`
}

type bilibiliMasterInfo struct {
	Code int `json:"code"`
	Data struct {
		Info struct {
			UName string `json:"uname"`
			Face  string `json:"face"`
		} `json:"info"`
	} `json:"data"`
}

// ---------------- 接口请求 ----------------

// fetchRoomPlayInfo 请求 xlive getRoomPlayInfo；qn 传 0 时仅探测画质列表。
// code!=0（房间不存在/风控）按错误处理，live_status!=1 由调用方按未开播处理。
func (b *BilibiliBuiltinPlatform) fetchRoomPlayInfo(roomID string, qn int, cookie string) (*bilibiliRoomInfo, error) {
	apiURL := fmt.Sprintf(
		"https://api.live.bilibili.com/xlive/web-room/v2/index/getRoomPlayInfo?room_id=%s&protocol=0,1&format=0,1,2&codec=0,1&qn=%d&platform=web&ptype=8&dolby=5&panorama=1",
		url.QueryEscape(roomID), qn)

	body, err := bilibiliGet(apiURL, cookie)
	if err != nil {
		return nil, err
	}

	var info bilibiliRoomInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("解析 getRoomPlayInfo 失败: %w", err)
	}
	if info.Code != 0 {
		msg := info.Message
		if msg == "" {
			msg = "未知错误"
		}
		return nil, fmt.Errorf("getRoomPlayInfo code=%d (%s)", info.Code, msg)
	}
	if info.Data.RoomID == 0 {
		return nil, fmt.Errorf("房间不存在: %s", roomID)
	}
	return &info, nil
}

// fetchAnchorInfo 请求 live_user/v1/Master/info 拿主播名与头像，失败不致命
func (b *BilibiliBuiltinPlatform) fetchAnchorInfo(uid int64, cookie string) (string, string) {
	if uid == 0 {
		return "", ""
	}
	apiURL := fmt.Sprintf("https://api.live.bilibili.com/live_user/v1/Master/info?uid=%d", uid)
	body, err := bilibiliGet(apiURL, cookie)
	if err != nil {
		return "", ""
	}

	var resp bilibiliMasterInfo
	if err := json.Unmarshal(body, &resp); err != nil || resp.Code != 0 {
		return "", ""
	}

	name := resp.Data.Info.UName
	face := resp.Data.Info.Face
	if face != "" {
		face = "/api/v1/builtin_recorder/proxy_image?url=" + url.QueryEscape(face)
	}
	return name, face
}

// bilibiliGet 统一的 GET 请求：带直播页 Referer 与 UA，可选 Cookie
func bilibiliGet(apiURL string, cookie string) ([]byte, error) {
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://live.bilibili.com/")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, err := builtinHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("B站接口 HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ---------------- 房间号与短链 ----------------

// bilibiliNormalizeRoomID 剥离误粘贴的整链与查询串；非纯数字（b23.tv 分享码）
// 走跳转解析还原真实房间号
func bilibiliNormalizeRoomID(roomID string) string {
	roomID = strings.TrimSpace(roomID)
	if idx := strings.LastIndex(roomID, "/"); idx != -1 {
		roomID = roomID[idx+1:]
	}
	if idx := strings.IndexAny(roomID, "?#"); idx != -1 {
		roomID = roomID[:idx]
	}
	roomID = strings.TrimSpace(roomID)

	if !isAllDigits(roomID) {
		if resolved, err := resolveBilibiliShortCode(roomID); err == nil && resolved != "" {
			return resolved
		}
	}
	return roomID
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// resolveBilibiliShortCode 跟随 b23.tv 分享码跳转，从落地页 URL 提取房间号
func resolveBilibiliShortCode(code string) (string, error) {
	req, err := http.NewRequest("GET", "https://b23.tv/"+url.PathEscape(code), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")

	resp, err := builtinHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	final := ""
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	if final == "" {
		return "", errors.New("b23.tv 跳转未得到落地链接")
	}

	u, err := url.Parse(final)
	if err != nil || !strings.Contains(u.Host, "bilibili.com") {
		return "", fmt.Errorf("b23.tv 落地链接非 B 站直播页: %s", final)
	}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segments) == 0 || segments[len(segments)-1] == "" {
		return "", errors.New("b23.tv 落地链接未包含房间号")
	}
	return segments[len(segments)-1], nil
}

// ExtractBuiltinBilibiliShortURL 供添加入口解析 b23.tv 分享短链为直播间长链
func ExtractBuiltinBilibiliShortURL(line string) (string, error) {
	code := line
	if idx := strings.Index(code, "b23.tv/"); idx != -1 {
		code = code[idx+len("b23.tv/"):]
	}
	if idx := strings.IndexAny(code, "?#/ \t"); idx != -1 {
		code = code[:idx]
	}
	if code == "" {
		return "", errors.New("未找到 b23.tv 分享码")
	}

	roomID, err := resolveBilibiliShortCode(code)
	if err != nil {
		return "", err
	}
	return "https://live.bilibili.com/" + roomID, nil
}

// ---------------- 画质与流地址 ----------------

// pickBilibiliCodec 从 stream 列表里挑一路对 ffmpeg 最友好的编码：
// 优先 flv 封装 + avc (h264)，其次 flv 内任一编码，最后退回任意一档
func pickBilibiliCodec(streams []bilibiliStream) *bilibiliCodec {
	flv := -1
	avcInFlv := -1
	avcAny := -1
	firstAny := -1

	for si, s := range streams {
		for fi, f := range s.Format {
			for ci, c := range f.Codec {
				if firstAny == -1 {
					firstAny = encodeStreamIndex(si, fi, ci)
				}
				if c.CodecName == "avc" && avcAny == -1 {
					avcAny = encodeStreamIndex(si, fi, ci)
				}
				if f.FormatName == "flv" && flv == -1 {
					flv = encodeStreamIndex(si, fi, ci)
				}
				if f.FormatName == "flv" && c.CodecName == "avc" && avcInFlv == -1 {
					avcInFlv = encodeStreamIndex(si, fi, ci)
				}
			}
		}
	}

	for _, idx := range []int{avcInFlv, flv, avcAny, firstAny} {
		if idx != -1 {
			if c := lookupBilibiliCodec(streams, idx); c != nil {
				return c
			}
		}
	}
	return nil
}

// encodeStreamIndex 把 (stream, format, codec) 三维下标压平为一维，便于统一挑选
func encodeStreamIndex(si, fi, ci int) int { return (si*16+fi)*16 + ci }

func lookupBilibiliCodec(streams []bilibiliStream, idx int) *bilibiliCodec {
	ci := idx % 16
	fi := (idx / 16) % 16
	si := idx / 256
	if si >= len(streams) {
		return nil
	}
	f := streams[si].Format
	if fi >= len(f) {
		return nil
	}
	c := f[fi].Codec
	if ci >= len(c) {
		return nil
	}
	return &c[ci]
}

// buildBilibiliStreamURL 按官方播放器规则拼装直链：host + base_url (+ "?" + extra)。
// base_url 常自带尾部 "?"，此时直接追加 extra。
func buildBilibiliStreamURL(codec *bilibiliCodec) string {
	if codec == nil || codec.BaseURL == "" || len(codec.URLInfo) == 0 {
		return ""
	}
	for _, u := range codec.URLInfo {
		if u.Host == "" {
			continue
		}
		host := u.Host
		if idx := strings.Index(host, "//"); idx != -1 && !strings.HasPrefix(host, "https://") {
			host = "https://" + host[idx+2:]
		} else if !strings.Contains(host, "//") {
			host = "https://" + strings.TrimPrefix(host, "https:")
		}
		base := codec.BaseURL
		if strings.HasSuffix(base, "?") {
			return host + base + u.Extra
		}
		return host + base + "?" + u.Extra
	}
	return ""
}

// pickBilibiliQN 从 accept_qn 里按画质诉求选档。
// 目标档位（uhd→原画 10000，hd→超清 250，sd→高清 150）不存在时按
// 高→低排序后取首/中/尾就近降档，与快手分档逻辑对齐。
func pickBilibiliQN(accept []int, quality string) int {
	qns := make([]int, 0, len(accept))
	for _, q := range accept {
		if q > 0 {
			qns = append(qns, q)
		}
	}
	if len(qns) == 0 {
		return 0
	}
	sort.Slice(qns, func(i, j int) bool { return qns[i] > qns[j] })

	target := map[string]int{"uhd": 10000, "hd": 250, "sd": 150}[quality]
	for _, q := range qns {
		if q == target {
			return q
		}
	}

	index := 0
	switch quality {
	case "sd":
		index = len(qns) - 1
	case "hd":
		index = len(qns) / 2
	}
	return qns[index]
}

// bilibiliCookie 读取用户配置的 B 站 Cookie（含 SESSDATA，用于解锁原画/风控）
func bilibiliCookie() string {
	builtinCookieMutex.RLock()
	defer builtinCookieMutex.RUnlock()
	if builtinCookies != nil {
		return builtinCookies.Bilibili
	}
	return ""
}
