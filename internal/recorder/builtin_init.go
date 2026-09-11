package recorder

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

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
				isPaused, platformName, roomID, customName, _, flags := parseBuiltinLine(line)
				if roomID == "" || platformName == "" {
					continue
				}
				key := platformName + "_" + roomID
				currentKeys[key] = true
				setBuiltinTaskFlags(platformName, roomID, flags)

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
						updateBuiltinStatus(platformName, roomID, displayName, "", Config().Quality, "已暂停")
					} else {
						updateBuiltinStatus(platformName, roomID, displayName, "", Config().Quality, "初始化中")
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
							// ✨ 优化：采用值拷贝(Copy-On-Write)，避免指针原地修改引发的前端数据脏读和状态闪回
							task := *(existingTask.(*BuiltinTaskStatus))
							task.IsPaused = true
							task.Status = "已暂停"
							builtinStatusMap.Store(key, &task)
						}
					} else if !isPaused && state == "paused" {
						stateChanged = true
						builtinTaskStates.Store(key, "running")
						if existingTask, ok := builtinStatusMap.Load(key); ok {
							// ✨ 优化：采用值拷贝更新状态
							task := *(existingTask.(*BuiltinTaskStatus))
							task.IsPaused = false
							task.Status = "监控中"
							builtinStatusMap.Store(key, &task)
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

// InitBuiltinRecorder 初始化内置录制引擎模块，挂载相关 API 路由并启动系统常驻协程。
// 内置引擎参数统一存放于 config.json 的 builtin 区块（旧文件由 internal/config 启动时迁移）。
func InitBuiltinRecorder(mux *http.ServeMux) {
	checkFFmpegBuiltin()

	// 从统一配置仓库加载内置引擎参数
	bs := BuiltinConfig{Quality: "uhd", CheckInterval: 30, SavePath: "./downloads"}
	if cfgStore != nil {
		bs = cfgStore.Get().Builtin
	}
	SetConfig(&bs)

	// 平台 Cookie 独立可变，保存时回写进统一配置
	builtinCookieMutex.Lock()
	snap := Config()
	builtinCookies = &BuiltinCookieConfig{
		Douyin:   snap.Cookies.Douyin,
		Kuaishou: snap.Cookies.Kuaishou,
		Soop:     snap.Cookies.Soop,
	}
	builtinCookieMutex.Unlock()

	// builtin_urls.txt 为用户手编名单，保持独立文件并支持热重载
	if _, err := os.Stat("builtin_urls.txt"); os.IsNotExist(err) {
		os.WriteFile("builtin_urls.txt", []byte(""), 0644)
	} else {
		content, _ := os.ReadFile("builtin_urls.txt")
		lines := strings.Split(string(content), "\n")
		for _, line := range lines {
			isPaused, platform, roomID, customName, _, flags := parseBuiltinLine(line)
			if roomID == "" || platform == "" {
				continue
			}
			key := platform + "_" + roomID
			setBuiltinTaskFlags(platform, roomID, flags)
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
				updateBuiltinStatus(platform, roomID, displayName, "", Config().Quality, "已暂停")
			} else {
				displayName := customName
				if displayName == "" {
					displayName = roomID
				}
				updateBuiltinStatus(platform, roomID, displayName, "", Config().Quality, "初始化中")
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

	// ✨ 启动广播防抖控制流：必须放在配置加载完成之后，
	// 防抖协程会调用 GetBuiltinRecorderTasks 读取 builtinConfig，提前启动会在初始化窗口期踩空指针
	startBuiltinBroadcastDebouncer()

	go builtinHotReloadLoop()
}

// GetBuiltinRecorderTasks 获取当前内存中所有的内置引擎任务运行快照以提供给前端界面
func GetBuiltinRecorderTasks() []BuiltinTaskStatus {
	var list []BuiltinTaskStatus
	builtinStatusMap.Range(func(key, value interface{}) bool {
		task := *value.(*BuiltinTaskStatus) // 安全提取切片
		if isBuiltinLiveStatus(task.Status) && !task.startTime.IsZero() {
			task.Duration = formatBuiltinDuration(time.Since(task.startTime))
		} else {
			task.Duration = "-"
		}
		// 同步最新开关，避免 status 快照过期
		f := getBuiltinTaskFlags(task.Platform, task.RoomID)
		task.Record = f.Record
		task.Screenshot = f.Screenshot
		safeName := sanitizeBuiltinFileName(task.AnchorName)
		if safeName == "" {
			safeName = task.RoomID
		}
		baseDir := getBuiltinSavePath()
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

// getBuiltinSavePath 安全获取录制落盘根目录。
// 引擎初始化完成前（或测试环境下）全局配置指针可能为空，直接解引用会触发空指针崩溃
func getBuiltinSavePath() string {
	if p := Config(); p.SavePath != "" {
		return p.SavePath
	}
	return "./downloads"
}

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
	return ExtractRoomID(input)
}

// sanitizeBuiltinFileName 清洗并规范化主播名称（见 internal/recorder）
func sanitizeBuiltinFileName(name string) string {
	return SanitizeName(name)
}

// formatBuiltinDuration 将 Go 时间差对象格式化为 X小时X分X秒 格式
func formatBuiltinDuration(d time.Duration) string {
	return FormatDuration(d)
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
	return FormatBytes(b)
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

// parseBuiltinLine 分析本地监控的行数据（委托 internal/recorder）
