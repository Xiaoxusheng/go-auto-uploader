package recorder

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

func ExtractBuiltinDouyinLiveURL(text string) (string, error) {
	re := regexp.MustCompile(`https?://v\.douyin\.com/[a-zA-Z0-9\-_]+/?`)
	shortURL := re.FindString(text)

	if shortURL == "" {
		return "", fmt.Errorf("未在文本中找到抖音分享短链接")
	}
	log.Printf("\n🔍 [解析引擎] 提取到纯净短链接: %s\n", shortURL)

	// 先走秒级 HTTP 重定向拦截；「未开播」是确定结论直接返回，
	// 其余失败/结论不明确时才升级无头 Chrome 深度穿透，避免每次添加白等十几秒
	result, err := resolveBuiltinDouyinByHTTP(shortURL)
	if err == nil || strings.Contains(err.Error(), "未开播") {
		return result, err
	}
	log.Printf("⚠️ [解析引擎] HTTP 拦截未拿到房间号（%v），升级无头 Chrome 深度穿透...", err)

	return resolveBuiltinDouyinByChrome(shortURL)
}

// resolveBuiltinDouyinByChrome 启动无头 Chrome 穿透反爬页，从最终 URL/页面数据里提取房间号。
func resolveBuiltinDouyinByChrome(shortURL string) (string, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("mute-audio", true),
		chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"),
	)
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	ctx, cancel2 := chromedp.NewContext(allocCtx)
	defer cancel2()

	ctx, cancel3 := context.WithTimeout(ctx, 15*time.Second)
	defer cancel3()

	var finalURL string
	var htmlContent string

	log.Println("⏳ [解析引擎] 正在启动无头 Chrome 进行深度穿透 (最长等待15秒)...")
	err := chromedp.Run(ctx,
		chromedp.Navigate(shortURL),
		chromedp.WaitReady("body"),
		chromedp.Sleep(3*time.Second), // 等待 JS 充分执行重定向
		chromedp.Location(&finalURL),
		chromedp.OuterHTML("html", &htmlContent),
	)

	if err == nil {
		if strings.Contains(finalURL, "douyin.com/user/") {
			return "", fmt.Errorf("解析失败: 该主播当前未开播 (重定向至个人主页)")
		}

		idRe := regexp.MustCompile(`live\.douyin\.com/(\d+)`)
		urlMatches := idRe.FindStringSubmatch(finalURL)
		if len(urlMatches) > 1 {
			idStr := urlMatches[1]
			if len(idStr) < 18 {
				return fmt.Sprintf("https://live.douyin.com/%s", idStr), nil
			}
		}
		webRid := extractBuiltinWebRid(htmlContent)
		if webRid != "" {
			return fmt.Sprintf("https://live.douyin.com/%s", webRid), nil
		}
	} else {
		log.Printf("err: %v", err)
		log.Printf("⚠️ [解析引擎] 无头浏览器超时或未安装...")
	}

	return "", fmt.Errorf("双重解析方案均未拿到有效房间号，可能是网络受限或滑块拦截")
}

// resolveBuiltinDouyinByHTTP 轻量解析：跟随分享短链重定向，从最终 URL 提取房间号。
// 重定向到个人主页 = 主播未开播的确定结论；拿不到有效房间号时返回错误交由 Chrome 兜底。
func resolveBuiltinDouyinByHTTP(shortURL string) (string, error) {
	fallbackClient := &http.Client{
		Timeout: 10 * time.Second,
	}
	req, _ := http.NewRequest("GET", shortURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := fallbackClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP 拦截失败: %v", err)
	}
	defer resp.Body.Close()
	resolvedURL := resp.Request.URL.String()

	if strings.Contains(resolvedURL, "douyin.com/user/") {
		return "", fmt.Errorf("解析失败: 该主播当前未开播 (重定向至个人主页)")
	}

	longIDRe := regexp.MustCompile(`\d{18,20}`)
	if longID := longIDRe.FindString(resolvedURL); longID != "" {
		return fmt.Sprintf("https://live.douyin.com/%s", longID), nil
	}
	if m := regexp.MustCompile(`live\.douyin\.com/(\d+)`).FindStringSubmatch(resolvedURL); m != nil {
		return fmt.Sprintf("https://live.douyin.com/%s", m[1]), nil
	}
	return "", fmt.Errorf("重定向未落在直播间页: %s", strings.Split(resolvedURL, "?")[0])
}

// extractBuiltinWebRid 使用正则暴力在返回的 HTML 数据中筛查包含真实房间号的配置段落
func extractBuiltinWebRid(html string) string {
	patterns := []string{
		`"web_rid"\s*:\s*"(\d+)"`,
		`\\"web_rid\\"\s*:\s*\\"(\d+)\\"`,
		`%22web_rid%22%3A%22(\d+)%22`,
		`"short_id"\s*:\s*"(\d+)"`,
		`\\"short_id\\"\s*:\s*\\"(\d+)\\"`,
		`%22short_id%22%3A%22(\d+)%22`,
		`"web_rid"\s*:\s*(\d+)`,
		`"short_id"\s*:\s*(\d+)`,
	}

	for _, p := range patterns {
		re := regexp.MustCompile(p)
		matches := re.FindAllStringSubmatch(html, -1)
		for _, match := range matches {
			idStr := match[1]
			if len(idStr) > 4 && len(idStr) < 18 {
				return idStr
			}
		}
	}
	return ""
}

// ==========================================
// 核心加密算法（实现见 internal/recorder）
// ==========================================

// builtinRC4Encrypt 实现标准的 RC4 对称加密方法
func builtinRC4Encrypt(plaintext, key string) string {
	return RC4Encrypt(plaintext, key)
}

// builtinGenerateABogus 生成抖音 a_bogus 签名
func builtinGenerateABogus(params, userAgent string) string {
	return GenerateABogus(params, userAgent)
}

// ==========================================
// 🚀 平台抓取实现
// ==========================================

// ---------------- Douyin ----------------
