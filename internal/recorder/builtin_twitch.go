package recorder

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type TwitchBuiltinPlatform struct{}

// GetPlatformName 提供用于逻辑判断及配置索引的 Twitch 平台名标识
func (t *TwitchBuiltinPlatform) GetPlatformName() string { return "Twitch" }

// Twitch 网页端公开 Client-ID 与 PlaybackAccessToken 持久化查询哈希（社区通用，
// 与 streamlink 等工具一致）；GQL 匿名可查直播状态，取流凭证需走 usher 签发。
const (
	twitchGQLEndpoint   = "https://gql.twitch.tv/gql"
	twitchGQLClientID   = "kimne78kx3ncx6brok4sw95dk2yxoosjw"
	twitchPlaybackHash  = "0828119ded1c13477966434e15800ff57ddacf13ba1911c129dc2200705b0712"
	twitchUsherEndpoint = "https://usher.ttvnw.net/api/channel/hls/%s.m3u8"
	twitchUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

// twitchHTTPClient 独立 HTTP 客户端：遵守标准代理环境变量（HTTPS_PROXY 等），
// 便于国内部署经代理访问 Twitch；不影响其它平台的直连请求。
var twitchHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// GetStreamURL 解析 Twitch 频道：GQL 查开播状态 → 持久化查询拿播放凭证 →
// usher 签发主 m3u8 → 按带宽挑 h264 分档。未开播返回空 url 且 err 为 nil。
func (t *TwitchBuiltinPlatform) GetStreamURL(roomID string, quality string) (string, string, string, error) {
	login := strings.ToLower(strings.TrimSpace(roomID))
	if login == "" {
		return "", "", "", fmt.Errorf("Twitch 频道名为空")
	}
	// 误粘贴整链时兜底取最后一段
	if strings.Contains(login, "/") {
		parts := strings.Split(strings.Trim(login, "/"), "/")
		login = strings.ToLower(parts[len(parts)-1])
	}

	meta, err := t.fetchStreamMeta(login)
	if err != nil {
		return "", login, "", err
	}

	if meta.User == nil {
		return "", login, "", fmt.Errorf("Twitch 频道不存在: %s", login)
	}
	avatar := meta.User.ProfileImageURL
	anchorName := meta.User.DisplayName
	if anchorName == "" {
		anchorName = login
	}
	if meta.User.Stream == nil || meta.User.Stream.Type != "live" {
		return "", anchorName, avatar, nil
	}

	token, sig, err := t.fetchPlaybackToken(login)
	if err != nil {
		return "", anchorName, avatar, err
	}

	master, err := t.fetchMasterPlaylist(login, token, sig)
	if err != nil {
		return "", anchorName, avatar, err
	}
	if master == "" {
		// usher 404：探测瞬间恰逢下播，按未开播处理
		return "", anchorName, avatar, nil
	}

	variants := parseTwitchMasterPlaylist(master)
	if len(variants) == 0 {
		return "", anchorName, avatar, errors.New("主播放列表未解析到可用分档")
	}
	return pickTwitchVariant(variants, quality), anchorName, avatar, nil
}

// ---------------- GQL 查询 ----------------

type twitchStreamMeta struct {
	User *struct {
		Login           string `json:"login"`
		DisplayName     string `json:"displayName"`
		ProfileImageURL string `json:"profileImageURL"`
		Stream          *struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Title   string `json:"title"`
			Viewers int    `json:"viewersCount"`
		} `json:"stream"`
	} `json:"user"`
}

type twitchPlaybackToken struct {
	Data struct {
		StreamPlaybackAccessToken *struct {
			Value string `json:"value"`
			Sig   string `json:"sig"`
		} `json:"streamPlaybackAccessToken"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// twitchGQLPost 发送 GQL 请求；配置了登录态时附带 OAuth Authorization，
// 登录态失效（401）时自动降级匿名重试，公开频道不因过期 Token 中断监控
func (t *TwitchBuiltinPlatform) twitchGQLPost(payload interface{}) ([]byte, error) {
	auth := twitchAuthToken()
	body, err := t.gqlRequest(payload, auth)
	if err == nil {
		return body, nil
	}
	if auth != "" && strings.Contains(err.Error(), "GQL HTTP 401") {
		return t.gqlRequest(payload, "")
	}
	return nil, err
}

// gqlRequest 按给定登录态发送一次 GQL 请求
func (t *TwitchBuiltinPlatform) gqlRequest(payload interface{}, authToken string) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", twitchGQLEndpoint, strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Client-ID", twitchGQLClientID)
	req.Header.Set("User-Agent", twitchUserAgent)
	req.Header.Set("X-Device-Id", twitchDeviceID())
	if authToken != "" {
		req.Header.Set("Authorization", "OAuth "+authToken)
	}

	resp, err := twitchHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, errors.New("GQL 请求被拒绝(403)，可能需要更新 OAuth Token")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("GQL HTTP 401（登录态无效或已过期）")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GQL HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// fetchStreamMeta 原生 query 查频道与直播状态（匿名可用）
func (t *TwitchBuiltinPlatform) fetchStreamMeta(login string) (*twitchStreamMeta, error) {
	payload := map[string]interface{}{
		"query":     `query($login:String!){user(login:$login){login displayName profileImageURL(width:300) stream{id type title viewersCount}}}`,
		"variables": map[string]interface{}{"login": login},
	}
	body, err := t.twitchGQLPost(payload)
	if err != nil {
		return nil, fmt.Errorf("查询频道状态失败: %w", err)
	}

	var meta twitchStreamMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, fmt.Errorf("解析频道状态失败: %w", err)
	}
	return &meta, nil
}

// fetchPlaybackToken 持久化查询申请播放签名凭证
func (t *TwitchBuiltinPlatform) fetchPlaybackToken(login string) (string, string, error) {
	payload := []map[string]interface{}{{
		"operationName": "PlaybackAccessToken",
		"variables": map[string]interface{}{
			"isLive":     true,
			"login":      login,
			"isVod":      false,
			"vodID":      "",
			"playerType": "site",
			"platform":   "web",
		},
		"extensions": map[string]interface{}{
			"persistedQuery": map[string]interface{}{
				"version":    1,
				"sha256Hash": twitchPlaybackHash,
			},
		},
	}}
	body, err := t.twitchGQLPost(payload)
	if err != nil {
		return "", "", fmt.Errorf("申请播放凭证失败: %w", err)
	}

	var token twitchPlaybackToken
	if err := json.Unmarshal(body, &token); err != nil {
		return "", "", fmt.Errorf("解析播放凭证失败: %w", err)
	}
	if len(token.Errors) > 0 {
		return "", "", fmt.Errorf("播放凭证查询报错: %s", token.Errors[0].Message)
	}
	if token.Data.StreamPlaybackAccessToken == nil {
		return "", "", errors.New("未取到播放凭证（限制级频道需配置 OAuth Token）")
	}
	return token.Data.StreamPlaybackAccessToken.Value, token.Data.StreamPlaybackAccessToken.Sig, nil
}

// fetchMasterPlaylist 请求 usher 签发主 m3u8；404 视为已下播
func (t *TwitchBuiltinPlatform) fetchMasterPlaylist(login, token, sig string) (string, error) {
	params := url.Values{}
	params.Set("sig", sig)
	params.Set("token", token)
	params.Set("allow_source", "true")
	params.Set("allow_audio_only", "true")
	params.Set("allow_spectre", "false")
	params.Set("p", strconv.Itoa(randIntn(1000000)))
	params.Set("platform", "web")
	params.Set("player_backend", "mediaplayer")
	params.Set("supported_codecs", "av1,h265,h264")
	params.Set("player", "twitchweb")
	params.Set("play_session_id", twitchSessionID())
	params.Set("cluster", "edge")

	req, err := http.NewRequest("GET", fmt.Sprintf(twitchUsherEndpoint, url.PathEscape(login))+"?"+params.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", twitchUserAgent)
	req.Header.Set("Referer", "https://player.twitch.tv/")

	resp, err := twitchHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusNotFound {
		// 通道已关闭：探测瞬间恰逢下播，按未开播处理
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("usher HTTP %d", resp.StatusCode)
	}
	return string(body), nil
}

// ---------------- 主播放列表解析 ----------------

// twitchVariant 主 m3u8 里的一个画质分档
type twitchVariant struct {
	Bandwidth int
	Codecs    string
	URL       string
}

// parseTwitchMasterPlaylist 逐行抽取 #EXT-X-STREAM-INF 属性与其后紧跟的分档地址
func parseTwitchMasterPlaylist(text string) []twitchVariant {
	variants := make([]twitchVariant, 0, 8)
	var pending *twitchVariant

	flush := func() {
		if pending != nil && pending.URL != "" {
			variants = append(variants, *pending)
		}
		pending = nil
	}

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			flush()
			v := &twitchVariant{}
			if m := twitchAttr(line, "BANDWIDTH"); m != "" {
				if bw, err := strconv.Atoi(m); err == nil {
					v.Bandwidth = bw
				}
			}
			v.Codecs = twitchAttr(line, "CODECS")
			pending = v
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if pending != nil {
			pending.URL = line
			flush()
		}
	}
	flush()
	return variants
}

// twitchAttr 从属性行里取 key=value（值可能带引号）
func twitchAttr(line, key string) string {
	idx := strings.Index(line, key+"=")
	if idx == -1 {
		return ""
	}
	rest := strings.TrimSpace(line[idx+len(key)+1:])
	if strings.HasPrefix(rest, "\"") {
		end := strings.Index(rest[1:], "\"")
		if end == -1 {
			return ""
		}
		return rest[1 : 1+end]
	}
	if end := strings.IndexAny(rest, ","); end != -1 {
		return rest[:end]
	}
	return rest
}

// pickTwitchVariant 按画质诉求选分档：优先 h264（mpegts 兼容最好），
// 带宽降序后 uhd 取最高、hd 取中位、sd 取最低
func pickTwitchVariant(variants []twitchVariant, quality string) string {
	usable := make([]twitchVariant, 0, len(variants))
	for _, v := range variants {
		if v.URL == "" {
			continue
		}
		usable = append(usable, v)
	}
	h264 := make([]twitchVariant, 0, len(usable))
	for _, v := range usable {
		if strings.Contains(v.Codecs, "avc1") {
			h264 = append(h264, v)
		}
	}
	if len(h264) > 0 {
		usable = h264
	}

	sort.SliceStable(usable, func(i, j int) bool { return usable[i].Bandwidth > usable[j].Bandwidth })

	index := 0
	switch quality {
	case "sd":
		index = len(usable) - 1
	case "hd":
		index = len(usable) / 2
	}
	return usable[index].URL
}

// ---------------- 鉴权与随机数 ----------------

// twitchAuthToken 读取用户配置的 Twitch 凭证，兼容三种填写方式：
// 整串浏览器 Cookie（自动提取 auth-token 字段）、裸 OAuth Token、
// 无效内容（返回空串走匿名，公开频道不受影响）。
func twitchAuthToken() string {
	builtinCookieMutex.RLock()
	var raw string
	if builtinCookies != nil {
		raw = builtinCookies.Twitch
	}
	builtinCookieMutex.RUnlock()

	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	// 整串 Cookie：提取 auth-token 字段（GQL Authorization 用的就是它）
	if idx := strings.Index(raw, "auth-token="); idx != -1 {
		rest := raw[idx+len("auth-token="):]
		if end := strings.IndexByte(rest, ';'); end != -1 {
			rest = rest[:end]
		}
		rest = strings.TrimSpace(rest)
		if dec, err := url.QueryUnescape(rest); err == nil {
			rest = dec
		}
		return rest
	}

	// 仍是 k=v;... 形态但没有 auth-token：无法提取登录态，宁缺毋滥走匿名
	if strings.Contains(raw, ";") || strings.Contains(raw, "=") {
		return ""
	}

	// 裸 OAuth Token
	return raw
}

// twitchProxyFromEnv 读取标准代理环境变量（HTTPS_PROXY → ALL_PROXY），
// 与 twitchHTTPClient 的 ProxyFromEnvironment 行为对齐，供 ffmpeg 走代理拉流
func twitchProxyFromEnv() string {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// twitchDeviceID 每进程生成一个稳定设备号，降低 GQL 匿名风控概率
var twitchDeviceIDOnce = sync.OnceValue(twitchRandomHex32)

func twitchDeviceID() string { return twitchDeviceIDOnce() }

// twitchSessionID 生成 usher 所需的会话 ID（uuid v4 形态）
func twitchSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// twitchRandomHex32 生成 32 位十六进制随机串
func twitchRandomHex32() string {
	const hex = "0123456789abcdef"
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "0123456789abcdef0123456789abcdef"
	}
	for i, v := range b {
		b[i] = hex[int(v)%16]
	}
	return string(b)
}

// randIntn 简易随机数（usher 的 p 参数仅为防缓存）
func randIntn(n int) int {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return 12345
	}
	v := int(b[0]) | int(b[1])<<8 | int(b[2])<<16 | int(b[3])<<24
	if v < 0 {
		v = -v
	}
	return v % n
}
