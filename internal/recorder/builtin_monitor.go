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

		// 单场录满时长上限标记：录满后本场不再续录，主播下播（探测明确离线）后自动复位
		cappedThisLive := false

		for {
			state, _ := builtinTaskStates.Load(key)

			if state == "deleted" {
				log.Printf("🗑️ [任务移除] 已停止监控 %s 房间: %s", platformName, roomID)
				builtinStatusMap.Delete(key)
				builtinActiveTasks.Delete(key)
				clearBuiltinDebounce(key)
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
			st := cfgSnap.SegmentTime

			// 画质取值：单主播覆盖（画质:uhd/hd/sd）优先，否则跟随全局设置
			taskFlags := getBuiltinTaskFlags(platformName, roomID)
			q := cfgSnap.Quality
			if taskFlags.Quality != "" {
				q = taskFlags.Quality
			}

			// 定时录制窗口：窗口外只探测等待，不调用平台接口、不拉流
			outsideWindow := !inRecordingWindow(taskFlags.Window, time.Now())

			var url, name, avatar string
			var err error
			if outsideWindow {
				updateBuiltinStatus(platformName, roomID, "", "", q, "非录制时段")
			} else {
				url, name, avatar, err = p.GetStreamURL(roomID, q)
				// 上报解析结果供 Cookie 健康被动检测：err 计入平台连续错误
				recordCookieProbeOutcome(platformName, err != nil)

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
			}

			if outsideWindow {
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
			} else if err != nil {
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
				// 录制中通过 set_flags 改画质/水印会打配置重开标记并取消会话：
				// 处于「已录满上限」轮询时同样要放行，否则新配置永远不会生效
				if cappedThisLive && clearConfigRestart(key) {
					cappedThisLive = false
				}

				if cappedThisLive {
					// 本场已录满最长录制时长：流仍在线时不续录，按探测间隔轮询直至下播
					updateBuiltinStatus(platformName, roomID, name, avatar, q, "已录满时长上限")
					sleepDur := Config().CheckInterval
					if sleepDur < 10 {
						sleepDur = 10
					}
					t := time.NewTimer(time.Duration(sleepDur) * time.Second)
					select {
					case <-ctx.Done():
						t.Stop()
						// 取消可能伴随配置重开标记而来（如录满轮询中改画质）：
						// 必须赶在循环尾部就地清理之前消费，否则新画质永远不生效
						if clearConfigRestart(key) {
							cappedThisLive = false
						}
					case <-t.C:
					}
					continue
				}

				// 单主播切片时长覆盖优先，否则跟随全局「自动分片时长」。
				// 切片由 ffmpeg 原生完成：同进程内按时间切文件，录制不中断、不丢帧。
				segTime := taskFlags.SegmentTime
				if segTime <= 0 {
					segTime = st
				}
				hitMax := RecordStream(ctx, url, platformName, roomID, name, avatar, q, segTime, taskFlags)
				if hitMax {
					cappedThisLive = true
					log.Printf("⏹️ [时长上限] %s %s 已录满单场上限（%d 分钟），本场停止续录，主播下播后自动恢复", platformName, name, taskFlags.MaxDuration)
				}

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
				// 探测明确离线（未开播/已下播）：解除本场录满标记，下次开播恢复正常录制
				cappedThisLive = false

				// 这里曾经先写一次「监控中」再写「未开播等待中」（相隔 0.1~0.3ms），
				// 两次调用的 name/avatar/quality 参数完全一致，纯属冗余。它唯一的效果
				// 是给前端制造误报窗口：广播是 500ms 聚合，时机不巧就会把这个瞬态推给
				// 前端，让「未开播」的卡片闪成 IDLE（灰点）——线上实测每 15~20 秒就会
				// 为每个未开播主播闪现一次。更糟的是它会把下面这次写入看到的 prev 状态
				// 改成非 live，从而绕过 builtin_status.go 的下播通知分支。
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
