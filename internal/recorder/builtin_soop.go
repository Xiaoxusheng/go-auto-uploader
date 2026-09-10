package recorder

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type SoopBuiltinPlatform struct{}

// GetPlatformName 提供用于逻辑判断及配置索引的 Soop 平台名标识
func (s *SoopBuiltinPlatform) GetPlatformName() string { return "Soop" }

// GetStreamURL 请求外网 Soop (AfreecaTV) PC端核心播放接口完成高强度的 CDN 画质授权防爬突破
func (s *SoopBuiltinPlatform) GetStreamURL(roomID string, quality string) (string, string, string, error) {
	// 1. 获取页面元信息 (BroadNo, AnchorName等) 并触发第一次 Cookie 鉴权
	pageUrl := fmt.Sprintf("https://play.sooplive.com/%s", roomID)
	reqPage, err := http.NewRequest("GET", pageUrl, nil)
	if err != nil {
		return "", roomID, "", err
	}

	globalUa := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
	reqPage.Header.Set("User-Agent", globalUa)
	builtinCookieMutex.RLock()
	myCookie := builtinCookies.Soop
	builtinCookieMutex.RUnlock()
	if myCookie != "" {
		reqPage.Header.Set("Cookie", myCookie)
	}

	respPage, err := builtinHTTPClient.Do(reqPage)
	if err != nil {
		return "", roomID, "", err
	}
	defer respPage.Body.Close()

	bodyPage, err := io.ReadAll(respPage.Body)
	if err != nil {
		return "", roomID, "", err
	}
	htmlStr := string(bodyPage)

	// 解析 window.nBroadNo 获取有效且动态更新的直播场次号
	reBroadNo := regexp.MustCompile(`window\.nBroadNo\s*=\s*(\d+|null);`)
	broadNoMatch := reBroadNo.FindStringSubmatch(htmlStr)
	if len(broadNoMatch) < 2 || broadNoMatch[1] == "null" {
		// 未开播（页面明确响应 null）
		return "", roomID, "", nil
	}
	broadNo := broadNoMatch[1]

	// 提取名字：解密页面 JS 转义的主播名称
	anchorName := roomID
	reBjNick := regexp.MustCompile(`window\.szBjNick\s*=\s*['"]((?:\\.|[^'"\\])*)['"]`)
	if m := reBjNick.FindStringSubmatch(htmlStr); len(m) >= 2 {
		anchorName = fmt.Sprintf("%s-%s", s.decodeEscapedString(m[1]), roomID)
	}

	// 2. 调用 player_live_api.php (type=live) 获取基础房间设定、CDN节点及最高支持画质
	apiURL := "https://live.sooplive.com/afreeca/player_live_api.php"
	formLive := url.Values{}
	formLive.Set("bid", roomID)
	formLive.Set("bno", broadNo)
	formLive.Set("type", "live")
	formLive.Set("mode", "landing")
	formLive.Set("player_type", "html5")
	formLive.Set("stream_type", "common")
	formLive.Set("pwd", "")
	formLive.Set("from_api", "0")

	reqLive, err := http.NewRequest("POST", apiURL, strings.NewReader(formLive.Encode()))
	if err != nil {
		return "", anchorName, "", err
	}
	reqLive.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqLive.Header.Set("User-Agent", globalUa)
	reqLive.Header.Set("Origin", "https://play.sooplive.com")
	reqLive.Header.Set("Referer", pageUrl)
	if myCookie != "" {
		reqLive.Header.Set("Cookie", myCookie)
	}

	respLive, err := builtinHTTPClient.Do(reqLive)
	if err != nil {
		return "", anchorName, "", err
	}
	defer respLive.Body.Close()

	bodyLive, _ := io.ReadAll(respLive.Body)

	// ✨ 高容错架构：采用 json.RawMessage 逃避类型审查，针对 label_resolution 这个忽而是数字忽而是字符串的风控节点实施降维打击
	type SoopChannelResp struct {
		Channel struct {
			Result     int    `json:"RESULT"`
			Bno        string `json:"BNO"`
			Rmd        string `json:"RMD"`
			Cdn        string `json:"CDN"`
			Bpwd       string `json:"BPWD"`
			Aid        string `json:"AID"`
			ViewPreset []struct {
				Name            string          `json:"name"`
				Label           string          `json:"label"`
				LabelResolution json.RawMessage `json:"label_resolution"` // 动态接口接收
				BPS             int             `json:"bps"`
			} `json:"VIEWPRESET"`
		} `json:"CHANNEL"`
	}

	var liveRes SoopChannelResp
	if err := json.Unmarshal(bodyLive, &liveRes); err != nil {
		return "", anchorName, "", fmt.Errorf("解析 live 接口 JSON 失败: %v", err)
	}

	// Result=-6 为经典登录失效错误，强制终端提示重试
	if liveRes.Channel.Result != 1 {
		if liveRes.Channel.Result == -6 {
			return "", anchorName, "", fmt.Errorf("需要登录(Result=-6)，请更新 Cookie")
		}
		return "", anchorName, "", nil // 未开播或其他限制
	}

	if liveRes.Channel.Bpwd == "Y" {
		return "", anchorName, "", fmt.Errorf("房间已开启密码保护，无法录制")
	}

	// ----------------------------------------------------
	// 🌟 全新画质权重排序器 (对齐官方的高可用流分配策略)
	// ----------------------------------------------------
	type SoopParsedPreset struct {
		Name string
		Res  int
		BPS  int
	}
	var parsedPresets []SoopParsedPreset

	for _, p := range liveRes.Channel.ViewPreset {
		if strings.EqualFold(p.Name, "auto") {
			continue // 排除假画质
		}
		resVal := s.parseInterfaceToInt(p.LabelResolution)
		parsedPresets = append(parsedPresets, SoopParsedPreset{
			Name: p.Name,
			Res:  resVal,
			BPS:  p.BPS,
		})
	}

	// 双重权重推举：分辨率优先，同等分辨率下 BPS 码率优先
	sort.SliceStable(parsedPresets, func(i, j int) bool {
		if parsedPresets[i].Res != parsedPresets[j].Res {
			return parsedPresets[i].Res > parsedPresets[j].Res
		}
		return parsedPresets[i].BPS > parsedPresets[j].BPS
	})

	if len(parsedPresets) == 0 {
		return "", anchorName, "", fmt.Errorf("没有捕获到任何可用画质流")
	}

	// 根据用户期待画质，智能匹配对应档位；找不到则默认采用头部最高画质作为无损回退
	targetQuality := parsedPresets[0].Name
	if quality == "original" {
		targetQuality = parsedPresets[0].Name
	} else {
		// 模拟用户对 hd 或 sd 的降级诉求查找
		for _, p := range parsedPresets {
			if strings.Contains(strings.ToLower(p.Name), quality) {
				targetQuality = p.Name
				break
			}
		}
	}

	// 3. 申请 AID (type=aid) 构建安全分发的访问鉴权
	formAid := url.Values{}
	formAid.Set("bid", roomID)
	formAid.Set("bno", broadNo)
	formAid.Set("type", "aid")
	formAid.Set("mode", "landing")
	formAid.Set("player_type", "html5")
	formAid.Set("stream_type", "common")
	formAid.Set("pwd", "")
	formAid.Set("quality", targetQuality)
	formAid.Set("from_api", "0")

	reqAid, _ := http.NewRequest("POST", apiURL, strings.NewReader(formAid.Encode()))
	reqAid.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqAid.Header.Set("User-Agent", globalUa)
	reqAid.Header.Set("Origin", "https://play.sooplive.com")
	reqAid.Header.Set("Referer", pageUrl)
	if myCookie != "" {
		reqAid.Header.Set("Cookie", myCookie)
	}

	respAid, err := builtinHTTPClient.Do(reqAid)
	if err != nil {
		return "", anchorName, "", err
	}
	defer respAid.Body.Close()

	bodyAid, _ := io.ReadAll(respAid.Body)
	var aidRes SoopChannelResp
	if err := json.Unmarshal(bodyAid, &aidRes); err != nil {
		return "", anchorName, "", fmt.Errorf("解析 aid 接口 JSON 失败: %v", err)
	}

	if aidRes.Channel.Result != 1 || aidRes.Channel.Aid == "" {
		return "", anchorName, "", fmt.Errorf("申请 AID 失败，Result=%d", aidRes.Channel.Result)
	}
	aid := aidRes.Channel.Aid

	// 4. 获取分配出的最终播放节点 view_url (✨ 强化增加指数退避与防封锁 Headers)
	cdnType := s.mapSoopCDNType(liveRes.Channel.Cdn)
	broadKey := s.buildSoopBroadKey(broadNo, targetQuality)
	rmdHost := strings.TrimRight(liveRes.Channel.Rmd, "/")

	// 追加防缓存时间戳
	viewURLReq := fmt.Sprintf("%s/broad_stream_assign.html?return_type=%s&use_cors=false&cors_origin_url=play.sooplive.com&broad_key=%s&time=%d",
		rmdHost, cdnType, broadKey, time.Now().UnixMilli())

	var bodyView []byte
	var viewFetchErr error

	// ✨ 核心强化：增加 3 次底层网络退避重试，专门抵挡 EOF TCP 重置断开
	for attempt := 1; attempt <= 3; attempt++ {
		reqView, _ := http.NewRequest("GET", viewURLReq, nil)
		// 补全所有特征，让其伪装成真正的合法 Chrome 环境请求
		reqView.Header.Set("User-Agent", globalUa)
		reqView.Header.Set("Accept", "application/json, text/plain, */*")
		reqView.Header.Set("Accept-Language", "ko-KR,ko;q=0.9,en-US;q=0.8,en;q=0.7")
		reqView.Header.Set("Connection", "keep-alive")
		reqView.Header.Set("Origin", "https://play.sooplive.com")
		reqView.Header.Set("Referer", pageUrl)
		if myCookie != "" {
			reqView.Header.Set("Cookie", myCookie)
		}

		respView, err := builtinHTTPClient.Do(reqView)
		if err == nil {
			bodyView, _ = io.ReadAll(respView.Body)
			respView.Body.Close()
			viewFetchErr = nil
			break // 成功则直接跳出重试环
		}

		viewFetchErr = err
		if strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "reset") {
			// 发生 EOF 断连时，休眠 500ms * 尝试次数 进行退避重试
			time.Sleep(time.Duration(500*attempt) * time.Millisecond)
			continue
		}
		break // 非网络强行掐断错误直接抛出不重试
	}

	if viewFetchErr != nil {
		return "", anchorName, "", fmt.Errorf("最终请求分配节点失败: %v", viewFetchErr)
	}

	var viewRes struct {
		ViewUrl string `json:"view_url"`
	}
	if err := json.Unmarshal(bodyView, &viewRes); err != nil {
		return "", anchorName, "", fmt.Errorf("解析 view_url 失败: %v", err)
	}

	if viewRes.ViewUrl == "" {
		return "", anchorName, "", fmt.Errorf("获取到的 view_url 为空")
	}

	finalStreamURL := viewRes.ViewUrl
	if strings.Contains(finalStreamURL, "?") {
		finalStreamURL += "&aid=" + aid
	} else {
		finalStreamURL += "?aid=" + aid
	}

	// 提取头像，由于是原生日历算法，截断前缀直接拼凑出 AF 的图床规则
	avatar := ""
	if len(roomID) >= 2 {
		avatar = fmt.Sprintf("https://stimg.afreecatv.com/LOGO/%s/%s/%s.jpg", roomID[:2], roomID, roomID)
		avatar = "/api/v1/builtin_recorder/proxy_image?url=" + url.QueryEscape(avatar)
	}

	return finalStreamURL, anchorName, avatar, nil
}

// decodeEscapedString 处理 HTML 中 JS 变量带来的深层引号与转义字符污染，洗白文本
func (s *SoopBuiltinPlatform) decodeEscapedString(raw string) string {
	raw = strings.ReplaceAll(raw, `\'`, `'`)
	quoted := `"` + strings.ReplaceAll(raw, `"`, `\"`) + `"`
	if decoded, err := strconv.Unquote(quoted); err == nil {
		return decoded
	}
	return raw
}

// parseInterfaceToInt 作为底层防雷网，将 JSON 接口中可能发生变异的 Number 实体转化为安全整形
func (s *SoopBuiltinPlatform) parseInterfaceToInt(raw json.RawMessage) int {
	var valInt int
	if err := json.Unmarshal(raw, &valInt); err == nil {
		return valInt
	}
	var valStr string
	if err := json.Unmarshal(raw, &valStr); err == nil {
		if i, err2 := strconv.Atoi(valStr); err2 == nil {
			return i
		}
	}
	return 0
}

// mapSoopCDNType 高度仿真浏览器在底层协议中声明的 CDN 连接请求类型
func (s *SoopBuiltinPlatform) mapSoopCDNType(cdn string) string {
	if strings.Contains(cdn, "gs_cdn") {
		return "gs_cdn_pc_web"
	}
	if strings.Contains(cdn, "lg_cdn") {
		return "lg_cdn_pc_web"
	}
	return cdn
}

// buildSoopBroadKey 完全对齐目标架构签名组装器，组装给调度系统的校验凭证
func (s *SoopBuiltinPlatform) buildSoopBroadKey(broadNo, quality string) string {
	return fmt.Sprintf("%s-common-%s-hls", broadNo, quality)
}

// apiProxyImage 设置本地反代接口转发获取直播封面图并伪造请求头，穿透部分平台的防盗链拦截限制
