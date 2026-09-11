package recorder

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type KuaishouBuiltinPlatform struct{}

// GetPlatformName 提供用于逻辑判断及配置索引的快手平台名标识
func (k *KuaishouBuiltinPlatform) GetPlatformName() string { return "Kuaishou" }

// ---------------- 会话与限速 ----------------
//
// 快手对匿名请求按设备号(did)+IP 做频控：旧实现把 did 写死在代码里，
// 所有部署共用一个设备号，早被拉黑（"Your action is too frequent"）。
// 这里改为每个进程启动时生成一个随机设备号，用户配置的 Cookie 仍优先。

var (
	kuaishouDeviceOnce sync.Once
	kuaishouDeviceID   string

	kuaishouFetchMu   sync.Mutex
	kuaishouLastFetch time.Time

	// 风控自适应退避：快手 SSR 数据接口对 IP+会话做频控，
	// 固定 30 秒轮询会把限频窗口一直打关。连续命中风控时全局冷却逐次翻倍（30s→300s 封顶），
	// 任一次成功解析（含正常未开播）即复位。
	kuaishouRiskMu        sync.Mutex
	kuaishouRiskStreak    int
	kuaishouRiskHoldUntil time.Time
)

const (
	kuaishouRiskBaseHold = 30 * time.Second
	kuaishouRiskMaxHold  = 5 * time.Minute
)

func kuaishouNoteRisk() {
	kuaishouRiskMu.Lock()
	defer kuaishouRiskMu.Unlock()
	kuaishouRiskStreak++
	hold := kuaishouRiskBaseHold * time.Duration(kuaishouRiskStreak)
	if hold > kuaishouRiskMaxHold {
		hold = kuaishouRiskMaxHold
	}
	kuaishouRiskHoldUntil = time.Now().Add(hold)
}

func kuaishouNoteOK() {
	kuaishouRiskMu.Lock()
	defer kuaishouRiskMu.Unlock()
	kuaishouRiskStreak = 0
	kuaishouRiskHoldUntil = time.Time{}
}

// kuaishouMinFetchGap 相邻两次快手探测的最小间隔（跨任务全局），
// 避免多个房间同时轮询时把同一出口 IP 打进风控。
const kuaishouMinFetchGap = 2 * time.Second

func kuaishouGetDeviceID() string {
	kuaishouDeviceOnce.Do(func() {
		const hex = "0123456789abcdef"
		b := make([]byte, 32)
		now := time.Now().UnixNano()
		for i := range b {
			now = now*6364136223846793005 + 1442695040888963407
			b[i] = hex[(uint64(now)>>33)%16]
		}
		kuaishouDeviceID = "web_" + string(b)
	})
	return kuaishouDeviceID
}

func kuaishouCookie() string {
	builtinCookieMutex.RLock()
	defer builtinCookieMutex.RUnlock()
	if builtinCookies != nil && builtinCookies.Kuaishou != "" {
		return builtinCookies.Kuaishou
	}
	return "did=" + kuaishouGetDeviceID() + "; didv=" + fmt.Sprint(time.Now().UnixMilli()) + "; kpn=KUAISHOU"
}

// kuaishouThrottle 全局串行化并保证最小间隔，返回本次请求应使用的 cookie
func kuaishouThrottle() {
	kuaishouFetchMu.Lock()
	defer kuaishouFetchMu.Unlock()
	if wait := kuaishouMinFetchGap - time.Since(kuaishouLastFetch); wait > 0 {
		time.Sleep(wait)
	}
	kuaishouLastFetch = time.Now()

	// 风控冷却期：全局挂起（不占锁语义问题——这里持锁睡，天然串行所有快手探测）
	kuaishouRiskMu.Lock()
	hold := kuaishouRiskHoldUntil
	kuaishouRiskMu.Unlock()
	if wait := time.Until(hold); wait > 0 {
		time.Sleep(wait)
	}
}

// kuaishouNormalizeRoomID 兼容误粘贴的整链与带 .html 后缀的房间 ID
func kuaishouNormalizeRoomID(roomID string) string {
	roomID = strings.TrimSpace(roomID)
	if idx := strings.LastIndex(roomID, "/"); idx != -1 {
		roomID = roomID[idx+1:]
	}
	roomID = strings.TrimSuffix(strings.TrimSuffix(roomID, ".html"), ".htm")
	return strings.TrimSpace(roomID)
}

// ---------------- 平台入口 ----------------

// GetStreamURL 解析快手直播间：主路径走 PC 网页 SSR（开播时服务端直接注入
// liveStream 数据，无需签名），被风控/网络失败时回退 App 分享 H5 接口。
// 返回值约定与其它平台一致：url 为空且 err 为 nil 表示未开播。
func (k *KuaishouBuiltinPlatform) GetStreamURL(roomID string, quality string) (string, string, string, error) {
	roomID = kuaishouNormalizeRoomID(roomID)
	if roomID == "" {
		return "", "", "", fmt.Errorf("快手房间号为空")
	}

	pageURL, pageName, pageAvatar, pageErr := k.fetchViaWebPage(roomID, quality)
	if pageErr == nil {
		return pageURL, pageName, pageAvatar, nil
	}

	apiURL, apiName, apiAvatar, apiErr := k.fetchViaByUserAPI(roomID, quality)
	if apiErr == nil {
		return apiURL, apiName, apiAvatar, nil
	}

	return "", pageName, pageAvatar, fmt.Errorf("快手网页端与移动端均解析失败: 网页端(%v) / 分享接口(%v)", pageErr, apiErr)
}

// ---------------- 主路径：PC 网页 SSR ----------------

type ksWebState struct {
	Liveroom struct {
		PlayList []ksPlayItem `json:"playList"`
	} `json:"liveroom"`
}

type ksPlayItem struct {
	IsLiving   bool         `json:"isLiving"`
	LiveStream ksLiveStream `json:"liveStream"`
	Author     struct {
		Name    string `json:"name"`
		HeadURL string `json:"headUrl"`
	} `json:"author"`
	ErrorType *struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	} `json:"errorType"`
}

type ksLiveStream struct {
	Living   bool            `json:"living"`
	Poster   string          `json:"poster"`
	PlayUrls json.RawMessage `json:"playUrls"`
}

type ksAdaptationSet struct {
	AdaptationSet struct {
		Representation []ksRepresentation `json:"representation"`
	} `json:"adaptationSet"`
}

type ksRepresentation struct {
	QualityLevel int    `json:"qualityLevel"`
	QualityType  string `json:"qualityType"`
	QualityLabel string `json:"qualityLabel"`
	Status       int    `json:"status"`
	Bitrate      int    `json:"bitrate"`
	AvgBitrate   int    `json:"avgBitrate"`
	URL          string `json:"url"`
	BackupURL    string `json:"backupUrl"`
}

// fetchViaWebPage 拉取 PC 直播间页并解析 __INITIAL_STATE__。
// 页面仅在开播时由服务端注入完整 liveStream；未开播返回空 url 且不报错；
// 服务端限频会落在 errorType 里（"请求过快，请稍后重试"），必须当作瞬时错误而非未开播。
func (k *KuaishouBuiltinPlatform) fetchViaWebPage(roomID string, quality string) (string, string, string, error) {
	kuaishouThrottle()

	req, err := http.NewRequest("GET", "https://live.kuaishou.com/u/"+url.PathEscape(roomID), nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.8,zh-TW;q=0.7,zh-HK;q=0.5,en-US;q=0.3,en;q=0.2")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	// SSR 数据接口校验请求指纹：缺 Sec-Fetch 系列头会直接落风控（"请求过快"）
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Referer", "https://live.kuaishou.com/")
	req.Header.Set("Cookie", kuaishouCookie())

	resp, err := builtinHTTPClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", err
	}

	state, err := parseKuaishouPageState(string(body))
	if err != nil {
		return "", "", "", err
	}
	// 空列表说明 SSR 未注入房间数据（限频抖动或房间不存在），按瞬时失败处理，
	// 不能当未开播——否则真在直播时会被静默跳过
	if len(state.Liveroom.PlayList) == 0 {
		kuaishouNoteRisk()
		return "", "", "", fmt.Errorf("页面未返回直播间数据，可能被限频或房间不存在")
	}

	anchorName := roomID
	avatar := ""

	for _, item := range state.Liveroom.PlayList {
		if item.ErrorType != nil {
			kuaishouNoteRisk()
			return "", anchorName, avatar, fmt.Errorf("快手页面数据被风控拦截: %s", item.ErrorType.Title)
		}
		kuaishouNoteOK()
		if item.LiveStream.Poster != "" {
			avatar = item.LiveStream.Poster
		} else if item.Author.HeadURL != "" {
			avatar = item.Author.HeadURL
		}
		if item.Author.Name != "" {
			anchorName = item.Author.Name
		}
		if !item.IsLiving && !item.LiveStream.Living {
			continue
		}
		streamURL := kuaishouPickRepresentation(parseKuaishouPlayUrls(item.LiveStream.PlayUrls), quality)
		if streamURL == "" {
			return "", anchorName, avatar, fmt.Errorf("页面显示开播中但未取到可用流地址")
		}
		if avatar != "" {
			avatar = wrapKuaishouImageProxy(avatar)
		}
		return streamURL, anchorName, avatar, nil
	}

	if avatar != "" {
		avatar = wrapKuaishouImageProxy(avatar)
	}
	return "", anchorName, avatar, nil
}

// parseKuaishouPageState 从页面 HTML 中定位并解析 __INITIAL_STATE__ JSON。
// 用括号配对而非正则截取，避免 JSON 字符串里出现 ";(function" 之类的截断陷阱。
func parseKuaishouPageState(html string) (*ksWebState, error) {
	const marker = "window.__INITIAL_STATE__="
	idx := strings.Index(html, marker)
	if idx == -1 {
		return nil, fmt.Errorf("页面未包含 __INITIAL_STATE__，可能被验证码拦截")
	}
	braceStart := strings.IndexByte(html[idx:], '{')
	if braceStart == -1 {
		return nil, fmt.Errorf("__INITIAL_STATE__ 格式异常")
	}
	raw, ok := extractJSONObject(html[idx+braceStart:])
	if !ok {
		return nil, fmt.Errorf("__INITIAL_STATE__ JSON 括号不配对")
	}

	var state ksWebState
	if err := json.Unmarshal([]byte(sanitizeJSLiterals(raw)), &state); err != nil {
		return nil, fmt.Errorf("解析 __INITIAL_STATE__ 失败: %w", err)
	}
	return &state, nil
}

// sanitizeJSLiterals 快手页面偶发在状态对象里输出 JS 字面量（如 "authToken":undefined），
// 严格 JSON 不接受。这里把字符串字面量以外的 undefined / NaN / Infinity 统一替换为 null，
// 字符串内部的同名单词不受影响。
func sanitizeJSLiterals(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr, esc := false, false
	for i := 0; i < len(s); {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			i++
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			i++
			continue
		}
		if c == '-' && strings.HasPrefix(s[i:], "-Infinity") {
			b.WriteString("null")
			i += len("-Infinity")
			continue
		}
		if (c == 'u' || c == 'N' || c == 'I') && jsTokenBoundary(s, i) {
			rest := s[i:]
			switch {
			case strings.HasPrefix(rest, "undefined"):
				b.WriteString("null")
				i += len("undefined")
				continue
			case strings.HasPrefix(rest, "NaN"):
				b.WriteString("null")
				i += len("NaN")
				continue
			case strings.HasPrefix(rest, "Infinity"):
				b.WriteString("null")
				i += len("Infinity")
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// jsTokenBoundary 判定 s[i:] 是否处于 JSON 值的合法起始位（前一个非空白字节是容器或冒号/逗号）
func jsTokenBoundary(s string, i int) bool {
	for j := i - 1; j >= 0; j-- {
		switch s[j] {
		case ' ', '\t', '\n', '\r':
			continue
		case '{', '[', ':', ',':
			return true
		default:
			return false
		}
	}
	return false
}

// extractJSONObject 从 s 开头的 '{' 起做括号配对，返回完整 JSON 对象文本
func extractJSONObject(s string) (string, bool) {
	if s == "" || s[0] != '{' {
		return "", false
	}
	depth, inStr, esc := 0, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1], true
			}
		}
	}
	return "", false
}

// parseKuaishouPlayUrls 兼容两种历史结构：
// 新版 {"h264":{"adaptationSet":{"representation":[...]}}}，旧版 [{"adaptationSet":{...}}]
func parseKuaishouPlayUrls(raw json.RawMessage) []ksRepresentation {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	adaptationFrom := func(node json.RawMessage) []ksRepresentation {
		var wrap ksAdaptationSet
		if err := json.Unmarshal(node, &wrap); err != nil {
			return nil
		}
		return wrap.AdaptationSet.Representation
	}

	var dict struct {
		H264 json.RawMessage `json:"h264"`
		H265 json.RawMessage `json:"h265"`
	}
	if err := json.Unmarshal(raw, &dict); err == nil {
		// 字典结构：优先 h264（TS 封装兼容性最好），仅 h265 时也兜住
		for _, node := range []json.RawMessage{dict.H264, dict.H265} {
			if len(node) == 0 {
				continue
			}
			if reps := adaptationFrom(node); len(reps) > 0 {
				return reps
			}
		}
		return nil
	}

	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	var reps []ksRepresentation
	for _, node := range list {
		reps = append(reps, adaptationFrom(node)...)
	}
	return reps
}

// kuaishouPickRepresentation 按画质诉求从清晰度列表中选一路可用地址。
// 有码率按码率排（高→低），否则按 qualityLevel 排（小号=更清晰），
// 都缺失时信任接口给出的原始顺序（首位=最优）。取不到目标档位就就近降档。
func kuaishouPickRepresentation(reps []ksRepresentation, quality string) string {
	usable := make([]ksRepresentation, 0, len(reps))
	for _, r := range reps {
		if r.URL == "" && r.BackupURL == "" {
			continue
		}
		usable = append(usable, r)
	}
	if len(usable) == 0 {
		return ""
	}

	sort.SliceStable(usable, func(i, j int) bool {
		bi, bj := usable[i].rank(), usable[j].rank()
		if bi != bj {
			return bi > bj
		}
		return usable[i].order() < usable[j].order()
	})

	index := 0
	switch quality {
	case "sd":
		index = len(usable) - 1
	case "hd":
		index = len(usable) / 2
	}

	if r := usable[index]; r.URL != "" {
		return r.URL
	}
	return usable[index].BackupURL
}

// rank 清晰度权重：码率优先，其次 qualityLevel 取倒数（1=蓝光 最高）
func (r ksRepresentation) rank() int {
	if r.Bitrate > 0 {
		return r.Bitrate
	}
	if r.AvgBitrate > 0 {
		return r.AvgBitrate
	}
	if r.QualityLevel > 0 && r.QualityLevel <= 20 {
		return 1 << (24 - r.QualityLevel)
	}
	return 0
}

// order 同权重时的稳定序：接口原序（先出现者优先）
func (r ksRepresentation) order() int {
	if r.QualityLevel > 0 {
		return r.QualityLevel
	}
	return 0
}

// ---------------- 回退：App 分享 H5 接口 ----------------

type ksByUserResp struct {
	Result     int    `json:"result"`
	ErrorMsg   string `json:"error_msg"`
	LiveStream struct {
		Living bool `json:"living"`
		User   struct {
			UserName string `json:"user_name"`
			HeadURL  string `json:"headUrl"`
		} `json:"user"`
		HLSPlayURL                 string              `json:"hlsPlayUrl"`
		PlayURL                    string              `json:"playUrl"`
		MultiResolutionPlayUrls    []ksResolutionGroup `json:"multiResolutionPlayUrls"`
		MultiResolutionHlsPlayUrls []ksResolutionGroup `json:"multiResolutionHlsPlayUrls"`
	} `json:"liveStream"`
}

type ksResolutionGroup struct {
	URLs []struct {
		URL string `json:"url"`
	} `json:"urls"`
}

func (g ksResolutionGroup) first() string {
	if len(g.URLs) > 0 {
		return g.URLs[0].URL
	}
	return ""
}

// fetchViaByUserAPI 走 App 分享落地页同款接口，兜住网页端 SSR 抖动。
// 该接口风控较严：成功与否不保证，失败即返回错误交由上层汇总。
func (k *KuaishouBuiltinPlatform) fetchViaByUserAPI(roomID string, quality string) (string, string, string, error) {
	kuaishouThrottle()

	payload := `{"source":5,"eid":"` + roomID + `","shareMethod":"card","clientType":"WEB_OUTSIDE_SHARE_H5"}`
	req, err := http.NewRequest("POST", "https://livev.m.chenzhongtech.com/rest/k/live/byUser?kpn=GAME_ZONE&captchaToken=", strings.NewReader(payload))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("User-Agent", "ios/7.830 (ios 17.0; ; iPhone 15 (A2846/A3089/A3090/A3090/A3092))")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.8,zh-TW;q=0.7,zh-HK;q=0.5,en-US;q=0.3,en;q=0.2")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", "https://www.kuaishou.com/")
	req.Header.Set("Cookie", kuaishouCookie())

	resp, err := builtinHTTPClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", err
	}

	var data ksByUserResp
	if len(body) == 0 {
		kuaishouNoteRisk()
		return "", "", "", fmt.Errorf("分享接口返回空响应（疑似风控）")
	}
	if err := json.Unmarshal(body, &data); err != nil {
		kuaishouNoteRisk()
		return "", "", "", fmt.Errorf("分享接口返回非 JSON: %w", err)
	}
	// result=1 且未开播是正常离线态；除此之外视为风控/失败
	if data.Result != 1 && !data.LiveStream.Living {
		kuaishouNoteRisk()
		msg := data.ErrorMsg
		if msg == "" {
			msg = "未知错误"
		}
		return "", "", "", fmt.Errorf("分享接口返回 result=%d (%s)", data.Result, msg)
	}
	kuaishouNoteOK()

	anchorName := data.LiveStream.User.UserName
	if anchorName == "" {
		anchorName = roomID
	}
	avatar := ""
	if data.LiveStream.User.HeadURL != "" {
		avatar = wrapKuaishouImageProxy(data.LiveStream.User.HeadURL)
	}

	if !data.LiveStream.Living {
		return "", anchorName, avatar, nil
	}

	ls := data.LiveStream
	if streamURL := kuaishouPickResolutionGroup(ls.MultiResolutionPlayUrls, quality); streamURL != "" {
		return streamURL, anchorName, avatar, nil
	}
	if ls.PlayURL != "" {
		return ls.PlayURL, anchorName, avatar, nil
	}
	if streamURL := kuaishouPickResolutionGroup(ls.MultiResolutionHlsPlayUrls, quality); streamURL != "" {
		return streamURL, anchorName, avatar, nil
	}
	if ls.HLSPlayURL != "" {
		return ls.HLSPlayURL, anchorName, avatar, nil
	}
	return "", anchorName, avatar, fmt.Errorf("分享接口显示开播中但未返回可用流地址")
}

// kuaishouPickResolutionGroup 从 multiResolution*PlayUrls 分档列表中按画质取流
func kuaishouPickResolutionGroup(groups []ksResolutionGroup, quality string) string {
	if len(groups) == 0 {
		return ""
	}
	index := 0
	switch quality {
	case "sd":
		index = len(groups) - 1
	case "hd":
		index = len(groups) / 2
	}
	return groups[index].first()
}

// wrapKuaishouImageProxy 封面/头像走图片代理，规避快手图床防盗链
func wrapKuaishouImageProxy(raw string) string {
	return "/api/v1/builtin_recorder/proxy_image?url=" + url.QueryEscape(raw)
}

// ---------------- Soop ----------------
