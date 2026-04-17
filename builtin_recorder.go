package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp" // ✨ 引入无头浏览器库
)

// ==========================================
// 内置录制引擎模块 (Built-in Recorder)
// ==========================================

var builtinFfmpegPath string = "ffmpeg"

var builtinHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	},
}

type BuiltinTaskStatus struct {
	Platform   string `json:"platform"`
	RoomID     string `json:"room_id"`
	AnchorName string `json:"anchor_name"`
	Avatar     string `json:"avatar"`
	Quality    string `json:"quality"`
	Status     string `json:"status"`
	UpdateTime string `json:"update_time"`
	IsPaused   bool   `json:"is_paused"`
	FileSize   string `json:"file_size"`
	Duration   string `json:"duration"`

	startTime time.Time `json:"-"`
}

var (
	builtinConfig      *BuiltinConfig
	builtinActiveTasks sync.Map
	builtinStatusMap   sync.Map
	builtinCookies     *BuiltinCookieConfig
	builtinCookieMutex sync.RWMutex

	builtinTaskStates  sync.Map // key: platform_roomID, value: "running", "paused", "deleted"
	builtinCancels     sync.Map // key: platform_roomID, value: context.CancelFunc
	builtinCustomNames sync.Map // 内存中保存的自定义名称 (由 txt 提供)

	// ✨ 添加全局防抖缓冲池：彻底消除由于网络颠簸引起的 FFmpeg 开播/下播反复横跳现象
	builtinNotifyDebounce sync.Map
)

var builtinAnchorLinesMutex sync.Mutex

type BuiltinConfig struct {
	Quality       string `json:"quality"`
	SegmentTime   int    `json:"segment_time"`
	CheckInterval int    `json:"check_interval"`
	SavePath      string `json:"save_path"`
}

type BuiltinCookieConfig struct {
	Douyin   string `json:"douyin"`
	Kuaishou string `json:"kuaishou"`
	Soop     string `json:"soop"`
}

type BuiltinPlatform interface {
	GetPlatformName() string
	GetStreamURL(roomID string, quality string) (streamURL string, anchorName string, avatar string, err error)
}

// triggerBuiltinBroadcast 触发内置引擎任务状态列表向前端加密 WebSocket 信道的全量广播
func triggerBuiltinBroadcast() {
	broadcastWS("builtinTasks", GetBuiltinRecorderTasks())
}

// updateBuiltinStatus 更新指定内置引擎任务的内存运行状态，并识别录制状态变更以触发广播与微信通知
func updateBuiltinStatus(platform, roomID, anchorName, avatar, quality, statusMsg string) {
	key := platform + "_" + roomID
	now := time.Now()
	var sTime time.Time

	isNewlyRecording := false

	if existing, ok := builtinStatusMap.Load(key); ok {
		oldTask := existing.(*BuiltinTaskStatus)
		if anchorName == "" || anchorName == roomID {
			anchorName = oldTask.AnchorName
		}
		if avatar == "" {
			avatar = oldTask.Avatar
		}

		if statusMsg == "录制中" {
			if oldTask.Status != "录制中" {
				// ✨ 防抖判定：确保两次相同的【开播通知】之间至少缓冲 3 分钟，否则静默恢复时间戳
				cacheKey := "live_" + key
				if last, has := builtinNotifyDebounce.Load(cacheKey); !has || time.Since(last.(time.Time)) > 3*time.Minute {
					builtinNotifyDebounce.Store(cacheKey, now)
					sTime = now
					isNewlyRecording = true
					sendWeChatNotify("开播通知", fmt.Sprintf("检测到平台 [%s] 的主播 [%s] 开始直播并已成功接管录制！", platform, anchorName))
				} else {
					sTime = oldTask.startTime // 静默无感知恢复推流，避免惊扰管理员
				}
			} else {
				sTime = oldTask.startTime
			}
		} else if statusMsg == "未开播等待中" || statusMsg == "断流缓冲中" || statusMsg == "已暂停" {
			if oldTask.Status == "录制中" {
				// ✨ 防抖判定：防下播通知连发
				cacheKey := "offline_" + key
				if last, has := builtinNotifyDebounce.Load(cacheKey); !has || time.Since(last.(time.Time)) > 3*time.Minute {
					builtinNotifyDebounce.Store(cacheKey, now)
					sendWeChatNotify("下播通知", fmt.Sprintf("检测到平台 [%s] 的主播 [%s] 已经下播或断流停止录制！", platform, anchorName))
				}
			}
		}
	} else {
		if statusMsg == "录制中" {
			sTime = now
			isNewlyRecording = true
			builtinNotifyDebounce.Store("live_"+key, now)
			sendWeChatNotify("开播通知", fmt.Sprintf("检测到平台 [%s] 的主播 [%s] 开始直播并已成功接管录制！", platform, anchorName))
		}
	}

	if anchorName == "" {
		anchorName = roomID
	}

	state, _ := builtinTaskStates.Load(key)
	isPaused := state == "paused"
	if isPaused {
		statusMsg = "已暂停"
	}

	builtinStatusMap.Store(key, &BuiltinTaskStatus{
		Platform:   platform,
		RoomID:     roomID,
		AnchorName: anchorName,
		Avatar:     avatar,
		Quality:    quality,
		Status:     statusMsg,
		UpdateTime: time.Now().Format("2006-01-02 15:04:05"),
		IsPaused:   isPaused,
		startTime:  sTime,
	})

	triggerBuiltinBroadcast()

	if isNewlyRecording && !isPaused {
		go func() {
			time.Sleep(2500 * time.Millisecond)
			triggerScan(fmt.Sprintf("内置引擎捕获[%s]开播", anchorName))
		}()
	}
}

// updateBuiltinNameInTxt 将新解析到的主播自定义名称同步持久化更新至本地的名单文件中
func updateBuiltinNameInTxt(platform, roomID, anchorName string) {
	builtinAnchorLinesMutex.Lock()
	defer builtinAnchorLinesMutex.Unlock()

	content, err := os.ReadFile("builtin_urls.txt")
	if err != nil {
		return
	}

	lines := strings.Split(string(content), "\n")
	changed := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		isP, p, rid, customName, rawURL := parseBuiltinLine(trimmed)
		if p == platform && rid == roomID {
			if customName != anchorName && anchorName != "" && anchorName != roomID {
				prefix := ""
				if isP {
					prefix = "#"
				}
				safeName := strings.ReplaceAll(anchorName, "\n", "")
				safeName = strings.ReplaceAll(safeName, "\r", "")
				lines[i] = fmt.Sprintf("%s%s,主播:%s", prefix, rawURL, safeName)
				changed = true
			}
		}
	}

	if changed {
		os.WriteFile("builtin_urls.txt", []byte(strings.Join(lines, "\n")+"\n"), 0644)
	}
}

// builtinHotReloadLoop 后台热重载守护协程，定时检测 builtin_urls.txt 的修改动态启停监控任务
func builtinHotReloadLoop() {
	var lastModTime time.Time
	for {
		time.Sleep(3 * time.Second)
		info, err := os.Stat("builtin_urls.txt")
		if err != nil {
			continue
		}

		if lastModTime.IsZero() {
			lastModTime = info.ModTime()
			continue
		}

		if info.ModTime().After(lastModTime) {
			lastModTime = info.ModTime()

			builtinAnchorLinesMutex.Lock()
			content, err := os.ReadFile("builtin_urls.txt")
			builtinAnchorLinesMutex.Unlock()

			if err != nil {
				continue
			}

			lines := strings.Split(string(content), "\n")
			currentKeys := make(map[string]bool)
			stateChanged := false

			for _, line := range lines {
				isPaused, platformName, roomID, customName, _ := parseBuiltinLine(line)
				if roomID == "" || platformName == "" {
					continue
				}
				key := platformName + "_" + roomID
				currentKeys[key] = true

				if customName != "" {
					builtinCustomNames.Store(key, customName)
				}

				state, exists := builtinTaskStates.Load(key)

				if !exists {
					stateChanged = true
					var p BuiltinPlatform
					switch platformName {
					case "Douyin":
						p = &DouyinBuiltinPlatform{}
					case "Kuaishou":
						p = &KuaishouBuiltinPlatform{}
					case "Soop":
						p = &SoopBuiltinPlatform{}
					}

					displayName := customName
					if displayName == "" {
						displayName = roomID
					}

					if isPaused {
						builtinTaskStates.Store(key, "paused")
						updateBuiltinStatus(platformName, roomID, displayName, "", builtinConfig.Quality, "已暂停")
					} else {
						updateBuiltinStatus(platformName, roomID, displayName, "", builtinConfig.Quality, "初始化中")
						if p != nil {
							wrapperStartMonitorIfNotRunning(p, roomID)
						}
					}
				} else {
					if isPaused && state == "running" {
						stateChanged = true
						builtinTaskStates.Store(key, "paused")
						if cancel, ok := builtinCancels.Load(key); ok {
							cancel.(context.CancelFunc)()
						}
						if existingTask, ok := builtinStatusMap.Load(key); ok {
							task := existingTask.(*BuiltinTaskStatus)
							task.IsPaused = true
							task.Status = "已暂停"
							builtinStatusMap.Store(key, task)
						}
					} else if !isPaused && state == "paused" {
						stateChanged = true
						builtinTaskStates.Store(key, "running")
						if existingTask, ok := builtinStatusMap.Load(key); ok {
							task := existingTask.(*BuiltinTaskStatus)
							task.IsPaused = false
							task.Status = "监控中"
							builtinStatusMap.Store(key, task)
						}
						var p BuiltinPlatform
						switch platformName {
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
				}
			}

			builtinTaskStates.Range(func(k, v interface{}) bool {
				key := k.(string)
				if _, found := currentKeys[key]; !found {
					if v.(string) != "deleted" {
						stateChanged = true
						builtinTaskStates.Store(key, "deleted")
						if cancel, ok := builtinCancels.Load(key); ok {
							cancel.(context.CancelFunc)()
						}
						builtinStatusMap.Delete(key)
						builtinActiveTasks.Delete(key)
					}
				}
				return true
			})

			if stateChanged {
				log.Println("[BUILTIN] 🔄 检测到底层监控文件发生变化，已热重载并同步至所有设备！")
				triggerBuiltinBroadcast()
			}
		}
	}
}

// InitBuiltinRecorder 初始化内置录制引擎模块，挂载相关 API 路由并启动系统常驻协程
func InitBuiltinRecorder(mux *http.ServeMux) {
	checkFFmpegBuiltin()

	if _, err := os.Stat("builtin_config.json"); os.IsNotExist(err) {
		builtinConfig = &BuiltinConfig{Quality: "uhd", CheckInterval: 30, SavePath: "./downloads"}
		data, _ := json.MarshalIndent(builtinConfig, "", "    ")
		os.WriteFile("builtin_config.json", data, 0644)
	} else {
		d, _ := os.ReadFile("builtin_config.json")
		builtinConfig = &BuiltinConfig{}
		json.Unmarshal(d, builtinConfig)
	}

	if builtinConfig.CheckInterval == 0 {
		builtinConfig.CheckInterval = 30
	}
	if builtinConfig.SavePath == "" {
		builtinConfig.SavePath = "./downloads"
	}

	if _, err := os.Stat("builtin_cookies.json"); os.IsNotExist(err) {
		builtinCookies = &BuiltinCookieConfig{}
		data, _ := json.MarshalIndent(builtinCookies, "", "    ")
		os.WriteFile("builtin_cookies.json", data, 0644)
	} else {
		d, _ := os.ReadFile("builtin_cookies.json")
		builtinCookies = &BuiltinCookieConfig{}
		json.Unmarshal(d, builtinCookies)
	}

	if _, err := os.Stat("builtin_urls.txt"); os.IsNotExist(err) {
		os.WriteFile("builtin_urls.txt", []byte(""), 0644)
	} else {
		content, _ := os.ReadFile("builtin_urls.txt")
		lines := strings.Split(string(content), "\n")
		for _, line := range lines {
			isPaused, platform, roomID, customName, _ := parseBuiltinLine(line)
			if roomID == "" || platform == "" {
				continue
			}
			key := platform + "_" + roomID
			if customName != "" {
				builtinCustomNames.Store(key, customName)
			}
			var p BuiltinPlatform
			switch platform {
			case "Douyin":
				p = &DouyinBuiltinPlatform{}
			case "Kuaishou":
				p = &KuaishouBuiltinPlatform{}
			case "Soop":
				p = &SoopBuiltinPlatform{}
			default:
				continue
			}

			if isPaused {
				builtinTaskStates.Store(key, "paused")
				displayName := customName
				if displayName == "" {
					displayName = roomID
				}
				updateBuiltinStatus(platform, roomID, displayName, "", builtinConfig.Quality, "已暂停")
			} else {
				displayName := customName
				if displayName == "" {
					displayName = roomID
				}
				updateBuiltinStatus(platform, roomID, displayName, "", builtinConfig.Quality, "初始化中")
				wrapperStartMonitorIfNotRunning(p, roomID)
			}
		}
	}

	os.MkdirAll("./covers", os.ModePerm)
	mux.Handle("/covers/", http.StripPrefix("/covers/", http.FileServer(http.Dir("./covers"))))

	mux.HandleFunc("/api/v1/builtin_recorder/proxy_image", apiProxyImage)
	mux.HandleFunc("/api/v1/builtin_recorder/config", apiRecorderConfig)
	mux.HandleFunc("/api/v1/builtin_recorder/cookies", apiRecorderCookies)
	mux.HandleFunc("/api/v1/builtin_recorder/add", apiRecorderAdd)
	mux.HandleFunc("/api/v1/builtin_recorder/control", apiRecorderControl)
	mux.HandleFunc("/api/v1/builtin_recorder/control_all", apiRecorderControlAll)

	log.Println("[BUILTIN] 🎥 内置轻量录制引擎已成功挂载！")

	go builtinHotReloadLoop()
}

// GetBuiltinRecorderTasks 获取当前内存中所有的内置引擎任务运行快照以提供给前端界面
func GetBuiltinRecorderTasks() []BuiltinTaskStatus {
	var list []BuiltinTaskStatus
	builtinStatusMap.Range(func(key, value interface{}) bool {
		task := *value.(*BuiltinTaskStatus)
		if task.Status == "录制中" && !task.startTime.IsZero() {
			task.Duration = formatBuiltinDuration(time.Since(task.startTime))
		} else {
			task.Duration = "-"
		}
		safeName := sanitizeBuiltinFileName(task.AnchorName)
		if safeName == "" {
			safeName = task.RoomID
		}
		baseDir := builtinConfig.SavePath
		targetDir := filepath.Join(baseDir, safeName)
		task.FileSize = getBuiltinDirSizeStr(targetDir)
		list = append(list, task)
		return true
	})

	// 【极致优化】：修复 Go map 遍历无序导致的乱序乱跳问题，按平台和房间号稳定排序
	sort.Slice(list, func(i, j int) bool {
		return list[i].Platform+"_"+list[i].RoomID < list[j].Platform+"_"+list[j].RoomID
	})

	return list
}

// ==========================================
// 辅助工具函数
// ==========================================

// checkFFmpegBuiltin 探测系统环境内是否有可用的 ffmpeg，作为推流数据解包的核心依赖
func checkFFmpegBuiltin() {
	localPath := filepath.Join(".", "ffmpeg.exe")
	if _, err := os.Stat(localPath); err == nil {
		absPath, _ := filepath.Abs(localPath)
		builtinFfmpegPath = absPath
		log.Printf("[BUILTIN] ✅ 成功加载本地 ffmpeg: %s\n", builtinFfmpegPath)
		return
	}
	path, err := exec.LookPath("ffmpeg")
	if err == nil {
		builtinFfmpegPath = path
		log.Printf("[BUILTIN] ✅ 成功加载系统环境变量中的 ffmpeg: %s\n", builtinFfmpegPath)
	} else {
		log.Println("[BUILTIN] ❌ 未找到 ffmpeg！内置录制功能将无法正常工作，请安装 ffmpeg 并配置环境变量！")
	}
}

// extractBuiltinRoomID 从各类直播间 URL 中提取出统一格式的纯净房间 ID
func extractBuiltinRoomID(input string) string {
	input = strings.TrimSpace(input)
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		u, err := url.Parse(input)
		if err == nil {
			path := strings.Trim(u.Path, "/")
			segments := strings.Split(path, "/")

			if strings.Contains(u.Host, "sooplive.co.kr") || strings.Contains(u.Host, "afreecatv.com") || strings.Contains(u.Host, "sooplive.com") {
				if len(segments) > 0 {
					return segments[0]
				}
			}

			if len(segments) > 0 {
				return segments[len(segments)-1]
			}
		}
	}
	return input
}

// sanitizeBuiltinFileName 清洗并规范化主播名称，剔除非法及容易导致操作异常的特殊字符
func sanitizeBuiltinFileName(name string) string {
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\t", "")
	name = strings.ReplaceAll(name, "　", " ")

	invalidChars := []string{"\\", "/", ":", "*", "?", "\"", "<", ">", "|"}
	for _, char := range invalidChars {
		name = strings.ReplaceAll(name, char, "")
	}

	name = strings.TrimSpace(name)
	name = strings.Trim(name, " ._-")

	if name == "" {
		return "未命名主播"
	}

	return name
}

// formatBuiltinDuration 将 Go 时间差对象格式化为 X小时X分X秒 格式
func formatBuiltinDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%02d小时%02d分%02d秒", h, m, s)
	}
	return fmt.Sprintf("%02d分%02d秒", m, s)
}

// getBuiltinDirSizeStr 遍历并计算指定保存目录的总物理文件大小
func getBuiltinDirSizeStr(path string) string {
	var size int64
	err := filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	if err != nil || size == 0 {
		return "0 B"
	}
	return formatBuiltinBytes(size)
}

// formatBuiltinBytes 将庞大的字节数据格式化为易读的 KB/MB/GB 规格字符串
func formatBuiltinBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// formatBuiltinQualityName 映射配置内的画质代码为前端直接展示的中文名称
func formatBuiltinQualityName(quality string) string {
	switch quality {
	case "uhd":
		return "蓝光/超清"
	case "hd":
		return "高清"
	case "sd":
		return "标清"
	default:
		return "未知画质"
	}
}

// parseBuiltinLine 分析本地监控的行数据，提炼平台归属、房间ID及自定义备注名
func parseBuiltinLine(line string) (isPaused bool, platform string, roomID string, customName string, rawURL string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}

	if strings.HasPrefix(line, "#") {
		isPaused = true
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
	}

	if idx := strings.Index(line, ",主播:"); idx != -1 {
		customName = strings.TrimSpace(line[idx+len(",主播:"):])
		rawURL = strings.TrimSpace(line[:idx])
	} else if idx := strings.Index(line, ", 主播:"); idx != -1 {
		customName = strings.TrimSpace(line[idx+len(", 主播:"):])
		rawURL = strings.TrimSpace(line[:idx])
	} else if idx := strings.Index(line, ","); idx != -1 {
		customName = strings.TrimSpace(line[idx+1:])
		rawURL = strings.TrimSpace(line[:idx])
	} else {
		rawURL = line
	}

	if strings.Contains(rawURL, "douyin.com") || strings.Contains(rawURL, "amemv.com") || strings.Contains(rawURL, "iesdouyin.com") || strings.Contains(rawURL, "douyin") {
		platform = "Douyin"
	} else if strings.Contains(rawURL, "kuaishou.com") || strings.Contains(rawURL, "chenzhongtech.com") {
		platform = "Kuaishou"
	} else if strings.Contains(rawURL, "sooplive.co.kr") || strings.Contains(rawURL, "afreecatv.com") || strings.Contains(rawURL, "sooplive.com") {
		platform = "Soop"
	}

	roomID = extractBuiltinRoomID(rawURL)
	return
}

// syncBuiltinAnchorToTxt 依据前端指令对本地配置文件里的内容作增、删、改并落地
func syncBuiltinAnchorToTxt(action string, platform, roomID string, rawLine string) {
	builtinAnchorLinesMutex.Lock()
	defer builtinAnchorLinesMutex.Unlock()

	content, err := os.ReadFile("builtin_urls.txt")
	var lines []string
	if err == nil {
		lines = strings.Split(string(content), "\n")
	}

	var newLines []string
	found := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		isP, p, rid, _, _ := parseBuiltinLine(trimmed)
		if p == platform && rid == roomID {
			found = true
			if action == "delete" {
				continue
			} else if action == "pause" {
				if !isP {
					newLines = append(newLines, "#"+trimmed)
				} else {
					newLines = append(newLines, trimmed)
				}
			} else if action == "resume" {
				if isP {
					newLines = append(newLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "#")))
				} else {
					newLines = append(newLines, trimmed)
				}
			}
		} else {
			newLines = append(newLines, trimmed)
		}
	}

	if !found && action == "add" && rawLine != "" {
		newLines = append(newLines, strings.TrimSpace(rawLine))
	}

	os.WriteFile("builtin_urls.txt", []byte(strings.Join(newLines, "\n")+"\n"), 0644)
}

// ==========================================
// ✨ 新增：抖音短链接无头浏览器深度解析 + HTTP 保底
// ==========================================

// ExtractBuiltinDouyinLiveURL 针对用户输入的短链接，采取无头浏览器和原生HTTP双擎重定向拿取长连接
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
// 核心加密算法复刻 (SM3, RC4, a_bogus)
// ==========================================

// builtinRC4Encrypt 实现标准的 RC4 对称加密方法，用于接口所需的 UserAgent 加密流程
func builtinRC4Encrypt(plaintext, key string) string {
	s := make([]int, 256)
	for i := 0; i < 256; i++ {
		s[i] = i
	}
	j := 0
	for i := 0; i < 256; i++ {
		j = (j + s[i] + int(key[i%len(key)])) % 256
		s[i], s[j] = s[j], s[i]
	}
	i := 0
	j = 0
	res := make([]byte, len(plaintext))
	for k := 0; k < len(plaintext); k++ {
		i = (i + 1) % 256
		j = (j + s[i]) % 256
		s[i], s[j] = s[j], s[i]
		t := (s[i] + s[j]) % 256
		res[k] = byte(int(plaintext[k]) ^ s[t])
	}
	return string(res)
}

// BuiltinSM3 SM3 散列算法结构体定义
type BuiltinSM3 struct {
	reg   []uint32
	chunk []byte
	size  uint64
}

// NewBuiltinSM3 初始化一个满足中国国家密码局算法标准的 SM3 散列计算器
func NewBuiltinSM3() *BuiltinSM3 {
	s := &BuiltinSM3{}
	s.Reset()
	return s
}

// Reset 清空 SM3 计算器的当前上下文状态和内部数据块缓存，重置回初始哈希常量
func (s *BuiltinSM3) Reset() {
	s.reg = []uint32{
		1937774191, 1226093241, 388252375, 3666478592,
		2842636476, 372324522, 3817729613, 2969243214,
	}
	s.chunk = []byte{}
	s.size = 0
}

// leftRotate 执行 SM3 算法中需要的 32 位无符号整型循环左移按位操作
func (s *BuiltinSM3) leftRotate(x uint32, n int) uint32 {
	n &= 0x1f
	if n == 0 {
		return x
	}
	return (x << n) | (x >> (32 - n))
}

// getT 返回对应运算轮次的常数项，属于 SM3 算法标准规范定义的一环
func (s *BuiltinSM3) getT(j int) uint32 {
	if j < 16 {
		return 2043430169
	}
	return 2055708042
}

// ff 执行 SM3 规定的布尔逻辑计算组合函数之一，取决于执行轮次 j 的区间
func (s *BuiltinSM3) ff(j int, x, y, z uint32) uint32 {
	if j < 16 {
		return x ^ y ^ z
	}
	return (x & y) | (x & z) | (y & z)
}

// gg 执行 SM3 规定的布尔逻辑计算组合函数之二，负责后续 48 轮次的数据混淆
func (s *BuiltinSM3) gg(j int, x, y, z uint32) uint32 {
	if j < 16 {
		return x ^ y ^ z
	}
	return (x & y) | (^x & z)
}

// compress 完成对传入的 512 bit (64 Byte) 的单块数据进行消息扩展并注入八位寄存器内
func (s *BuiltinSM3) compress(data []byte) {
	w := make([]uint32, 132)
	for t := 0; t < 16; t++ {
		w[t] = binary.BigEndian.Uint32(data[4*t : 4*t+4])
	}
	for j := 16; j < 68; j++ {
		a := w[j-16] ^ w[j-9] ^ s.leftRotate(w[j-3], 15)
		w[j] = a ^ s.leftRotate(a, 15) ^ s.leftRotate(a, 23) ^ s.leftRotate(w[j-13], 7) ^ w[j-6]
	}
	for j := 0; j < 64; j++ {
		w[j+68] = w[j] ^ w[j+4]
	}
	a, b, c, d, e, f, g, h := s.reg[0], s.reg[1], s.reg[2], s.reg[3], s.reg[4], s.reg[5], s.reg[6], s.reg[7]
	for j := 0; j < 64; j++ {
		ss1 := s.leftRotate((s.leftRotate(a, 12) + e + s.leftRotate(s.getT(j), j)), 7)
		ss2 := ss1 ^ s.leftRotate(a, 12)
		tt1 := s.ff(j, a, b, c) + d + ss2 + w[j+68]
		tt2 := s.gg(j, e, f, g) + h + ss1 + w[j]
		d = c
		c = s.leftRotate(b, 9)
		b = a
		a = tt1
		h = g
		g = s.leftRotate(f, 19)
		f = e
		e = tt2 ^ s.leftRotate(tt2, 9) ^ s.leftRotate(tt2, 17)
	}
	s.reg[0] ^= a
	s.reg[1] ^= b
	s.reg[2] ^= c
	s.reg[3] ^= d
	s.reg[4] ^= e
	s.reg[5] ^= f
	s.reg[6] ^= g
	s.reg[7] ^= h
}

// Write 满足 hash.Hash 接口定义，持续吞并字符串并放入缓存器触发增量解算
func (s *BuiltinSM3) Write(data string) {
	b := []byte(data)
	s.size += uint64(len(b))
	f := 64 - len(s.chunk)
	if len(b) < f {
		s.chunk = append(s.chunk, b...)
	} else {
		s.chunk = append(s.chunk, b[:f]...)
		for len(s.chunk) >= 64 {
			s.compress(s.chunk)
			b = b[f:]
			if len(b) < 64 {
				s.chunk = b
				break
			}
			s.chunk = b[:64]
			f = 64
		}
	}
}

// Sum 封装收尾运算，执行数据填充标准 (Bit 1 随后 0 以及长度字段) 输出 32 字节哈希值
func (s *BuiltinSM3) Sum() []byte {
	bitLength := s.size * 8
	s.chunk = append(s.chunk, 0x80)
	for (len(s.chunk)+8)%64 != 0 {
		s.chunk = append(s.chunk, 0)
	}
	lenBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(lenBytes, bitLength)
	s.chunk = append(s.chunk, lenBytes...)
	for i := 0; i < len(s.chunk); i += 64 {
		s.compress(s.chunk[i : i+64])
	}
	res := make([]byte, 32)
	for i := 0; i < 8; i++ {
		binary.BigEndian.PutUint32(res[4*i:], s.reg[i])
	}
	s.Reset()
	return res
}

// builtinResultEncrypt 依靠逆向取得的前端特制混淆表，对给出的密文数据进行类 Base64 的私有编码替换
func builtinResultEncrypt(longStr, num string) string {
	encodingTables := map[string]string{
		"s0": "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=",
		"s1": "Dkdpgh4ZKsQB80/Mfvw36XI1R25+WUAlEi7NLboqYTOPuzmFjJnryx9HVGcaStCe=",
		"s2": "Dkdpgh4ZKsQB80/Mfvw36XI1R25-WUAlEi7NLboqYTOPuzmFjJnryx9HVGcaStCe=",
		"s3": "ckdp1h4ZKsUB80/Mfvw36XIgR25+WQAlEi7NLboqYTOPuzmFjJnryx9HVGDaStCe",
		"s4": "Dkdpgh2ZmsQB80/MfvV36XI1R45-WUAlEixNLwoqYTOPuzKFjJnry79HbGcaStCe",
	}
	table := encodingTables[num]
	masks := []int{16515072, 258048, 4032, 63}
	shifts := []int{18, 12, 6, 0}
	var res strings.Builder
	roundNum := 0
	getLongInt := func(round int, s string) int {
		idx := round * 3
		var ch1, ch2, ch3 int
		if idx < len(s) {
			ch1 = int(s[idx])
		}
		if idx+1 < len(s) {
			ch2 = int(s[idx+1])
		}
		if idx+2 < len(s) {
			ch3 = int(s[idx+2])
		}
		return (ch1 << 16) | (ch2 << 8) | ch3
	}
	longInt := getLongInt(roundNum, longStr)
	totalChars := int(math.Ceil(float64(len(longStr)) / 3.0 * 4.0))
	for i := 0; i < totalChars; i++ {
		if i/4 != roundNum {
			roundNum++
			longInt = getLongInt(roundNum, longStr)
		}
		index := i % 4
		charIndex := (longInt & masks[index]) >> shifts[index]
		res.WriteByte(table[charIndex])
	}
	return res.String()
}

// generBuiltinRandom 生成请求校验签名时所需的前置随机噪声比特位组合
func generBuiltinRandom(randomNum int, option []int) []int {
	byte1 := randomNum & 255
	byte2 := (randomNum >> 8) & 255
	return []int{
		(byte1 & 170) | (option[0] & 85),
		(byte1 & 85) | (option[0] & 170),
		(byte2 & 170) | (option[1] & 85),
		(byte2 & 85) | (option[1] & 170),
	}
}

// generateBuiltinRandomStr 根据上述的噪声组合规则，通过时间种子转换得到完全不规律的 ASCII 字节前缀序列
func generateBuiltinRandomStr() string {
	r1 := rand.Float64()
	r2 := rand.Float64()
	r3 := rand.Float64()

	var bytes []int
	bytes = append(bytes, generBuiltinRandom(int(r1*10000), []int{3, 45})...)
	bytes = append(bytes, generBuiltinRandom(int(r2*10000), []int{1, 0})...)
	bytes = append(bytes, generBuiltinRandom(int(r3*10000), []int{1, 5})...)

	var sb strings.Builder
	for _, b := range bytes {
		sb.WriteByte(byte(b))
	}
	return sb.String()
}

// builtinGenerateABogus 负责合并各层算法完成 a_bogus 参数的终极推导以突破某音接口抓取封锁限制
func builtinGenerateABogus(params, userAgent string) string {
	windowEnvStr := "1920|1080|1920|1040|0|30|0|0|1872|92|1920|1040|1857|92|1|24|Win32"
	suffix := "cus"
	arguments := []int{0, 1, 14}

	sm3 := NewBuiltinSM3()
	startTime := int(time.Now().UnixNano() / 1e6)

	sm3.Write(params + suffix)
	hash1 := string(sm3.Sum())
	sm3.Write(hash1)
	urlSearchParamsList := sm3.Sum()

	sm3.Write(suffix)
	hash2 := string(sm3.Sum())
	sm3.Write(hash2)
	cus := sm3.Sum()

	uaKey := string([]byte{0, 1, 14})
	uaEnc := builtinRC4Encrypt(userAgent, uaKey)
	uaB64 := builtinResultEncrypt(uaEnc, "s3")
	sm3.Write(uaB64)
	uaHash := sm3.Sum()

	b := make(map[int]int)
	b[8] = 3
	b[10] = startTime + 100
	b[16] = startTime
	b[18] = 44

	splitToBytes := func(num int) []int {
		return []int{(num >> 24) & 255, (num >> 16) & 255, (num >> 8) & 255, num & 255}
	}

	stBytes := splitToBytes(b[16])
	b[20], b[21], b[22], b[23] = stBytes[0], stBytes[1], stBytes[2], stBytes[3]
	b[24] = (b[16] >> 32) & 255
	b[25] = (b[16] >> 40) & 255

	arg0 := splitToBytes(arguments[0])
	b[26], b[27], b[28], b[29] = arg0[0], arg0[1], arg0[2], arg0[3]
	b[30] = (arguments[1] >> 8) & 255
	b[31] = arguments[1] & 255
	arg1 := splitToBytes(arguments[1])
	b[32], b[33] = arg1[0], arg1[1]
	arg2 := splitToBytes(arguments[2])
	b[34], b[35], b[36], b[37] = arg2[0], arg2[1], arg2[2], arg2[3]

	b[38] = int(urlSearchParamsList[21])
	b[39] = int(urlSearchParamsList[22])
	b[40] = int(cus[21])
	b[41] = int(cus[22])
	b[42] = int(uaHash[23])
	b[43] = int(uaHash[24])

	etBytes := splitToBytes(b[10])
	b[44], b[45], b[46], b[47] = etBytes[0], etBytes[1], etBytes[2], etBytes[3]
	b[48] = b[8]
	b[49] = (b[10] >> 32) & 255
	b[50] = (b[10] >> 40) & 255

	pageId := 110624
	b[51] = pageId
	pIdBytes := splitToBytes(pageId)
	b[52], b[53], b[54], b[55] = pIdBytes[0], pIdBytes[1], pIdBytes[2], pIdBytes[3]

	aid := 6383
	b[56] = aid
	b[57] = aid & 255
	b[58] = (aid >> 8) & 255
	b[59] = (aid >> 16) & 255
	b[60] = (aid >> 24) & 255

	winEnvList := []byte(windowEnvStr)
	b[64] = len(winEnvList)
	b[65] = b[64] & 255
	b[66] = (b[64] >> 8) & 255
	b[69], b[70], b[71] = 0, 0, 0

	xorSum := b[18] ^ b[20] ^ b[26] ^ b[30] ^ b[38] ^ b[40] ^ b[42] ^ b[21] ^ b[27] ^ b[31] ^
		b[35] ^ b[39] ^ b[41] ^ b[43] ^ b[22] ^ b[28] ^ b[32] ^ b[36] ^ b[23] ^ b[29] ^
		b[33] ^ b[37] ^ b[44] ^ b[45] ^ b[46] ^ b[47] ^ b[48] ^ b[49] ^ b[50] ^ b[24] ^
		b[25] ^ b[52] ^ b[53] ^ b[54] ^ b[55] ^ b[57] ^ b[58] ^ b[59] ^ b[60] ^ b[65] ^
		b[66] ^ b[70] ^ b[71]
	b[72] = xorSum

	var bb []byte
	indices := []int{
		18, 20, 52, 26, 30, 34, 58, 38, 40, 53, 42, 21,
		27, 54, 55, 31, 35, 57, 39, 41, 43, 22, 28, 32,
		60, 36, 23, 29, 33, 37, 44, 45, 59, 46, 47, 48,
		49, 50, 24, 25, 65, 66, 70, 71,
	}
	for _, idx := range indices {
		bb = append(bb, byte(b[idx]))
	}
	bb = append(bb, winEnvList...)
	bb = append(bb, byte(b[72]))

	prefix := generateBuiltinRandomStr()
	body := builtinRC4Encrypt(string(bb), string([]byte{121}))
	return builtinResultEncrypt(prefix+body, "s4") + "="
}

// ==========================================
// 🚀 平台抓取实现
// ==========================================

// ---------------- Douyin ----------------
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
type SoopBuiltinPlatform struct{}

// GetPlatformName 提供用于逻辑判断及配置索引的 Soop 平台名标识
func (s *SoopBuiltinPlatform) GetPlatformName() string { return "Soop" }

// GetStreamURL 请求外网 Soop (AfreecaTV) 接口获取跨国 CDN 的可用录像推流源及主播信息
func (s *SoopBuiltinPlatform) GetStreamURL(roomID string, quality string) (string, string, string, error) {
	globalApi := fmt.Sprintf("https://api.sooplive.com/v2/stream/info/%s", roomID)
	reqGlobal, err := http.NewRequest("GET", globalApi, nil)

	globalUa := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	if err == nil {
		clientId := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", rand.Uint32(), rand.Uint32()>>16, rand.Uint32()>>16, rand.Uint32()>>16, uint64(rand.Uint32())<<32|uint64(rand.Uint32()))
		reqGlobal.Header.Set("client-id", clientId)
		reqGlobal.Header.Set("User-Agent", globalUa)

		builtinCookieMutex.RLock()
		if builtinCookies.Soop != "" {
			reqGlobal.Header.Set("Cookie", builtinCookies.Soop)
		}
		builtinCookieMutex.RUnlock()

		respGlobal, err := builtinHTTPClient.Do(reqGlobal)
		if err == nil {
			bodyGlobal, _ := io.ReadAll(respGlobal.Body)
			respGlobal.Body.Close()
			var globalResult map[string]interface{}
			if json.Unmarshal(bodyGlobal, &globalResult) == nil {
				if dataMap, ok := globalResult["data"].(map[string]interface{}); ok && dataMap != nil {
					isStream, _ := dataMap["isStream"].(bool)

					anchorName := roomID
					avatar := ""
					infoApi := fmt.Sprintf("https://api.sooplive.com/v2/channel/info/%s", roomID)
					reqInfo, _ := http.NewRequest("GET", infoApi, nil)
					reqInfo.Header.Set("client-id", clientId)
					reqInfo.Header.Set("User-Agent", globalUa)

					builtinCookieMutex.RLock()
					if builtinCookies.Soop != "" {
						reqInfo.Header.Set("Cookie", builtinCookies.Soop)
					}
					builtinCookieMutex.RUnlock()

					if respInfo, err := builtinHTTPClient.Do(reqInfo); err == nil {
						infoBody, _ := io.ReadAll(respInfo.Body)
						respInfo.Body.Close()
						var infoRes map[string]interface{}
						if json.Unmarshal(infoBody, &infoRes) == nil {
							if infoData, ok := infoRes["data"].(map[string]interface{}); ok {
								if channelInfo, ok := infoData["streamerChannelInfo"].(map[string]interface{}); ok {
									if nick, ok := channelInfo["nickname"].(string); ok {
										anchorName = fmt.Sprintf("%s-%s", nick, roomID)
									}
									if profileImg, ok := channelInfo["profileImage"].(string); ok {
										avatar = profileImg
									}
								}
							}
						}
					}

					if avatar != "" {
						avatar = "/api/v1/builtin_recorder/proxy_image?url=" + url.QueryEscape(avatar)
					}

					if isStream {
						streamURL := fmt.Sprintf("https://global-media.sooplive.com/live/%s/master.m3u8", roomID)
						return streamURL, anchorName, avatar, nil
					} else {
						return "", anchorName, avatar, nil
					}
				}
			}
		}
	}

	apiURL := "http://api.m.sooplive.co.kr/broad/a/watch"
	formData := url.Values{}
	formData.Set("bj_id", roomID)
	formData.Set("broad_no", "")
	formData.Set("agent", "web")
	formData.Set("confirm_adult", "true")
	formData.Set("player_type", "webm")
	formData.Set("mode", "live")

	req, err := http.NewRequest("POST", apiURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return "", roomID, "", err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", globalUa)
	req.Header.Set("Origin", "https://m.sooplive.co.kr")
	req.Header.Set("Referer", "https://m.sooplive.co.kr/")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.8,zh-TW;q=0.7,zh-HK;q=0.5,en-US;q=0.3,en;q=0.2")

	builtinCookieMutex.RLock()
	if builtinCookies.Soop != "" {
		req.Header.Set("Cookie", builtinCookies.Soop)
	}
	builtinCookieMutex.RUnlock()

	resp, err := builtinHTTPClient.Do(req)
	if err != nil {
		return "", roomID, "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", roomID, "", err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		if len(body) > 0 && body[0] == '<' {
			return "", roomID, "", fmt.Errorf("被平台风控拦截(需更新Cookie)")
		}
		return "", roomID, "", fmt.Errorf("JSON 解析失败: %v", err)
	}

	dataMap, _ := result["data"].(map[string]interface{})

	anchorName := roomID
	if dataMap != nil {
		if nick, ok := dataMap["user_nick"].(string); ok && nick != "" {
			if bjID, ok := dataMap["bj_id"].(string); ok && bjID != "" {
				anchorName = fmt.Sprintf("%s-%s", nick, bjID)
			} else {
				anchorName = nick
			}
		}
	}

	avatar := ""
	if dataMap != nil {
		if bjID, ok := dataMap["bj_id"].(string); ok && len(bjID) >= 2 {
			avatar = fmt.Sprintf("https://stimg.afreecatv.com/LOGO/%s/%s/%s.jpg", bjID[:2], bjID, bjID)
		}
	}

	if avatar != "" {
		avatar = "/api/v1/builtin_recorder/proxy_image?url=" + url.QueryEscape(avatar)
	}

	resCode, ok := result["result"].(float64)
	if !ok || resCode != 1 {
		if dataMap != nil {
			if code, ok := dataMap["code"].(float64); ok {
				if code == -6001 || code == -3001 {
					return "", anchorName, avatar, nil
				} else if code == -3002 || code == -3004 {
					return "", anchorName, avatar, fmt.Errorf("该直播需要19+登录或验证，请更新 Cookie (code: %v)", code)
				}
			}
		}
		return "", anchorName, avatar, nil
	}

	broadNoStr := ""
	if bn, ok := dataMap["broad_no"].(string); ok {
		broadNoStr = bn
	} else if bnFloat, ok := dataMap["broad_no"].(float64); ok {
		broadNoStr = fmt.Sprintf("%.0f", bnFloat)
	}

	aid := ""
	if a, ok := dataMap["hls_authentication_key"].(string); ok {
		aid = a
	}

	if broadNoStr == "" || aid == "" {
		return "", anchorName, avatar, fmt.Errorf("提取 broad_no 或 aid 失败")
	}

	cdns := []string{"gcp_cdn", "kt_cdn", "cf_cdn", "ak_cdn", "rmc_cdn"}
	var suffixes []string
	if quality == "hd" {
		suffixes = []string{"-common-hd-hls", "-common-master-hls", "-common-original-hls", "-default-hls"}
	} else if quality == "sd" {
		suffixes = []string{"-common-sd-hls", "-common-hd-hls", "-common-master-hls", "-default-hls"}
	} else {
		suffixes = []string{"-common-original-hls", "-common-master-hls", "-default-hls", "-common-hd-hls"}
	}

	var finalStreamURL string

OuterLoop:
	for _, cdn := range cdns {
		for _, suffix := range suffixes {
			cdnURL := fmt.Sprintf("http://livestream-manager.sooplive.co.kr/broad_stream_assign.html?return_type=%s&use_cors=false&cors_origin_url=play.sooplive.co.kr&broad_key=%s%s&time=%d", cdn, broadNoStr, suffix, time.Now().UnixMilli())

			reqCdn, err := http.NewRequest("GET", cdnURL, nil)
			if err != nil {
				continue
			}

			reqCdn.Header.Set("User-Agent", globalUa)
			reqCdn.Header.Set("Origin", "https://play.sooplive.co.kr")
			reqCdn.Header.Set("Referer", "https://play.sooplive.co.kr/")
			reqCdn.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			respCdn, err := builtinHTTPClient.Do(reqCdn)
			if err != nil {
				continue
			}

			bodyCdn, err := io.ReadAll(respCdn.Body)
			respCdn.Body.Close()
			if err != nil {
				continue
			}

			var cdnResult map[string]interface{}
			if err := json.Unmarshal(bodyCdn, &cdnResult); err == nil {
				if viewURL, ok := cdnResult["view_url"].(string); ok && viewURL != "" {
					finalStreamURL = viewURL + "?aid=" + aid
					break OuterLoop
				}
			}
		}
	}

	if finalStreamURL == "" {
		return "", anchorName, avatar, fmt.Errorf("遍历 CDN 节点提取 view_url 失败")
	}

	return finalStreamURL, anchorName, avatar, nil
}

// apiProxyImage 设置本地反代接口转发获取直播封面图并伪造请求头，穿透部分平台的防盗链拦截限制
func apiProxyImage(w http.ResponseWriter, r *http.Request) {
	targetURL := r.URL.Query().Get("url")
	if targetURL == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}

	doProxy := func(withReferer bool) (*http.Response, error) {
		req, err := http.NewRequest("GET", targetURL, nil)
		if err != nil {
			return nil, err
		}

		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		req.Header.Set("Cache-Control", "no-cache")

		if withReferer {
			if strings.Contains(targetURL, "douyinpic.com") || strings.Contains(targetURL, "douyincdn.com") || strings.Contains(targetURL, "byteimg.com") {
				req.Header.Set("Referer", "https://live.douyin.com/")
			} else if strings.Contains(targetURL, "kuaishou") || strings.Contains(targetURL, "yximgs.com") {
				req.Header.Set("Referer", "https://live.kuaishou.com/")
			}
		}

		return builtinHTTPClient.Do(req)
	}

	resp, err := doProxy(true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	if resp.StatusCode == 403 || resp.StatusCode == 401 {
		resp.Body.Close()
		resp, err = doProxy(false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		return
	}

	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// ==========================================
// 🌟 终极截帧黑科技：内存级多路分发截帧
// ==========================================

// builtinTailBuffer 环形日志尾部缓冲池，避免收集底层日志时消耗过多内存
type builtinTailBuffer struct {
	buf []byte
	mu  sync.Mutex
}

// Write (builtinTailBuffer) 实现一个固定长度的循环尾部日志记录缓冲区，避免长期运行消耗过多机器内存
func (t *builtinTailBuffer) Write(p []byte) (n int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		copy(t.buf, t.buf[len(t.buf)-2048:])
		t.buf = t.buf[:2048]
	}
	return len(p), nil
}

// extractBuiltinCoverFromLocalFile 旁路抽帧大法：只读取本地录像尾部少量字节流交由内存端 ffmpeg 解析，避免卡死并极大降低系统 CPU
func extractBuiltinCoverFromLocalFile(dir, prefix, coverPath string) bool {
	// 1. 读取指定目录下的所有文件列表，寻找当前录像切片
	files, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	var latestFile string
	var maxTime time.Time

	// 2. 遍历筛选出符合前缀且最新的 .ts 视频分片文件
	for _, f := range files {
		if !f.IsDir() && strings.HasPrefix(f.Name(), prefix) && strings.HasSuffix(f.Name(), ".ts") {
			info, err := f.Info()
			// 确保文件大小大于 512KB 以防止读取到损坏或不完整的切片
			if err == nil && info.ModTime().After(maxTime) && info.Size() > 512*1024 {
				maxTime = info.ModTime()
				latestFile = filepath.Join(dir, f.Name())
			}
		}
	}

	// 如果没有找到有效文件，则直接返回失败，等待下一轮轮询
	if latestFile == "" {
		return false
	}

	// 3. 打开最新写入的视频文件准备读取尾部数据
	file, err := os.Open(latestFile)
	if err != nil {
		return false
	}
	defer file.Close()

	// 4. 计算并提取文件尾部的数据块 (提升至最高读取 5MB，确保绝对能命中一个完整的 I 帧)
	stat, _ := file.Stat()
	size := stat.Size()
	readSize := int64(5 * 1024 * 1024)
	if size < readSize {
		readSize = size
	}

	// 将文件指针移动到尾部并读取数据到缓冲区中
	file.Seek(-readSize, 2)
	buf := make([]byte, readSize)
	_, err = io.ReadFull(file, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return false
	}

	// 5. 组装 FFmpeg 截帧命令 (采用 I 帧精准提取 + PNG 无损编码引擎)
	cmd := exec.Command(builtinFfmpegPath,
		"-y",
		"-i", "pipe:0",
		"-vf", "select='eq(pict_type,I)'",
		"-frames:v", "1",
		"-c:v", "png",
		"-f", "image2",
		coverPath,
	)

	// 6. 将内存中的尾部视频流通过标准输入管道 (pipe) 传给 FFmpeg 进程
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false
	}

	// 启动进程并异步写入流数据
	cmd.Start()
	stdin.Write(buf)
	stdin.Close()

	// 等待截帧真正执行完毕并落盘
	cmd.Wait()

	return true
}

// BuiltinRecordStream 调动底层 FFmpeg 进程并将推流直通本地文件，增加了高度强化的上下文状态管控防止僵尸进程
func BuiltinRecordStream(ctx context.Context, streamURL, platformName, roomID, anchorName, avatar, quality string, segmentTime int) {
	updateBuiltinStatus(platformName, roomID, anchorName, avatar, quality, "录制中")
	safeName := sanitizeBuiltinFileName(anchorName)
	if safeName == "" {
		safeName = roomID
	}

	baseDir := builtinConfig.SavePath
	if baseDir == "" {
		baseDir = "./downloads"
	}

	outDir := filepath.Join(baseDir, safeName)
	os.MkdirAll(outDir, os.ModePerm)
	timestamp := time.Now().Format("2006-01-02_15-04-05")

	var args []string
	var outPath string

	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	args = append(args, "-y", "-user_agent", ua)

	if platformName == "Douyin" {
		args = append(args, "-headers", "Referer: https://live.douyin.com/\r\n")
	} else if platformName == "Soop" {
		args = append(args, "-headers", "Referer: https://play.sooplive.co.kr/\r\nOrigin: https://play.sooplive.co.kr\r\n")
	}

	args = append(args, "-rw_timeout", "15000000", "-analyzeduration", "5000000", "-probesize", "5000000", "-i", streamURL)
	args = append(args, "-map", "0:v?", "-map", "0:a?", "-ignore_unknown")

	if segmentTime > 0 {
		outPath = filepath.Join(outDir, fmt.Sprintf("%s_%s_%%03d.ts", safeName, timestamp))
		args = append(args, "-c:v", "copy", "-c:a", "copy", "-f", "segment", "-segment_time", fmt.Sprintf("%d", segmentTime*60), "-reset_timestamps", "1", outPath)
	} else {
		outPath = filepath.Join(outDir, fmt.Sprintf("%s_%s.ts", safeName, timestamp))
		args = append(args, "-c:v", "copy", "-c:a", "copy", "-f", "mpegts", outPath)
	}

	fileName := fmt.Sprintf("%s_%s.png", platformName, roomID)
	coverDir := filepath.Join(".", "covers")
	os.MkdirAll(coverDir, os.ModePerm)
	coverPath := filepath.Join(coverDir, fileName)

	log.Printf("\n🟢 [开始录制] 平台: %s | 主播: %s | 画质: %s\n   📂 TS视频存至: %s\n   📸 旁路截图机制已启动", platformName, anchorName, formatBuiltinQualityName(quality), outPath)

	startTime := time.Now()

	// 引入派生 Context 与 WaitGroup 强化协程与进程生命周期管理
	recordCtx, cancelRecord := context.WithCancel(ctx)
	defer cancelRecord()

	cmd := exec.Command(builtinFfmpegPath, args...)

	var stderrBuf builtinTailBuffer
	cmd.Stderr = &stderrBuf

	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("获取ffmpeg stdin失败: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("\n🔴 [启动录制失败] %s | %s: %v\n", platformName, anchorName, err)
		updateBuiltinStatus(platformName, roomID, anchorName, avatar, quality, "未开播等待中")
		return
	}

	// 使用 WaitGroup 精确实阻塞与释放旁路抽帧子协程
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var lastModTime time.Time
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()

		coverCount := 1
		filePrefix := fmt.Sprintf("%s_%s", safeName, timestamp)

		time.Sleep(5 * time.Second)
		extractBuiltinCoverFromLocalFile(outDir, filePrefix, coverPath)

		for {
			select {
			case <-recordCtx.Done(): // 收到严格终止指令，立刻结束旁路监测
				return
			case <-ticker.C:
				extracted := extractBuiltinCoverFromLocalFile(outDir, filePrefix, coverPath)

				if extracted {
					if info, err := os.Stat(coverPath); err == nil && info.Size() > 0 {
						modTime := info.ModTime()
						if modTime.After(lastModTime) {
							lastModTime = modTime

							data, readErr := os.ReadFile(coverPath)
							if readErr == nil && len(data) > 0 {
								imgArchiveDir := filepath.Join(outDir, "Screenshots")
								os.MkdirAll(imgArchiveDir, os.ModePerm)

								archiveCoverPath := filepath.Join(imgArchiveDir, fmt.Sprintf("%s_%s_cover_%04d.png", safeName, timestamp, coverCount))
								_ = os.WriteFile(archiveCoverPath, data, 0644)
								log.Printf("[BUILTIN] 📸 旁路截帧 (第 %d 次) 已保存至独立截图目录，等待自动上传: %s", coverCount, archiveCoverPath)
								coverCount++
							}

							key := platformName + "_" + roomID
							if existing, ok := builtinStatusMap.Load(key); ok {
								task := existing.(*BuiltinTaskStatus)
								task.Avatar = fmt.Sprintf("/covers/%s?t=%d", fileName, time.Now().UnixMilli())
								builtinStatusMap.Store(key, task)
								triggerBuiltinBroadcast()
							}
						}
					}
				}
			}
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	// 核心生命周期管控模型
	select {
	case <-recordCtx.Done():
		log.Printf("\n⚠️ [终止信号] 收到任务中止信号，正在安全结束 %s | %s ...\n", platformName, anchorName)
		if stdin != nil {
			io.WriteString(stdin, "q\n") // 优先发送封装指令
			stdin.Close()
		}

		select {
		case <-done:
			log.Printf("\n✅ [手动停止] %s | %s | 录像已安全保存完毕\n", platformName, anchorName)
		case <-time.After(10 * time.Second):
			if cmd.Process != nil {
				cmd.Process.Kill() // 10秒不退则毫不留情强杀，断绝僵尸进程
			}
			log.Printf("\n🔴 [超时强杀] %s | %s | FFmpeg卡死已回收内存\n", platformName, anchorName)
		}
	case err := <-done:
		duration := time.Since(startTime)
		if err != nil {
			log.Printf("\n🔴 [录制异常/断流] %s | %s | 时长: %s | 错误: %v\n🔥 FFmpeg 底层真实报错:\n%s\n", platformName, anchorName, formatBuiltinDuration(duration), err, string(stderrBuf.buf))
		} else {
			log.Printf("\n🟢 [录制结束] %s | %s | 时长: %s (自然完成)\n", platformName, anchorName, formatBuiltinDuration(duration))
		}
	}

	cancelRecord()

	wg.Wait() // 彻底等待旁路协程销毁

	// 自动清理小于 1KB 的失效封面图残余
	if info, err := os.Stat(coverPath); err == nil && info.Size() < 1024 {
		os.Remove(coverPath)
	}

	updateBuiltinStatus(platformName, roomID, anchorName, avatar, quality, "未开播等待中")
}

// wrapperStartMonitorIfNotRunning 将单个监控目标封入守护协程，并实现任务防重载冲突及心跳检测重连功能
func wrapperStartMonitorIfNotRunning(p BuiltinPlatform, roomID string) {
	platformName := p.GetPlatformName()
	key := platformName + "_" + roomID

	if _, exists := builtinActiveTasks.Load(key); exists {
		return
	}
	builtinActiveTasks.Store(key, true)

	go func() {
		builtinTaskStates.Store(key, "running")
		log.Printf("👀 [启动监控] %s 房间: %s", platformName, roomID)
		updateBuiltinStatus(platformName, roomID, "", "", "-", "监控中")

		rand.NewSource(time.Now().UnixNano())

		for {
			state, _ := builtinTaskStates.Load(key)

			if state == "deleted" {
				log.Printf("🗑️ [任务移除] 已停止监控 %s 房间: %s", platformName, roomID)
				builtinStatusMap.Delete(key)
				builtinActiveTasks.Delete(key)
				return
			}

			if state == "paused" {
				updateBuiltinStatus(platformName, roomID, "", "", "-", "已暂停")
				time.Sleep(2 * time.Second)
				continue
			}

			ctx, cancel := context.WithCancel(context.Background())
			builtinCancels.Store(key, cancel)

			q := builtinConfig.Quality
			st := builtinConfig.SegmentTime

			url, name, avatar, err := p.GetStreamURL(roomID, q)

			if name != "" && name != roomID && !strings.Contains(name, "未命名") {
				if custom, ok := builtinCustomNames.Load(key); !ok || custom.(string) != name {
					builtinCustomNames.Store(key, name)
					updateBuiltinNameInTxt(platformName, roomID, name)
				}
			} else {
				if custom, ok := builtinCustomNames.Load(key); ok && custom.(string) != "" {
					name = custom.(string)
				}
			}

			if err != nil {
				log.Printf("⚠️ [检测出错] %s %s: %v", platformName, roomID, err)
				updateBuiltinStatus(platformName, roomID, name, avatar, q, "检测异常等待中")

				sleepDur := builtinConfig.CheckInterval
				if sleepDur < 10 {
					sleepDur = 10
				}
				t := time.NewTimer(time.Duration(sleepDur) * time.Second)
				select {
				case <-ctx.Done():
					t.Stop()
				case <-t.C:
				}
			} else if url != "" {
				updateBuiltinStatus(platformName, roomID, name, avatar, q, "录制中")

				BuiltinRecordStream(ctx, url, platformName, roomID, name, avatar, q, st)

				state, _ = builtinTaskStates.Load(key)
				if state != "deleted" && state != "paused" {
					log.Printf("⏳ [断流等待] %s %s 进入15秒冷却...", platformName, name)
					updateBuiltinStatus(platformName, roomID, name, avatar, q, "断流缓冲中")

					t := time.NewTimer(15 * time.Second)
					select {
					case <-ctx.Done():
						t.Stop()
					case <-t.C:
					}
				}
			} else {
				if name != "" {
					updateBuiltinStatus(platformName, roomID, name, avatar, q, "监控中")
				}

				sleepDur := builtinConfig.CheckInterval
				if sleepDur < 10 {
					sleepDur = 10
				}
				jitter := rand.Intn(5)

				updateBuiltinStatus(platformName, roomID, name, avatar, q, "未开播等待中")

				t := time.NewTimer(time.Duration(sleepDur+jitter) * time.Second)
				select {
				case <-ctx.Done():
					t.Stop()
				case <-t.C:
				}
			}

			builtinCancels.Delete(key)
			cancel() // 确保释放本轮的局部监听器
		}
	}()
}

// ==========================================
// 内置录制系统 Web API (已接入商业级解密)
// ==========================================

// apiRecorderConfig 处理内置引擎对画质及参数配置的解析和存储，已强制兼容加密格式接收
func apiRecorderConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var c BuiltinConfig
		if err := parseEncryptedRequest(r, &c); err != nil {
			sendJSONError(w, r, http.StatusBadRequest, "商业安全网关拦截: 非法配置实体或解密异常")
			return
		}

		if c.Quality != "" {
			builtinConfig.Quality = c.Quality
		}
		builtinConfig.SegmentTime = c.SegmentTime
		if c.SavePath != "" {
			builtinConfig.SavePath = c.SavePath
		}
		data, _ := json.MarshalIndent(builtinConfig, "", "    ")
		os.WriteFile("builtin_config.json", data, 0644)
		sendJSONSuccess(w, r, nil)
		return
	}
	sendJSONSuccess(w, r, builtinConfig)
}

// apiRecorderCookies 处理内置引擎应对各大平台反制而提供的 Cookie 更新，已强制兼容加密格式接收
func apiRecorderCookies(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var c BuiltinCookieConfig
		if err := parseEncryptedRequest(r, &c); err != nil {
			sendJSONError(w, r, http.StatusBadRequest, "商业安全网关拦截: 非法 Cookie 实体或解密异常")
			return
		}

		builtinCookieMutex.Lock()
		builtinCookies.Douyin = c.Douyin
		builtinCookies.Kuaishou = c.Kuaishou
		builtinCookies.Soop = c.Soop
		builtinCookieMutex.Unlock()
		data, _ := json.MarshalIndent(builtinCookies, "", "    ")
		os.WriteFile("builtin_cookies.json", data, 0644)
		sendJSONSuccess(w, r, nil)
		return
	}
	builtinCookieMutex.RLock()
	sendJSONSuccess(w, r, builtinCookies)
	builtinCookieMutex.RUnlock()
}

// apiRecorderAdd 提供将前端通过面板添加的单条或批量直播间转录成录制指令池内的待处理任务功能
func apiRecorderAdd(w http.ResponseWriter, r *http.Request) {
	var d struct {
		Platform string `json:"platform"`
		URL      string `json:"url"`
	}

	if err := parseEncryptedRequest(r, &d); err != nil {
		sendJSONError(w, r, http.StatusBadRequest, "商业安全网关拦截: 非法添加实体或解密异常")
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

		isP, platformName, roomID, customName, _ := parseBuiltinLine(fullLineToSave)
		if roomID == "" {
			continue
		}
		if platformName == "" {
			platformName = d.Platform
		}
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
		sendJSONError(w, r, http.StatusBadRequest, "该主播/直播间已存在于列表中，请勿重复添加！")
		return
	}

	sendJSONSuccess(w, r, nil)
}

// apiRecorderControl 为列表里的单条项目指派状态机动作（恢复监控、挂起监控、完全剔除等）
func apiRecorderControl(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action   string `json:"action"`
		Platform string `json:"platform"`
		RoomID   string `json:"room_id"`
	}

	if err := parseEncryptedRequest(r, &req); err != nil {
		sendJSONError(w, r, http.StatusBadRequest, "商业安全网关拦截: 非法操作实体或解密异常")
		return
	}

	key := req.Platform + "_" + req.RoomID
	switch req.Action {
	case "pause":
		builtinTaskStates.Store(key, "paused")
		if cancel, ok := builtinCancels.Load(key); ok {
			cancel.(context.CancelFunc)()
		}
		syncBuiltinAnchorToTxt("pause", req.Platform, req.RoomID, "")
		if existing, ok := builtinStatusMap.Load(key); ok {
			task := existing.(*BuiltinTaskStatus)
			task.IsPaused = true
			task.Status = "已暂停"
			builtinStatusMap.Store(key, task)
		}
	case "resume":
		builtinTaskStates.Store(key, "running")
		syncBuiltinAnchorToTxt("resume", req.Platform, req.RoomID, "")
		if existing, ok := builtinStatusMap.Load(key); ok {
			task := existing.(*BuiltinTaskStatus)
			task.IsPaused = false
			task.Status = "监控中"
			builtinStatusMap.Store(key, task)
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
	sendJSONSuccess(w, r, nil)
}

// apiRecorderControlAll 执行对当前用户记录中的全部任务群发起全局同步的批量管控状态更新
func apiRecorderControlAll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}

	if err := parseEncryptedRequest(r, &req); err != nil {
		sendJSONError(w, r, http.StatusBadRequest, "商业安全网关拦截: 非法全局操作实体或解密异常")
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
			task.IsPaused = true
			task.Status = "已暂停"
			builtinStatusMap.Store(key, task)
		} else if req.Action == "resume_all" {
			builtinTaskStates.Store(key, "running")
			task.IsPaused = false
			task.Status = "监控中"
			builtinStatusMap.Store(key, task)
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
	sendJSONSuccess(w, r, nil)
}
