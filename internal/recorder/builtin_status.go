package recorder

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func startBuiltinBroadcastDebouncer() {
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond) // 500ms 聚合窗口，性能与实时性的最佳平衡
		defer ticker.Stop()
		needsBroadcast := false

		for {
			select {
			case <-builtinBroadcastChan:
				needsBroadcast = true
			case <-ticker.C:
				if needsBroadcast {
					hookBroadcast("builtinTasks", GetBuiltinRecorderTasks())
					needsBroadcast = false
				}
			}
		}
	}()
}

// triggerBuiltinBroadcast 触发内置引擎任务状态列表向前端加密 WebSocket 信道的全量广播
// ✨ 优化：已接入全局防抖节流机制，避免瞬间多路状态变化引起广播风暴卡死前端
func triggerBuiltinBroadcast() {
	select {
	case builtinBroadcastChan <- struct{}{}:
	default:
		// 通道已满（当前节流周期内已有待处理信号），直接丢弃重复触发
	}
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
		// ✨ 修复核心：默认继承上一次的 startTime，防止其他非录制状态将其重置为零值
		sTime = oldTask.startTime

		if isBuiltinLiveStatus(statusMsg) {
			if !isBuiltinLiveStatus(oldTask.Status) {
				// ✨ 防抖判定：确保两次相同的【开播通知】之间至少缓冲 3 分钟，否则静默恢复时间戳
				cacheKey := "live_" + key
				if last, has := builtinNotifyDebounce.Load(cacheKey); !has || time.Since(last.(time.Time)) > 3*time.Minute {
					builtinNotifyDebounce.Store(cacheKey, now)
					sTime = now
					isNewlyRecording = true
					hookNotify("开播通知", fmt.Sprintf("检测到平台 [%s] 的主播 [%s] 开始直播并已成功接管录制！", platform, anchorName))
				} else {
					sTime = oldTask.startTime // 静默无感知恢复推流，避免惊扰管理员
				}
			} else {
				sTime = oldTask.startTime
			}
		} else if statusMsg == "未开播等待中" || statusMsg == "断流缓冲中" || statusMsg == "已暂停" || statusMsg == "配置重载中" {
			// 配置热重载（切换视频水印等）导致的中断不是下播，不能误报通知
			if isBuiltinLiveStatus(oldTask.Status) && !isConfigRestart(key) {
				// ✨ 防抖判定：防下播通知连发
				cacheKey := "offline_" + key
				if last, has := builtinNotifyDebounce.Load(cacheKey); !has || time.Since(last.(time.Time)) > 3*time.Minute {
					builtinNotifyDebounce.Store(cacheKey, now)
					hookNotify("下播通知", fmt.Sprintf("检测到平台 [%s] 的主播 [%s] 已经下播或断流停止录制！", platform, anchorName))
				}
			}
		}
	} else {
		if isBuiltinLiveStatus(statusMsg) {
			sTime = now
			isNewlyRecording = true
			builtinNotifyDebounce.Store("live_"+key, now)
			hookNotify("开播通知", fmt.Sprintf("检测到平台 [%s] 的主播 [%s] 开始直播并已成功接管录制！", platform, anchorName))
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

	// 整体覆盖指针，不存在内部并发修改的脏数据竞争问题
	taskFlags := getBuiltinTaskFlags(platform, roomID)
	builtinStatusMap.Store(key, &BuiltinTaskStatus{
		Platform:        platform,
		RoomID:          roomID,
		AnchorName:      anchorName,
		Avatar:          avatar,
		Quality:         quality,
		Status:          statusMsg,
		UpdateTime:      time.Now().Format("2006-01-02 15:04:05"),
		IsPaused:        isPaused,
		Record:          taskFlags.Record,
		Screenshot:      taskFlags.Screenshot,
		ShotInterval:    taskFlags.ShotInterval,
		Watermark:       taskFlags.Watermark,
		QualityOverride: taskFlags.Quality,
		MaxDuration:     taskFlags.MaxDuration,
		startTime:       sTime,
	})

	triggerBuiltinBroadcast()

	if isNewlyRecording && !isPaused {
		go func() {
			time.Sleep(2500 * time.Millisecond)
			hookTriggerScan(fmt.Sprintf("内置引擎捕获[%s]开播", anchorName))
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
		isP, p, rid, customName, rawURL, _ := parseBuiltinLine(trimmed)
		if p == platform && rid == roomID {
			if customName != anchorName && anchorName != "" && anchorName != roomID {
				curFlags := getBuiltinTaskFlags(p, rid)
				prefix := ""
				if isP {
					prefix = "#"
				}
				safeName := strings.ReplaceAll(anchorName, "\n", "")
				safeName = strings.ReplaceAll(safeName, "\r", "")
				lines[i] = rebuildBuiltinLineWithFlags(fmt.Sprintf("%s%s,主播:%s", prefix, rawURL, safeName), curFlags)
				changed = true
			}
		}
	}

	if changed {
		os.WriteFile("builtin_urls.txt", []byte(strings.Join(lines, "\n")+"\n"), 0644)
	}
}

// builtinHotReloadLoop 后台热重载守护协程，定时检测 builtin_urls.txt 的修改动态启停监控任务
