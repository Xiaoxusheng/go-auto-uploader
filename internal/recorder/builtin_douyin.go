package recorder

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type DouyinBuiltinPlatform struct{}

// 手机端分享短链只携带 room_id，enter 接口只认 web_rid（错误码 4001038），
// 必须先经 reflow 页把 room_id 换算成 webRid 才能进标准探测流程。
var douyinWebRidCache sync.Map // 名单原始 roomID -> douyinRidEntry（含失败占位，避免每次轮询都重试）

type douyinRidEntry struct {
	webRid     string
	resolvedAt time.Time
}

const douyinWebRidRefreshInterval = 10 * time.Minute

var (
	douyinLiveURLRe  = regexp.MustCompile(`live\.douyin\.com/(\d+)`)
	douyinRootLiveRe = regexp.MustCompile(`douyin\.com/(?:root/)?live/(\d+)`)
	douyinReflowRe   = regexp.MustCompile(`(?:reflow|room_id(?:_str)?)/(\d+)`)
	douyinWebRidRe   = regexp.MustCompile(`webRid\\?":\\?"(\d+)`)
	douyinPageURLRe  = regexp.MustCompile(`(?:v\.douyin\.com|amemv\.com)`)
)

// isDouyinRoomIDCandidate 超长纯数字（>=15 位）一般是 room_id 而非 web_rid。
func isDouyinRoomIDCandidate(s string) bool {
	if len(s) < 15 {
		return false
	}
	return isAllDigits(s)
}

// resolveDouyinWebRid 把名单里的短链 / room_id 换算成 enter 接口可用的 web_rid。
// 标准格式（live.douyin.com/<web_rid> 的纯数字段）原样返回，不进换算流程。
func resolveDouyinWebRid(roomID string) string {
	rid := strings.TrimSpace(roomID)
	if rid == "" {
		return rid
	}
	if e, ok := douyinWebRidCache.Load(rid); ok {
		if w := e.(douyinRidEntry).webRid; w != "" {
			return w
		}
		return rid
	}
	if !douyinPageURLRe.MatchString(rid) && !douyinLiveURLRe.MatchString(rid) &&
		!douyinRootLiveRe.MatchString(rid) && !isDouyinRoomIDCandidate(rid) {
		return rid
	}
	webRid := resolveDouyinWebRidUncached(rid)
	douyinWebRidCache.Store(rid, douyinRidEntry{webRid: webRid, resolvedAt: time.Now()})
	if webRid == "" {
		log.Printf("[BUILTIN] 🔁 房间换算失败（10分钟后重试）: %s", rid)
		return rid
	}
	log.Printf("[BUILTIN] 🔁 房间换算: %s → web_rid %s", rid, webRid)
	return webRid
}

// resolveDouyinWebRidUncached 跟随短链重定向定位 room_id，再从 reflow 页提取 webRid。
// 支持三种输入：裸 room_id（上游解析引擎只解出数字）、live.douyin.com 数字链接、
// v.douyin.com/amemv 短链（跟随重定向）。
func resolveDouyinWebRidUncached(link string) string {
	roomID := ""
	switch {
	case isDouyinRoomIDCandidate(strings.TrimSpace(link)):
		roomID = strings.TrimSpace(link)
	case douyinLiveURLRe.MatchString(link), douyinRootLiveRe.MatchString(link):
		m := douyinLiveURLRe.FindStringSubmatch(link)
		if m == nil {
			m = douyinRootLiveRe.FindStringSubmatch(link)
		}
		if !isDouyinRoomIDCandidate(m[1]) {
			return m[1]
		}
		roomID = m[1]
	default:
		finalURL, body, err := douyinFetchBody(link)
		if err != nil {
			return ""
		}
		if m := douyinLiveURLRe.FindStringSubmatch(finalURL); m != nil {
			return m[1]
		}
		// 优先信任重定向后的 URL；短链不走重定向时兜底扫响应体
		if m := douyinReflowRe.FindStringSubmatch(finalURL); m != nil {
			roomID = m[1]
		} else if m := douyinReflowRe.FindStringSubmatch(string(body)); m != nil {
			roomID = m[1]
		}
	}
	if roomID == "" {
		return ""
	}
	_, page, err := douyinFetchBody("https://webcast.amemv.com/douyin/webcast/reflow/" + roomID)
	if err != nil {
		return ""
	}
	if m := douyinWebRidRe.FindStringSubmatch(string(page)); m != nil {
		return m[1]
	}
	return ""
}

// shouldRefreshDouyinWebRid 换算结果会随开播会话轮换而失效，
// 失效后按固定间隔允许重算一次；未换算过的超长纯数字视作 room_id 直接尝试。
func shouldRefreshDouyinWebRid(origRoomID, currentRid string) bool {
	if e, ok := douyinWebRidCache.Load(origRoomID); ok {
		return time.Since(e.(douyinRidEntry).resolvedAt) > douyinWebRidRefreshInterval
	}
	return currentRid == origRoomID && isDouyinRoomIDCandidate(origRoomID)
}

// douyinFetchBody 带 UA 拉取页面，返回最终重定向 URL 与响应体。
func douyinFetchBody(rawURL string) (string, []byte, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	resp, err := builtinHTTPClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	finalURL := ""
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	return finalURL, body, nil
}

// GetPlatformName 提供用于逻辑判断及配置索引的抖音平台名标识
func (d *DouyinBuiltinPlatform) GetPlatformName() string { return "Douyin" }

// GetStreamURL 动态调用抖音 API 探测目标房间号，获得推流地址及封面和信息
func (d *DouyinBuiltinPlatform) GetStreamURL(roomID string, quality string) (string, string, string, error) {
	origRoomID := roomID
	roomID = resolveDouyinWebRid(roomID)

	params := url.Values{}
	params.Set("aid", "6383")
	params.Set("app_name", "douyin_web")
	params.Set("live_id", "1")
	params.Set("device_platform", "web")
	params.Set("language", "zh-CN")
	params.Set("browser_language", "zh-CN")
	params.Set("browser_platform", "Win32")
	params.Set("browser_name", "Chrome")
	params.Set("browser_version", "116.0.0.0")
	params.Set("web_rid", roomID)
	params.Set("msToken", "")

	ua := "Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/116.0.5845.97 Safari/537.36 Core/1.116.567.400 QQBrowser/19.7.6764.400"
	query := params.Encode()
	aBogus := builtinGenerateABogus(query, ua)
	apiURL := fmt.Sprintf("https://live.douyin.com/webcast/room/web/enter/?%s&a_bogus=%s", query, aBogus)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", "", "", err
	}

	builtinCookieMutex.RLock()
	myCookie := builtinCookies.Douyin
	builtinCookieMutex.RUnlock()

	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.8,zh-TW;q=0.7,zh-HK;q=0.5,en-US;q=0.3,en;q=0.2")
	req.Header.Set("Referer", "https://live.douyin.com/")
	if myCookie != "" {
		req.Header.Set("Cookie", myCookie)
	} else {
		req.Header.Set("Cookie", "ttwid=1%7C2iDIYVmjzMcpZ20fcaFde0VghXAA3NaNXE_SLR68IyE%7C1761045455%7Cab35197d5cfb21df6cbb2fa7ef1c9262206b062c315b9d04da746d0b37dfbc7d")
	}

	resp, err := builtinHTTPClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", err
	}

	var data struct {
		Data struct {
			Data []struct {
				Status    int `json:"status"`
				StreamURL struct {
					FlvPullURL    map[string]string `json:"flv_pull_url"`
					HlsPullURLMap map[string]string `json:"hls_pull_url_map"`
				} `json:"stream_url"`
			} `json:"data"`
			User struct {
				Nickname    string `json:"nickname"`
				AvatarThumb struct {
					UrlList []string `json:"url_list"`
				} `json:"avatar_thumb"`
			} `json:"user"`
		} `json:"data"`
	}

	json.Unmarshal(body, &data)

	anchorName := roomID
	if data.Data.User.Nickname != "" {
		anchorName = data.Data.User.Nickname
	}

	avatar := ""
	coverRe := regexp.MustCompile(`(?s)"(?:dynamic_cover|cover|room_cover)"\s*:\s*\{[^}]*"url_list"\s*:\s*\[\s*"([^"]+)"`)
	if m := coverRe.FindSubmatch(body); len(m) >= 2 {
		avatar = strings.ReplaceAll(string(m[1]), `\u002F`, "/")
	} else if len(data.Data.User.AvatarThumb.UrlList) > 0 {
		avatar = data.Data.User.AvatarThumb.UrlList[0]
	}

	if avatar != "" {
		avatar = "/api/v1/builtin_recorder/proxy_image?url=" + url.QueryEscape(avatar)
	}

	if len(data.Data.Data) == 0 {
		// 查不到房间：短链换算结果可能已过期（webRid 轮换），超长纯数字可能是从未换算的 room_id，
		// 按间隔重算一次并重试；仍查不到则按未开播处理
		if shouldRefreshDouyinWebRid(origRoomID, roomID) {
			douyinWebRidCache.Delete(origRoomID)
			if again := resolveDouyinWebRid(origRoomID); again != roomID && again != origRoomID {
				return d.GetStreamURL(again, quality)
			}
		}
		return "", anchorName, avatar, nil
	}

	roomData := data.Data.Data[0]
	if roomData.Status != 2 {
		return "", anchorName, avatar, nil
	}

	var streamURL string
	targetKeys := []string{"ORIGIN1", "ORIGIN", "FULL_HD1"}
	if quality == "hd" {
		targetKeys = []string{"HD1"}
	} else if quality == "sd" {
		targetKeys = []string{"SD1"}
	}

	for _, key := range targetKeys {
		streamURL = roomData.StreamURL.FlvPullURL[key]
		if streamURL != "" {
			break
		}
		streamURL = roomData.StreamURL.HlsPullURLMap[key]
		if streamURL != "" {
			break
		}
	}

	if streamURL == "" {
		for _, v := range roomData.StreamURL.FlvPullURL {
			streamURL = v
			break
		}
	}

	return streamURL, anchorName, avatar, nil
}

// ---------------- Kuaishou ----------------
