package recorder

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
)

func apiRecorderConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var c BuiltinConfig
		if err := hookParseEncrypted(r, &c); err != nil {
			hookJSONErr(w, r, http.StatusBadRequest, "商业安全网关拦截: 非法配置实体或解密异常")
			return
		}

		if c.Quality != "" {
			builtinConfig.Quality = c.Quality
		}
		builtinConfig.SegmentTime = c.SegmentTime
		if c.SavePath != "" {
			builtinConfig.SavePath = c.SavePath
		}

		// ✨ 更新前端传来的水印参数
		builtinConfig.WatermarkEnable = c.WatermarkEnable
		builtinConfig.VideoWatermarkEnable = c.VideoWatermarkEnable
		builtinConfig.WatermarkText = c.WatermarkText
		builtinConfig.WatermarkFormat = c.WatermarkFormat
		builtinConfig.WatermarkPosition = c.WatermarkPosition
		if c.WatermarkFontSize > 0 {
			builtinConfig.WatermarkFontSize = c.WatermarkFontSize
		}
		if c.WatermarkFontColor != "" {
			builtinConfig.WatermarkFontColor = c.WatermarkFontColor
		}

		data, _ := json.MarshalIndent(builtinConfig, "", "    ")
		os.WriteFile("builtin_config.json", data, 0644)
		hookJSONOK(w, r, nil)
		return
	}
	hookJSONOK(w, r, builtinConfig)
}

// apiRecorderCookies 处理内置引擎应对各大平台反制而提供的 Cookie 更新，已强制兼容加密格式接收
func apiRecorderCookies(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var c BuiltinCookieConfig
		if err := hookParseEncrypted(r, &c); err != nil {
			hookJSONErr(w, r, http.StatusBadRequest, "商业安全网关拦截: 非法 Cookie 实体或解密异常")
			return
		}

		builtinCookieMutex.Lock()
		builtinCookies.Douyin = c.Douyin
		builtinCookies.Kuaishou = c.Kuaishou
		builtinCookies.Soop = c.Soop
		builtinCookieMutex.Unlock()
		data, _ := json.MarshalIndent(builtinCookies, "", "    ")
		os.WriteFile("builtin_cookies.json", data, 0644)
		hookJSONOK(w, r, nil)
		return
	}
	builtinCookieMutex.RLock()
	hookJSONOK(w, r, builtinCookies)
	builtinCookieMutex.RUnlock()
}

// apiRecorderAdd 提供将前端通过面板添加的单条或批量直播间转录成录制指令池内的待处理任务功能
func apiRecorderAdd(w http.ResponseWriter, r *http.Request) {
	var d struct {
		Platform string `json:"platform"`
		URL      string `json:"url"`
	}

	if err := hookParseEncrypted(r, &d); err != nil {
		hookJSONErr(w, r, http.StatusBadRequest, "商业安全网关拦截: 非法添加实体或解密异常")
		return
	}

	lines := strings.Split(d.URL, "\n")
	addedCount := 0
	duplicateCount := 0

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}

		var customNameFromSuffix string
		if idx := strings.LastIndex(line, ",主播:"); idx != -1 {
			customNameFromSuffix = strings.TrimSpace(line[idx+len(",主播:"):])
			line = line[:idx]
		} else if idx := strings.LastIndex(line, ", 主播:"); idx != -1 {
			customNameFromSuffix = strings.TrimSpace(line[idx+len(", 主播:"):])
			line = line[:idx]
		}

		if customNameFromSuffix == "" {
			nameRe := regexp.MustCompile(`【([^】]+)】`)
			if m := nameRe.FindStringSubmatch(line); len(m) > 1 {
				customNameFromSuffix = m[1]
			}
		}

		urlRe := regexp.MustCompile(`https?://[^\s,]+`)
		foundURL := urlRe.FindString(line)
		if foundURL != "" {
			line = foundURL
		}

		shortURLRe := regexp.MustCompile(`https?://v\.douyin\.com/[a-zA-Z0-9]+/?`)
		if shortURLRe.MatchString(line) {
			log.Printf("[BUILTIN] 检测到抖音短链接，正在解析: %s", line)
			realURL, err := ExtractBuiltinDouyinLiveURL(line)
			if err == nil && realURL != "" {
				log.Printf("[BUILTIN] ✅ 最终解析成功: %s", realURL)
				line = realURL
			} else {
				log.Printf("[BUILTIN] ❌ 短链接解析失败: %v", err)
			}
		}

		if idx := strings.Index(line, "?"); idx != -1 {
			line = line[:idx]
		}
		line = strings.TrimSuffix(line, "/")

		fullLineToSave := line
		if customNameFromSuffix != "" {
			fullLineToSave = line + ",主播:" + customNameFromSuffix
		}

		isP, platformName, roomID, customName, _, addFlags := parseBuiltinLine(fullLineToSave)
		if roomID == "" {
			continue
		}
		if platformName == "" {
			platformName = d.Platform
		}
		setBuiltinTaskFlags(platformName, roomID, addFlags)
		key := platformName + "_" + roomID

		if customName != "" {
			builtinCustomNames.Store(key, customName)
		}

		if _, exists := builtinActiveTasks.Load(key); exists {
			duplicateCount++
			continue
		}

		var p BuiltinPlatform
		switch platformName {
		case "Douyin":
			p = &DouyinBuiltinPlatform{}
		case "Kuaishou":
			p = &KuaishouBuiltinPlatform{}
		case "Soop":
			p = &SoopBuiltinPlatform{}
		default:
			continue
		}

		syncBuiltinAnchorToTxt("add", platformName, roomID, fullLineToSave)

		displayName := customName
		if displayName == "" {
			displayName = roomID
		}
		if isP {
			builtinTaskStates.Store(key, "paused")
			updateBuiltinStatus(platformName, roomID, displayName, "", builtinConfig.Quality, "已暂停")
		} else {
			updateBuiltinStatus(platformName, roomID, displayName, "", builtinConfig.Quality, "初始化中")
			wrapperStartMonitorIfNotRunning(p, roomID)
		}
		addedCount++
	}

	triggerBuiltinBroadcast()

	if addedCount == 0 && duplicateCount > 0 {
		hookJSONErr(w, r, http.StatusBadRequest, "该主播/直播间已存在于列表中，请勿重复添加！")
		return
	}

	hookJSONOK(w, r, nil)
}

// apiRecorderControl 为列表里的单条项目指派状态机动作（恢复监控、挂起监控、完全剔除、设置录屏/截屏开关等）
func apiRecorderControl(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action     string `json:"action"`
		Platform   string `json:"platform"`
		RoomID     string `json:"room_id"`
		Record     *bool  `json:"record"`
		Screenshot *bool  `json:"screenshot"`
	}

	if err := hookParseEncrypted(r, &req); err != nil {
		hookJSONErr(w, r, http.StatusBadRequest, "商业安全网关拦截: 非法操作实体或解密异常")
		return
	}

	key := req.Platform + "_" + req.RoomID
	switch req.Action {
	case "set_flags":
		prev := getBuiltinTaskFlags(req.Platform, req.RoomID)
		cur := prev
		if req.Record != nil {
			cur.Record = *req.Record
		}
		if req.Screenshot != nil {
			cur.Screenshot = *req.Screenshot
		}
		changed := cur.Record != prev.Record || cur.Screenshot != prev.Screenshot
		setBuiltinTaskFlags(req.Platform, req.RoomID, cur)
		persistBuiltinFlagsToTxt(req.Platform, req.RoomID, cur)
		if existing, ok := builtinStatusMap.Load(key); ok {
			task := *(existing.(*BuiltinTaskStatus))
			task.Record = cur.Record
			task.Screenshot = cur.Screenshot
			builtinStatusMap.Store(key, &task)
		}
		// 仅当开关实际变化且任务在跑时才取消，避免无意义重启造成断流
		if changed {
			if cancel, ok := builtinCancels.Load(key); ok {
				if state, _ := builtinTaskStates.Load(key); state == "running" {
					cancel.(context.CancelFunc)()
				}
			}
		}
	case "pause":
		builtinTaskStates.Store(key, "paused")
		if cancel, ok := builtinCancels.Load(key); ok {
			cancel.(context.CancelFunc)()
		}
		syncBuiltinAnchorToTxt("pause", req.Platform, req.RoomID, "")
		if existing, ok := builtinStatusMap.Load(key); ok {
			// ✨ 优化：值拷贝避免读写竞争引发脏数据导致前台乱跳
			task := *(existing.(*BuiltinTaskStatus))
			task.IsPaused = true
			task.Status = "已暂停"
			builtinStatusMap.Store(key, &task)
		}
	case "resume":
		builtinTaskStates.Store(key, "running")
		syncBuiltinAnchorToTxt("resume", req.Platform, req.RoomID, "")
		if existing, ok := builtinStatusMap.Load(key); ok {
			// ✨ 优化：值拷贝避免并发竞争
			task := *(existing.(*BuiltinTaskStatus))
			task.IsPaused = false
			task.Status = "监控中"
			builtinStatusMap.Store(key, &task)
		}
		var p BuiltinPlatform
		switch req.Platform {
		case "Douyin":
			p = &DouyinBuiltinPlatform{}
		case "Kuaishou":
			p = &KuaishouBuiltinPlatform{}
		case "Soop":
			p = &SoopBuiltinPlatform{}
		}
		if p != nil {
			wrapperStartMonitorIfNotRunning(p, req.RoomID)
		}
	case "delete":
		builtinTaskStates.Store(key, "deleted")
		if cancel, ok := builtinCancels.Load(key); ok {
			cancel.(context.CancelFunc)()
		}
		syncBuiltinAnchorToTxt("delete", req.Platform, req.RoomID, "")
		builtinStatusMap.Delete(key)
		builtinActiveTasks.Delete(key)
	}
	triggerBuiltinBroadcast()
	hookJSONOK(w, r, nil)
}

// apiRecorderControlAll 执行对当前用户记录中的全部任务群发起全局同步的批量管控状态更新
func apiRecorderControlAll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}

	if err := hookParseEncrypted(r, &req); err != nil {
		hookJSONErr(w, r, http.StatusBadRequest, "商业安全网关拦截: 非法全局操作实体或解密异常")
		return
	}

	builtinAnchorLinesMutex.Lock()
	content, err := os.ReadFile("builtin_urls.txt")
	if err == nil {
		lines := strings.Split(string(content), "\n")
		var newLines []string
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if req.Action == "pause_all" {
				if !strings.HasPrefix(trimmed, "#") {
					newLines = append(newLines, "#"+trimmed)
				} else {
					newLines = append(newLines, trimmed)
				}
			} else if req.Action == "resume_all" {
				if strings.HasPrefix(trimmed, "#") {
					newLines = append(newLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "#")))
				} else {
					newLines = append(newLines, trimmed)
				}
			}
		}
		os.WriteFile("builtin_urls.txt", []byte(strings.Join(newLines, "\n")+"\n"), 0644)
	}
	builtinAnchorLinesMutex.Unlock()

	builtinStatusMap.Range(func(key, value interface{}) bool {
		task := value.(*BuiltinTaskStatus)
		parts := strings.SplitN(key.(string), "_", 2)
		if len(parts) != 2 {
			return true
		}
		platform, roomID := parts[0], parts[1]

		if req.Action == "pause_all" {
			builtinTaskStates.Store(key, "paused")
			if cancel, ok := builtinCancels.Load(key); ok {
				cancel.(context.CancelFunc)()
			}
			// ✨ 优化：进行安全拷贝，防止遍历过程中的指针脏写
			taskVal := *(task)
			taskVal.IsPaused = true
			taskVal.Status = "已暂停"
			builtinStatusMap.Store(key, &taskVal)
		} else if req.Action == "resume_all" {
			builtinTaskStates.Store(key, "running")
			// ✨ 优化：安全值拷贝
			taskVal := *(task)
			taskVal.IsPaused = false
			taskVal.Status = "监控中"
			builtinStatusMap.Store(key, &taskVal)
			var p BuiltinPlatform
			switch platform {
			case "Douyin":
				p = &DouyinBuiltinPlatform{}
			case "Kuaishou":
				p = &KuaishouBuiltinPlatform{}
			case "Soop":
				p = &SoopBuiltinPlatform{}
			}
			if p != nil {
				wrapperStartMonitorIfNotRunning(p, roomID)
			}
		}
		return true
	})

	triggerBuiltinBroadcast()
	hookJSONOK(w, r, nil)
}
