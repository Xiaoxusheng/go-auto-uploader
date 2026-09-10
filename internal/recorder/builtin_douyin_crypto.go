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
	re := regexp.MustCompile(`https?://v\.douyin\.com/[a-zA-Z0-9]+/?`)
	shortURL := re.FindString(text)

	if shortURL == "" {
		return "", fmt.Errorf("未在文本中找到抖音分享短链接")
	}
	log.Printf("\n🔍 [解析引擎] 提取到纯净短链接: %s\n", shortURL)

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
		log.Printf("⚠️ [解析引擎] 无头浏览器超时或未安装，触发底层 HTTP 拦截器保底...")
	}

	fallbackClient := &http.Client{
		Timeout: 10 * time.Second,
	}
	req, _ := http.NewRequest("GET", shortURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, fallbackErr := fallbackClient.Do(req)
	if fallbackErr == nil {
		defer resp.Body.Close()
		resolvedURL := resp.Request.URL.String()

		if strings.Contains(resolvedURL, "douyin.com/user/") {
			return "", fmt.Errorf("解析失败: 该主播当前未开播 (重定向至个人主页)")
		}

		longIDRe := regexp.MustCompile(`\d{18,20}`)
		longID := longIDRe.FindString(resolvedURL)
		if longID != "" {
			return fmt.Sprintf("https://live.douyin.com/%s", longID), nil
		}
		return strings.Split(resolvedURL, "?")[0], nil
	}

	return "", fmt.Errorf("双重解析方案均未拿到有效房间号，可能是网络受限或滑块拦截")
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
