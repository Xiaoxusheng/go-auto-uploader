// Package recorder — Cookie 健康检查：主动探活（有权威校验接口的平台）+ 被动连续错误检测（全平台），
// 失效时经 hookNotify 推送告警，健康状态经 /cookies/health 供前端 Cookie 面板标红。
package recorder

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CookieHealth 单平台 Cookie 健康快照。
// State: ok=正常 invalid=已失效 suspect=疑似失效(连续接口报错) unknown=未检测/探测失败
type CookieHealth struct {
	Platform  string `json:"platform"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
	CheckedAt string `json:"checked_at"`
}

var (
	builtinCookieHealth sync.Map // platform -> CookieHealth
	builtinCookieProbes sync.Map // platform -> *cookieProbeState（被动连续错误计数）
	builtinHealthHTTP   = &http.Client{Timeout: 15 * time.Second}
)

type cookieProbeState struct {
	mu                sync.Mutex
	consecutiveErrors int
}

func platformCookie(platform string) string {
	builtinCookieMutex.RLock()
	defer builtinCookieMutex.RUnlock()
	if builtinCookies == nil {
		return ""
	}
	switch platform {
	case "Douyin":
		return builtinCookies.Douyin
	case "Kuaishou":
		return builtinCookies.Kuaishou
	case "Soop":
		return builtinCookies.Soop
	case "Bilibili":
		return builtinCookies.Bilibili
	case "Twitch":
		return builtinCookies.Twitch
	}
	return ""
}

// setCookieHealth 更新健康状态；仅在跨入 invalid/suspect 时告警、跨回 ok 时报恢复，
// unknown（探测网络失败）不覆盖已有结论，避免网络抖动洗掉真实失效告警。
func setCookieHealth(platform, state, reason string) {
	prevAny, had := builtinCookieHealth.Load(platform)
	prevState := "unknown"
	if had {
		prevState = prevAny.(CookieHealth).State
	}

	if state == "unknown" && had && prevState != "unknown" {
		return
	}

	health := CookieHealth{
		Platform:  platform,
		State:     state,
		Reason:    reason,
		CheckedAt: time.Now().Format("2006-01-02 15:04:05"),
	}
	builtinCookieHealth.Store(platform, health)

	prevBad := prevState == "invalid" || prevState == "suspect"
	nowBad := state == "invalid" || state == "suspect"
	if nowBad && !prevBad {
		log.Printf("[COOKIE] ❌ 平台 %s Cookie 健康异常（%s）: %s", platform, state, reason)
		hookNotify("Cookie告警", fmt.Sprintf("平台 [%s] 的 Cookie %s：%s。请尽快到控制台「Cookie 设置」更新，否则相关直播间的录制/画质会受影响！", platform, stateLabel(state), reason))
	} else if !nowBad && prevBad {
		log.Printf("[COOKIE] ✅ 平台 %s Cookie 已恢复正常", platform)
		hookNotify("Cookie恢复", fmt.Sprintf("平台 [%s] 的 Cookie 已恢复正常，相关直播间录制不受影响。", platform))
	}
}

func stateLabel(s string) string {
	switch s {
	case "invalid":
		return "已失效"
	case "suspect":
		return "疑似失效"
	}
	return "异常"
}

// recordCookieProbeOutcome 监控循环在每次房间解析后上报结果：
// err != nil 计入平台连续错误，成功/正常未开播则清零并判 Cookie 健康。
func recordCookieProbeOutcome(platform string, failed bool) {
	if platformCookie(platform) == "" {
		return
	}
	any, _ := builtinCookieProbes.LoadOrStore(platform, &cookieProbeState{})
	st := any.(*cookieProbeState)

	st.mu.Lock()
	if !failed {
		st.consecutiveErrors = 0
		st.mu.Unlock()
		if h, ok := builtinCookieHealth.Load(platform); ok {
			if hh := h.(CookieHealth); hh.State == "suspect" {
				setCookieHealth(platform, "ok", "平台接口恢复正常")
			}
		}
		return
	}
	st.consecutiveErrors++
	hits := st.consecutiveErrors
	st.mu.Unlock()

	// 连续 12 次（多房间同时报错也累计）平台级错误：大概率 Cookie 失效/风控，也可能平台故障
	if hits == 12 {
		setCookieHealth(platform, "suspect", "监控接口连续 12 次报错，Cookie 可能已失效或被风控（平台故障也会出现）")
	}
}

// builtinCookieHealthLoop 常驻探活协程：启动 90 秒后首检，之后每 30 分钟一次。
func builtinCookieHealthLoop() {
	time.Sleep(90 * time.Second)
	for {
		probeBilibiliCookie()
		probeTwitchCookie()
		time.Sleep(30 * time.Minute)
	}
}

// probeBilibiliCookie 用 nav 接口校验 SESSDATA（isLogin=false 即失效，结论权威）。
func probeBilibiliCookie() {
	ck := platformCookie("Bilibili")
	if ck == "" {
		return
	}
	req, err := http.NewRequest(http.MethodGet, "https://api.bilibili.com/x/web-interface/nav", nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Cookie", ck)
	resp, err := builtinHealthHTTP.Do(req)
	if err != nil {
		setCookieHealth("Bilibili", "unknown", "探活请求失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil || resp.StatusCode != http.StatusOK {
		setCookieHealth("Bilibili", "unknown", fmt.Sprintf("探活响应异常: HTTP %d", resp.StatusCode))
		return
	}
	var out struct {
		Code int `json:"code"`
		Data struct {
			IsLogin bool `json:"isLogin"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &out) != nil {
		setCookieHealth("Bilibili", "unknown", "探活响应解析失败")
		return
	}
	if out.Data.IsLogin {
		setCookieHealth("Bilibili", "ok", "SESSDATA 校验通过")
	} else {
		setCookieHealth("Bilibili", "invalid", "B 站接口返回未登录，SESSDATA 已失效")
	}
}

// probeTwitchCookie 用 OAuth validate 接口校验 token（401 即失效，结论权威）。
func probeTwitchCookie() {
	token := strings.TrimSpace(twitchAuthToken())
	if token == "" {
		return
	}
	req, err := http.NewRequest(http.MethodGet, "https://id.twitch.tv/oauth2/validate", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "OAuth "+token)
	resp, err := builtinHealthHTTP.Do(req)
	if err != nil {
		setCookieHealth("Twitch", "unknown", "探活请求失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode == http.StatusOK {
		setCookieHealth("Twitch", "ok", "OAuth Token 校验通过")
	} else if resp.StatusCode == http.StatusUnauthorized {
		setCookieHealth("Twitch", "invalid", "Twitch 返回 401，OAuth Token 已失效")
	} else {
		setCookieHealth("Twitch", "unknown", fmt.Sprintf("探活响应异常: HTTP %d", resp.StatusCode))
	}
}

// GetBuiltinCookieHealth 汇总各平台 Cookie 健康快照（未配置 Cookie 的平台不出现在结果里）。
func GetBuiltinCookieHealth() map[string]CookieHealth {
	out := make(map[string]CookieHealth)
	for _, p := range []string{"Douyin", "Kuaishou", "Soop", "Bilibili", "Twitch"} {
		if platformCookie(p) == "" {
			continue
		}
		h := CookieHealth{Platform: p, State: "unknown", Reason: "尚未检测"}
		if v, ok := builtinCookieHealth.Load(p); ok {
			h = v.(CookieHealth)
		}
		out[p] = h
	}
	return out
}

// apiCookieHealth GET /api/v1/builtin_recorder/cookies/health
func apiCookieHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		hookJSONErr(w, r, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	hookJSONOK(w, r, GetBuiltinCookieHealth())
}
