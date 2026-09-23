package recorder

import (
	"context"
	"fmt"
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

		prev := Config()
		// 视频水印决定 ffmpeg 是否重编码，只在进程启动时生效
		wmEnableChanged := prev.VideoWatermarkEnable != c.VideoWatermarkEnable

		// 复制-修改-原子发布：避免与监控协程的并发读产生数据竞争
		UpdateConfig(func(b *BuiltinConfig) {
			if c.Quality != "" {
				b.Quality = c.Quality
			}
			b.SegmentTime = c.SegmentTime
			if c.SavePath != "" {
				b.SavePath = c.SavePath
			}
			if c.ScreenshotInterval > 0 {
				b.ScreenshotInterval = c.ScreenshotInterval
			}
			b.WatermarkEnable = c.WatermarkEnable
			b.VideoWatermarkEnable = c.VideoWatermarkEnable
			b.WatermarkText = c.WatermarkText
			b.WatermarkFormat = c.WatermarkFormat
			b.WatermarkPosition = c.WatermarkPosition
			if c.WatermarkFontSize > 0 {
				b.WatermarkFontSize = c.WatermarkFontSize
			}
			if c.WatermarkFontColor != "" {
				b.WatermarkFontColor = c.WatermarkFontColor
			}
			// 高光切片参数全部是离线后处理用的，不影响录制会话，直接赋值即可。
			// 权重用「两者同时为 0 视为未提交」判定，这样 0 仍可作为合法值提交
			//（只保留单因子）；其余数值项的 0 都不是合法取值，用 > 0 判定。
			b.HighlightEnable = c.HighlightEnable
			if c.HighlightMotionW > 0 || c.HighlightAudioW > 0 {
				b.HighlightMotionW = c.HighlightMotionW
				b.HighlightAudioW = c.HighlightAudioW
			}
			if c.HighlightThreshold > 0 {
				b.HighlightThreshold = c.HighlightThreshold
			}
			if c.HighlightMinDur > 0 {
				b.HighlightMinDur = c.HighlightMinDur
			}
			if c.HighlightMaxDur > 0 {
				b.HighlightMaxDur = c.HighlightMaxDur
			}
			if c.HighlightPerClip > 0 {
				b.HighlightPerClip = c.HighlightPerClip
			}
			if c.HighlightMergeGap > 0 {
				b.HighlightMergeGap = c.HighlightMergeGap
			}
			b.HighlightOnlyUpload = c.HighlightOnlyUpload
		})

		if err := PersistConfig(); err != nil {
			log.Printf("[BUILTIN] ⚠️ 内置配置落盘失败: %v", err)
		}

		// 开关变化时取消正在录的会话，监控循环按新配置重开（无需 systemctl restart）。
		if wmEnableChanged {
			log.Printf("[BUILTIN] 🎬 视频水印开关已切换为 %v，正在按新配置重开录制会话…", c.VideoWatermarkEnable)
			RestartActiveRecordings()
		}

		hookJSONOK(w, r, nil)
		return
	}
	// 设置面板不下发 Cookie，避免鉴权串外泄
	resp := *Config()
	resp.Cookies = BuiltinCookieConfig{}
	hookJSONOK(w, r, resp)
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
		if builtinCookies == nil {
			builtinCookies = &BuiltinCookieConfig{}
		}
		builtinCookies.Douyin = c.Douyin
		builtinCookies.Kuaishou = c.Kuaishou
		builtinCookies.Soop = c.Soop
		builtinCookies.Bilibili = c.Bilibili
		builtinCookies.Twitch = c.Twitch
		ck := *builtinCookies
		builtinCookieMutex.Unlock()

		UpdateConfig(func(b *BuiltinConfig) { b.Cookies = ck })
		if err := PersistConfig(); err != nil {
			log.Printf("[BUILTIN] ⚠️ Cookie 落盘失败: %v", err)
		}
		hookJSONOK(w, r, nil)
		return
	}
	builtinCookieMutex.RLock()
	ck := BuiltinCookieConfig{}
	if builtinCookies != nil {
		ck = *builtinCookies
	}
	builtinCookieMutex.RUnlock()
	hookJSONOK(w, r, ck)
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

		shortURLRe := regexp.MustCompile(`https?://v\.douyin\.com/[a-zA-Z0-9\-_]+/?`)
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

		b23URLRe := regexp.MustCompile(`https?://b23\.tv/[a-zA-Z0-9\-_/]+`)
		if b23URLRe.MatchString(line) {
			log.Printf("[BUILTIN] 检测到B站短链接，正在解析: %s", line)
			realURL, err := ExtractBuiltinBilibiliShortURL(line)
			if err == nil && realURL != "" {
				log.Printf("[BUILTIN] ✅ 最终解析成功: %s", realURL)
				line = realURL
			} else {
				// B站短链解不出房间号即无法监控（视频/主页链接不存在“开播后再换算”），
				// 明确拒绝入库，避免存下永远探测不到的垃圾任务
				log.Printf("[BUILTIN] ❌ B站短链接解析失败: %v", err)
				hookJSONErr(w, r, http.StatusBadRequest, fmt.Sprintf("B站短链接解析失败: %v", err))
				return
			}
		}

		if idx := strings.Index(line, "?"); idx != -1 {
			line = line[:idx]
		}
		line = strings.TrimSuffix(line, "/")

		// 抖音房间号归一：解析引擎产出的 room_id / 各链接形态统一换算成标准 web_rid 再入库，
		// 名单里落干净的 live.douyin.com/<web_rid> 行，探测层不再需要反复兜底换算
		if strings.Contains(line, "douyin") {
			if webRid := resolveDouyinWebRid(line); webRid != line && isAllDigits(webRid) {
				log.Printf("[BUILTIN] 📥 添加归一: %s → https://live.douyin.com/%s", line, webRid)
				line = "https://live.douyin.com/" + webRid
			}
		}

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

		p := NewBuiltinPlatform(platformName)
		if p == nil {
			continue
		}

		syncBuiltinAnchorToTxt("add", platformName, roomID, fullLineToSave)

		displayName := customName
		if displayName == "" {
			displayName = roomID
		}
		if isP {
			builtinTaskStates.Store(key, "paused")
			updateBuiltinStatus(platformName, roomID, displayName, "", Config().Quality, "已暂停")
		} else {
			updateBuiltinStatus(platformName, roomID, displayName, "", Config().Quality, "初始化中")
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
		Action        string  `json:"action"`
		Platform      string  `json:"platform"`
		RoomID        string  `json:"room_id"`
		Record        *bool   `json:"record"`
		Screenshot    *bool   `json:"screenshot"`
		ShotInterval  *int    `json:"shot_interval"`
		Watermark     *int    `json:"watermark"`      // 0=跟随全局 1=强制开 2=强制关
		Quality       *string `json:"quality"`        // ""=跟随全局 uhd/hd/sd
		MaxDuration   *int    `json:"max_duration"`   // 单场最长录制时长（分钟），0=不限制
		SegmentTime   *int    `json:"segment_time"`   // 单主播切片时长（分钟），0=跟随全局
		Window        *string `json:"window"`         // 录制时段 "HH:MM-HH:MM"，""=全天
		Highlight     *int    `json:"highlight"`      // 0=跟随全局 1=强制开 2=强制关（离线后处理）
		HighlightOnly *int    `json:"highlight_only"` // 0=跟随全局 1=只传高光 2=原片与高光都传
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
		if req.ShotInterval != nil && *req.ShotInterval >= 0 {
			cur.ShotInterval = *req.ShotInterval
		}
		if req.Watermark != nil && *req.Watermark >= 0 && *req.Watermark <= 2 {
			cur.Watermark = *req.Watermark
		}
		if req.Quality != nil {
			v := strings.TrimSpace(*req.Quality)
			if v == "" || builtinQualityCodes[v] {
				cur.Quality = v
			}
		}
		if req.MaxDuration != nil && *req.MaxDuration >= 0 {
			cur.MaxDuration = *req.MaxDuration
		}
		if req.SegmentTime != nil && *req.SegmentTime >= 0 {
			cur.SegmentTime = *req.SegmentTime
		}
		if req.Window != nil {
			v := strings.TrimSpace(*req.Window)
			if v == "" {
				cur.Window = ""
			} else if _, _, ok := parseRecordWindow(v); ok {
				cur.Window = v
			}
		}
		if req.Highlight != nil && *req.Highlight >= 0 && *req.Highlight <= 2 {
			cur.Highlight = *req.Highlight
		}
		if req.HighlightOnly != nil && *req.HighlightOnly >= 0 && *req.HighlightOnly <= 2 {
			cur.HighlightOnly = *req.HighlightOnly
		}
		// 录屏/截屏开关变化需要重开会话；截图间隔、截图水印、最长录制时长与高光
		// 由抽帧循环/录制巡检/离线后处理热读取，无需断流；画质决定拉流档位，
		// 切片时长决定 ffmpeg 分段参数，两者都只在进程启动时生效，须重开会话
		modeChanged := cur.Record != prev.Record || cur.Screenshot != prev.Screenshot
		// 视频水印与拉流画质只在 ffmpeg 启动时生效：变化且正在录屏时须重开会话
		wmChanged := cur.Watermark != prev.Watermark
		qualityChanged := cur.Quality != prev.Quality
		segmentChanged := cur.SegmentTime != prev.SegmentTime
		setBuiltinTaskFlags(req.Platform, req.RoomID, cur)
		persistBuiltinFlagsToTxt(req.Platform, req.RoomID, cur)
		if existing, ok := builtinStatusMap.Load(key); ok {
			task := *(existing.(*BuiltinTaskStatus))
			task.Record = cur.Record
			task.Screenshot = cur.Screenshot
			task.ShotInterval = cur.ShotInterval
			task.Watermark = cur.Watermark
			task.QualityOverride = cur.Quality
			task.MaxDuration = cur.MaxDuration
			task.SegmentTime = cur.SegmentTime
			task.Window = cur.Window
			task.Highlight = cur.Highlight
			task.HighlightOnly = cur.HighlightOnly
			builtinStatusMap.Store(key, &task)
		}
		if wmChanged {
			log.Printf("[BUILTIN] 🎬 主播 %s（%s）水印覆盖已切换为 %d，按需重开会话…", req.Platform, req.RoomID, cur.Watermark)
		}
		if qualityChanged {
			log.Printf("[BUILTIN] 🎚️ 主播 %s（%s）画质覆盖已切换为 %q，按需重开会话…", req.Platform, req.RoomID, cur.Quality)
		}
		if segmentChanged {
			if cur.SegmentTime > 0 {
				log.Printf("[BUILTIN] ✂️ 主播 %s（%s）切片时长已覆盖为 %d 分钟，按需重开会话…", req.Platform, req.RoomID, cur.SegmentTime)
			} else {
				log.Printf("[BUILTIN] ✂️ 主播 %s（%s）切片时长已恢复跟随全局，按需重开会话…", req.Platform, req.RoomID)
			}
		}
		if cur.MaxDuration != prev.MaxDuration {
			log.Printf("[BUILTIN] ⏱️ 主播 %s（%s）最长录制时长已切换为 %d 分钟（热生效，无需重开）", req.Platform, req.RoomID, cur.MaxDuration)
		}
		if cur.Window != prev.Window {
			log.Printf("[BUILTIN] ⏰ 主播 %s（%s）录制时段已切换为 %q（热生效，无需重开）", req.Platform, req.RoomID, cur.Window)
		}
		if cur.Highlight != prev.Highlight {
			log.Printf("[BUILTIN] ✨ 主播 %s（%s）高光切片已切换为 %d（离线后处理，热生效）", req.Platform, req.RoomID, cur.Highlight)
		}
		if cur.HighlightOnly != prev.HighlightOnly {
			log.Printf("[BUILTIN] 📤 主播 %s（%s）只传高光已切换为 %d（上传策略，热生效）", req.Platform, req.RoomID, cur.HighlightOnly)
		}
		// 录屏/截屏/画质/水印/切片实际变化 → 直接取消；纯水印或画质变化 → 先打配置重载标记
		//（让监控循环知道这是配置重开、不是断流，避免误报下播）
		if modeChanged || wmChanged || qualityChanged || segmentChanged {
			if !modeChanged {
				markConfigRestart(key)
			}
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
			task.Status = resumeStatusAfterUnpause(task.Status)
			builtinStatusMap.Store(key, &task)
		}
		if p := NewBuiltinPlatform(req.Platform); p != nil {
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
		clearBuiltinDebounce(key)
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
			taskVal.Status = resumeStatusAfterUnpause(taskVal.Status)
			builtinStatusMap.Store(key, &taskVal)
			if p := NewBuiltinPlatform(platform); p != nil {
				wrapperStartMonitorIfNotRunning(p, roomID)
			}
		}
		return true
	})

	triggerBuiltinBroadcast()
	hookJSONOK(w, r, nil)
}
