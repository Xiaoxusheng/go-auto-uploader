package recorder

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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

	// 状态真正变化时打一行，并带上调用方位置。
	// 起因：线上出现过「主播实际在录、控制台却显示未开播」——后端状态停在「监控中」而非
	// 「录制中」，但当时没有任何日志能看出是谁把它覆盖的。有这行就能直接定位到调用点。
	if prev, ok := builtinStatusMap.Load(key); ok {
		if old := prev.(*BuiltinTaskStatus); old.Status != statusMsg {
			_, file, line, _ := runtime.Caller(1)
			log.Printf("[STATUS] %s/%s %s: %q → %q  (%s)",
				platform, roomID, anchorName, old.Status, statusMsg,
				filepath.Base(file)+":"+strconv.Itoa(line))
		}
	}

	// ✨ 兜底：已接管推流却拿不到有效起点时补当前时刻。
	// 唯一会把 startTime 留成零值的路径是上面的防抖分支——命中 3 分钟窗口时
	// 直接沿用 oldTask.startTime，而条目若刚被删过重建（热重载移除后重加、
	// 删除后重加），旧快照的 startTime 就是零值，于是零值被一路继承下去。
	// 后果：GetBuiltinRecorderTasks 判定 startTime 为零 → 下发 duration="-"，
	// 前端「REC · 时长」恒显示 --:--（重启进程才恢复，因为状态表是内存态）。
	if isBuiltinLiveStatus(statusMsg) && sTime.IsZero() {
		sTime = now
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
		SegmentTime:     taskFlags.SegmentTime,
		Window:          taskFlags.Window,
		Highlight:       taskFlags.Highlight,
		HighlightOnly:   taskFlags.HighlightOnly,
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

// resumeStatusAfterUnpause 返回「恢复监控」时应写入的状态。
//
// 关键约束：只有确实处于暂停态时才回落「监控中」。
// 恢复监控走的是 builtinTaskStates 的 paused → running，但监控协程可能压根没停——
// 典型路径是任务从未被暂停过，或暂停期间 RecordStream 仍在阻塞（ffmpeg 未退出、
// 本场 TS 仍在增长）。此时状态表里的「录制中 / 截屏中」才是真实状态，无条件覆盖会
// 让控制台在「正在录」的时候显示 IDLE，并且因为 RecordStream 只在启动录制时写一次
// 状态（builtin_ffmpeg.go），这个错误会一直持续到本场录制结束才被下一轮循环纠正。
// 副作用：GetBuiltinRecorderTasks 判 isBuiltinLiveStatus 失败 → 下发 duration="-"，
// 前端「时长」恒显示 --:--，看起来像录制卡死。
func resumeStatusAfterUnpause(cur string) string {
	if cur == "" || cur == "已暂停" {
		return "监控中"
	}
	return cur
}

// clearBuiltinDebounce 清理某任务的上下播通知防抖记录。
// 任务条目被移除（删除 / 热重载剔除 / 监控协程退出）时必须一并清理：
// 防抖记录按 platform_roomID 存，生命周期却比条目长。若残留，
// 同一主播在 3 分钟内被重新加入并开播时会被误判成「同一次直播的静默重连」，
// 既吞掉本该发出的开播通知，又会去继承一个已失效（甚至零值）的录制起点。
func clearBuiltinDebounce(key string) {
	builtinNotifyDebounce.Delete("live_" + key)
	builtinNotifyDebounce.Delete("offline_" + key)
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
