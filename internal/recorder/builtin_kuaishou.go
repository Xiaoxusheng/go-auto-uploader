package recorder

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

type KuaishouBuiltinPlatform struct{}

// GetPlatformName 提供用于逻辑判断及配置索引的快手平台名标识
func (k *KuaishouBuiltinPlatform) GetPlatformName() string { return "Kuaishou" }

// GetStreamURL 请求快手服务端底层接口解析主播 ID 并返回视频分发连接信息
func (k *KuaishouBuiltinPlatform) GetStreamURL(roomID string, quality string) (string, string, string, error) {
	apiURL := "https://livev.m.chenzhongtech.com/rest/k/live/byUser?kpn=GAME_ZONE&captchaToken="

	reqData := map[string]interface{}{
		"source":      5,
		"eid":         roomID,
		"shareMethod": "card",
		"clientType":  "WEB_OUTSIDE_SHARE_H5",
	}
	jsonData, _ := json.Marshal(reqData)

	req, err := http.NewRequest("POST", apiURL, strings.NewReader(string(jsonData)))
	if err != nil {
		return k.fallbackWeb(roomID, quality)
	}

	req.Header.Set("User-Agent", "ios/7.830 (ios 17.0; ; iPhone 15 (A2846/A3089/A3090/A3090/A3092))")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.8,zh-TW;q=0.7,zh-HK;q=0.5,en-US;q=0.3,en;q=0.2")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", "https://www.kuaishou.com/short-video/3x224rwabjmuc9y?fid=1712760877&cc=share_copylink&followRefer=151&shareMethod=TOKEN&docId=9&kpn=KUAISHOU&subBiz=BROWSE_SLIDE_PHOTO&photoId=3x224rwabjmuc9y&shareId=17144298796566&shareToken=X-6FTMeYTsY97qYL&shareResourceType=PHOTO_OTHER&userId=3xtnuitaz2982eg&shareType=1&et=1_i/2000048330179867715_h3052&shareMode=APP&originShareId=17144298796566&appType=21&shareObjectId=5230086626478274600&shareUrlOpened=0&timestamp=1663833792288&utm_source=app_share&utm_medium=app_share&utm_campaign=app_share&location=app_share")

	builtinCookieMutex.RLock()
	myCookie := builtinCookies.Kuaishou
	builtinCookieMutex.RUnlock()
	if myCookie != "" {
		req.Header.Set("Cookie", myCookie)
	} else {
		req.Header.Set("Cookie", "did=web_e988652e11b545469633396abe85a89f; didv=1796004001000")
	}

	resp, err := builtinHTTPClient.Do(req)
	if err != nil {
		return k.fallbackWeb(roomID, quality)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return k.fallbackWeb(roomID, quality)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return k.fallbackWeb(roomID, quality)
	}

	liveStream, ok := result["liveStream"].(map[string]interface{})
	if !ok || liveStream == nil {
		return k.fallbackWeb(roomID, quality)
	}

	anchorName := roomID
	avatar := ""
	if userMap, ok := liveStream["user"].(map[string]interface{}); ok {
		if userName, ok := userMap["user_name"].(string); ok && userName != "" {
			anchorName = userName
		}
		if headUrl, ok := userMap["headUrl"].(string); ok && headUrl != "" {
			avatar = headUrl
		}
	}

	if avatar != "" {
		avatar = "/api/v1/builtin_recorder/proxy_image?url=" + url.QueryEscape(avatar)
	}

	living, _ := liveStream["living"].(bool)
	if !living {
		return "", anchorName, avatar, nil
	}

	var finalStreamURL string

	if multiUrls, ok := liveStream["multiResolutionPlayUrls"].([]interface{}); ok && len(multiUrls) > 0 {
		idx := 0
		if quality == "sd" {
			idx = len(multiUrls) - 1
		} else if quality == "hd" && len(multiUrls) > 1 {
			idx = 1
		}
		if firstObj, ok := multiUrls[idx].(map[string]interface{}); ok {
			if urls, ok := firstObj["urls"].([]interface{}); ok && len(urls) > 0 {
				if urlObj, ok := urls[0].(map[string]interface{}); ok {
					if urlStr, ok := urlObj["url"].(string); ok {
						finalStreamURL = urlStr
					}
				}
			}
		}
	}

	if finalStreamURL == "" {
		if playUrls, ok := liveStream["playUrls"].([]interface{}); ok && len(playUrls) > 0 {
			if urlObj, ok := playUrls[0].(map[string]interface{}); ok {
				if urlStr, ok := urlObj["url"].(string); ok {
					finalStreamURL = urlStr
				}
			}
		}
	}

	if finalStreamURL == "" {
		return k.fallbackWeb(roomID, quality)
	}

	return finalStreamURL, anchorName, avatar, nil
}

// fallbackWeb 当快手 App 端接口由于版本或风控限制无法返回数据时，退回使用普通网页访问强行抽取
func (k *KuaishouBuiltinPlatform) fallbackWeb(roomID string, quality string) (string, string, string, error) {
	reqURL := fmt.Sprintf("https://live.kuaishou.com/u/%s", roomID)
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return "", "", "", err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	builtinCookieMutex.RLock()
	myCookie := builtinCookies.Kuaishou
	builtinCookieMutex.RUnlock()
	if myCookie != "" {
		req.Header.Set("Cookie", myCookie)
	} else {
		req.Header.Set("Cookie", "did=web_12345678901234567890123456789012")
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
	htmlStr := string(body)

	anchorName := roomID
	titleRe := regexp.MustCompile(`<title>([^<]+)</title>`)
	if m := titleRe.FindStringSubmatch(htmlStr); len(m) >= 2 {
		name := strings.Split(m[1], "在快手直播")[0]
		if strings.TrimSpace(name) != "" {
			anchorName = strings.TrimSpace(name)
		}
	}

	avatar := ""
	posterRe := regexp.MustCompile(`"(?:poster|coverUrl|livePoster)"\s*:\s*"([^"]+)"`)
	if m := posterRe.FindSubmatch(body); len(m) >= 2 {
		avatar = strings.ReplaceAll(string(m[1]), `\u002F`, "/")
	} else {
		avatarRe := regexp.MustCompile(`"(?:headUrl|avatar)"\s*:\s*"([^"]+)"`)
		if m := avatarRe.FindSubmatch(body); len(m) >= 2 {
			avatar = strings.ReplaceAll(string(m[1]), `\u002F`, "/")
		}
	}

	if avatar != "" {
		avatar = "/api/v1/builtin_recorder/proxy_image?url=" + url.QueryEscape(avatar)
	}

	re := regexp.MustCompile(`window\.__INITIAL_STATE__=({.*?});\(function`)
	matches := re.FindSubmatch(body)
	if len(matches) < 2 {
		return "", anchorName, avatar, fmt.Errorf("移动端/PC端均无法获取快手数据，可能被防爬拦截")
	}

	streamRe := regexp.MustCompile(`"url":"([^"]+\.flv[^"]*)"`)
	streamMatches := streamRe.FindAllStringSubmatch(string(matches[1]), -1)
	if len(streamMatches) > 0 {
		idx := 0
		if quality == "sd" {
			idx = len(streamMatches) - 1
		}
		return strings.ReplaceAll(streamMatches[idx][1], `\u0026`, "&"), anchorName, avatar, nil
	}
	return "", anchorName, avatar, nil
}

// ---------------- Soop ----------------
