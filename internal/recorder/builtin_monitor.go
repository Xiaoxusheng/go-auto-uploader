package recorder

import (
	"context"
	"log"
	"math/rand"
	"strings"
	"time"
)

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

			cfgSnap := Config()
			q := cfgSnap.Quality
			st := cfgSnap.SegmentTime

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

				sleepDur := Config().CheckInterval
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
				taskFlags := getBuiltinTaskFlags(platformName, roomID)
				RecordStream(ctx, url, platformName, roomID, name, avatar, q, st, taskFlags)

				// 配置热重载（如视频水印开关切换）导致的中断：立即按新配置重开，
				// 不走 30 秒断流退避，也不触发下播通知。
				if clearConfigRestart(key) {
					log.Printf("🔄 [配置重载] %s %s 已按新配置立即重启录制", platformName, name)
					builtinCancels.Delete(key)
					cancel()
					continue
				}

				state, _ = builtinTaskStates.Load(key)
				if state != "deleted" && state != "paused" {
					// 录屏/截屏全关时仅探测，不进入断流冷却，避免状态横跳
					if !taskFlags.Record && !taskFlags.Screenshot {
						sleepDur := Config().CheckInterval
						if sleepDur < 10 {
							sleepDur = 10
						}
						updateBuiltinStatus(platformName, roomID, name, avatar, q, "监控中")
						t := time.NewTimer(time.Duration(sleepDur) * time.Second)
						select {
						case <-ctx.Done():
							t.Stop()
						case <-t.C:
						}
					} else {
						// 断流后退避：避免 CDN 抖动时 15s 紧循环重拉把 CPU 打满。
						// Twitch 广告插入/CDN 轮换会频繁触发流 EOF，用短冷却快速重连减少内容丢失
						backoff := 30 * time.Second
						if platformName == "Twitch" {
							backoff = 8 * time.Second
						}
						log.Printf("⏳ [断流等待] %s %s 进入%d秒冷却...", platformName, name, int(backoff.Seconds()))
						updateBuiltinStatus(platformName, roomID, name, avatar, q, "断流缓冲中")

						t := time.NewTimer(backoff)
						select {
						case <-ctx.Done():
							t.Stop()
						case <-t.C:
						}
					}
				}
			} else {
				if name != "" {
					updateBuiltinStatus(platformName, roomID, name, avatar, q, "监控中")
				}

				sleepDur := Config().CheckInterval
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

			// 休眠期间被配置重载打断时，标记已无意义（下一轮本就是全新会话），就地清理
			clearConfigRestart(key)
			builtinCancels.Delete(key)
			cancel() // 确保释放本轮的局部监听器
		}
	}()
}

// ==========================================
// 内置录制系统 Web API (已接入商业级解密)
// ==========================================

// apiRecorderConfig 处理内置引擎对画质及参数配置的解析和存储，已强制兼容加密格式接收
