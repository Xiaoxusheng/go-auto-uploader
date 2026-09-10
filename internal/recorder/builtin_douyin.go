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

type DouyinBuiltinPlatform struct{}

// GetPlatformName 提供用于逻辑判断及配置索引的抖音平台名标识
func (d *DouyinBuiltinPlatform) GetPlatformName() string { return "Douyin" }

// GetStreamURL 动态调用抖音 API 探测目标房间号，获得推流地址及封面和信息
func (d *DouyinBuiltinPlatform) GetStreamURL(roomID string, quality string) (string, string, string, error) {
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
