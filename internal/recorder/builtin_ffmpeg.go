package recorder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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

// extractBuiltinCoverFromLocalFile 旁路抽帧大法：只读取本地录像尾部少量字节流交由内存端 ffmpeg 解析。
// 此方案避免了全量读取大文件导致的 I/O 阻塞，极大降低系统 CPU 负担，并安全处理了动态水印的烧录。
// 返回值：bool 表示是否成功提取并生成封面图。
// ✨ 修改内容：增加 anchorName 参数，当未配置全局水印文本时，自动回退使用主播名称作为水印。
func extractBuiltinCoverFromLocalFile(dir, prefix, coverPath, anchorName string) bool {
	// 读取目标目录下所有文件
	files, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	var latestFile string
	var maxTime time.Time

	// 遍历筛选，定位当前录制任务最新的有效 .ts 视频分片
	for _, f := range files {
		if !f.IsDir() && strings.HasPrefix(f.Name(), prefix) && strings.HasSuffix(f.Name(), ".ts") {
			info, err := f.Info()
			// 过滤掉小于 512KB 的无效碎片，找到修改时间最晚的文件
			if err == nil && info.ModTime().After(maxTime) && info.Size() > 512*1024 {
				maxTime = info.ModTime()
				latestFile = filepath.Join(dir, f.Name())
			}
		}
	}

	// 如果没有找到符合条件的录像文件，直接退出
	if latestFile == "" {
		return false
	}

	// 打开定位到的最新录像文件
	file, err := os.Open(latestFile)
	if err != nil {
		return false
	}
	defer file.Close()

	stat, _ := file.Stat()
	size := stat.Size()

	// 性能优化核心：仅截取文件尾部最多 5MB 的字节流数据，防范大文件 I/O 卡死
	readSize := int64(5 * 1024 * 1024)
	if size < readSize {
		readSize = size
	}

	// 将文件指针移动到倒数 readSize 的位置
	file.Seek(-readSize, 2)
	buf := make([]byte, readSize)

	// 将尾部数据载入内存缓冲区
	_, err = io.ReadFull(file, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false
	}

	// 基础视频滤镜：精准抓取第一个关键帧 (I-frame)，避免花屏和解码黑屏
	vfFilter := "select='eq(pict_type,I)'"

	// 截图水印：复用 PrepareDrawtextFilter
	if builtinConfig != nil && builtinConfig.WatermarkEnable {
		drawtext, textFile, werr := prepareBuiltinDrawtextFilter(anchorName, "shot")
		if werr != nil {
			log.Printf("[BUILTIN] ⚠️ 截图水印准备失败，跳过水印: %v", werr)
		} else {
			defer os.Remove(textFile)
			vfFilter += "," + drawtext
		}
	}

	// 构建 FFmpeg 执行指令，通过 pipe:0 读取内存流，直接输出 png 图片
	// +genpts/igndts：TS 尾部切片时间戳常不连续，避免 exit 69
	cmd := exec.Command(builtinFfmpegPath,
		"-y",
		"-fflags", "+genpts+igndts",
		"-i", "pipe:0",
		"-vf", vfFilter,
		"-frames:v", "1",
		"-c:v", "png",
		"-f", "image2",
		coverPath,
	)

	// 挂载错误日志捕获缓冲池
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	// 获取管道输入口
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false
	}

	// 启动 FFmpeg 进程
	if err := cmd.Start(); err != nil {
		log.Printf("[BUILTIN] ❌ FFmpeg 启动失败: %v", err)
		return false
	}

	// 高速将内存字节注入 FFmpeg 管道，写完立即关闭 stdin 触发 EOF 处理
	stdin.Write(buf)
	stdin.Close()

	// 等待底层图像渲染和编码结束
	err = cmd.Wait()

	// TS 尾部可能没有 I 帧导致 select 0 帧退出；去掉 select 再抽一帧兜底
	if err != nil {
		vfRetry := "null"
		if builtinConfig != nil && builtinConfig.WatermarkEnable {
			if drawtext, textFile, werr := prepareBuiltinDrawtextFilter(anchorName, "shot2"); werr == nil {
				defer os.Remove(textFile)
				vfRetry = drawtext
			}
		}
		retry := exec.Command(builtinFfmpegPath,
			"-y",
			"-fflags", "+genpts+igndts",
			"-i", "pipe:0",
			"-vf", vfRetry,
			"-frames:v", "1",
			"-c:v", "png",
			"-f", "image2",
			coverPath,
		)
		var retryErr bytes.Buffer
		retry.Stderr = &retryErr
		stdin2, pipeErr := retry.StdinPipe()
		if pipeErr == nil {
			if startErr := retry.Start(); startErr == nil {
				stdin2.Write(buf)
				stdin2.Close()
				if retry.Wait() == nil {
					return true
				}
			}
		}
	}

	// ✨ 核心追踪日志：只要开启了水印，就强行把 FFmpeg 的底层报错池抖出来！
	// 无论截帧成功与否，只要检测到 "No such filter" 或滤镜相关的错误，立即高亮暴露问题
	if builtinConfig != nil && builtinConfig.WatermarkEnable {
		stderrStr := stderrBuf.String()
		if strings.Contains(stderrStr, "No such filter") {
			log.Printf("[BUILTIN-ERROR] 💀 致命错误：你的 FFmpeg 未编译 drawtext 滤镜 (缺少 libfreetype)！请重新安装完整版 FFmpeg！")
		} else if strings.Contains(stderrStr, "Parsed_drawtext") || strings.Contains(stderrStr, "Error") || err != nil {
			//log.Printf("[BUILTIN-DEBUG] ⚠️ FFmpeg 水印滤镜执行追踪：\n%s", stderrStr)
		}
	}

	if err != nil {
		log.Printf("[BUILTIN] ❌ FFmpeg 截图底层进程异常崩溃: %v\n", err)
		return false
	}

	return true
}

// normalizeFFmpegFontColor 将前端十六进制色（含 #RRGGBB / #RRGGBBAA）归一为 drawtext 可识别的 0x 形式
func normalizeFFmpegFontColor(c string) string {
	c = strings.TrimSpace(c)
	if c == "" {
		return "white@0.95"
	}
	if strings.HasPrefix(c, "#") {
		return "0x" + strings.TrimPrefix(c, "#")
	}
	return c
}

// findBuiltinFontPath 按可执行文件目录 → 工作目录的顺序寻找中文字体
func findBuiltinFontPath() string {
	return FindFontPath()
}

// buildBuiltinWatermarkText 组装「前缀（默认主播名）+ 动态时间」水印文本
func buildBuiltinWatermarkText(anchorName string) string {
	if builtinConfig == nil {
		return strings.TrimSpace(anchorName)
	}
	return BuildWatermarkText(watermarkStyleFromConfig(), anchorName)
}

func watermarkStyleFromConfig() WatermarkStyle {
	if builtinConfig == nil {
		return WatermarkStyle{Format: "%Y-%m-%d %H:%M:%S", Position: "bottom-right", FontSize: 38, FontColor: "white@0.95"}
	}
	return StyleFrom(
		builtinConfig.WatermarkText,
		builtinConfig.WatermarkFormat,
		builtinConfig.WatermarkPosition,
		builtinConfig.WatermarkFontColor,
		builtinConfig.WatermarkFontSize,
	)
}

// builtinDrawtextPosStr 根据配置返回九宫格坐标
func builtinDrawtextPosStr() string {
	if builtinConfig == nil {
		return "x=w-tw-20:y=h-th-20"
	}
	return DrawtextPos(builtinConfig.WatermarkPosition)
}

// prepareBuiltinDrawtextFilter 生成 drawtext 滤镜串，并落盘临时 textfile。
// 返回 filter、临时文件绝对路径（调用方负责 Remove）。
func prepareBuiltinDrawtextFilter(anchorName, tag string) (filter string, textFile string, err error) {
	if builtinConfig == nil {
		return "", "", fmt.Errorf("builtinConfig 未初始化")
	}
	return PrepareDrawtextFilter(watermarkStyleFromConfig(), anchorName, tag)
}

// BuiltinRecordStream 调动底层 FFmpeg 进程并将推流直通本地文件，增加了高度强化的上下文状态管控防止僵尸进程
// flags 控制本任务是否落盘录像 / 是否旁路截屏。
func RecordStream(ctx context.Context, streamURL, platformName, roomID, anchorName, avatar, quality string, segmentTime int, flags BuiltinTaskFlags) {
	if !flags.Record && !flags.Screenshot {
		log.Printf("⚪ [空转模式] %s | %s 录屏与截屏均已关闭，仅保持开播探测", platformName, anchorName)
		updateBuiltinStatus(platformName, roomID, anchorName, avatar, quality, "监控中")
		// 等待 ctx 取消或短暂休眠，避免外层循环疯狂打流
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
		return
	}

	statusLabel := "录制中"
	if !flags.Record && flags.Screenshot {
		statusLabel = "截屏中"
	}
	updateBuiltinStatus(platformName, roomID, anchorName, avatar, quality, statusLabel)

	safeName := sanitizeBuiltinFileName(anchorName)
	if safeName == "" {
		safeName = roomID
	}

	baseDir := getBuiltinSavePath()

	// ✨ 修改内容：提取当前日期并构建日期命名的子文件夹作为落盘根目录
	dateStr := time.Now().Format("2006-01-02")
	outDir := filepath.Join(baseDir, safeName, dateStr)
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

	// 抖音 FLV 节点抖动常见：放宽读超时到 60s，并开启 HTTP 断线重连
	args = append(args,
		"-rw_timeout", "60000000",
		"-reconnect", "1",
		"-reconnect_streamed", "1",
		"-reconnect_delay_max", "30",
		"-analyzeduration", "5000000",
		"-probesize", "5000000",
		"-i", streamURL,
	)
	args = append(args, "-map", "0:v?", "-map", "0:a?", "-ignore_unknown")

	// 仅截屏：强制短分片，抽帧后删片，磁盘不长期保留视频
	effectiveSegment := segmentTime
	if !flags.Record && flags.Screenshot {
		if effectiveSegment <= 0 || effectiveSegment > 2 {
			effectiveSegment = 2
		}
	}

	// 视频烧录水印：开启时必须重编码（libx264），关闭则零拷贝 copy
	useVideoWM := flags.Record && builtinConfig != nil && builtinConfig.VideoWatermarkEnable
	var videoCodecArgs []string
	if useVideoWM {
		vf, textFile, werr := prepareBuiltinDrawtextFilter(anchorName, "vid")
		if werr != nil {
			log.Printf("[BUILTIN] ⚠️ 视频水印准备失败，回退为无水印 copy 录制: %v", werr)
			videoCodecArgs = []string{"-c:v", "copy"}
		} else {
			defer os.Remove(textFile)
			videoCodecArgs = []string{"-vf", vf, "-c:v", "libx264", "-preset", "veryfast", "-crf", "23"}
			log.Printf("   🎬 视频画面烧录水印已启用（%s）", WatermarkWallClockNote)
		}
	} else {
		videoCodecArgs = []string{"-c:v", "copy"}
	}

	if effectiveSegment > 0 {
		outPath = filepath.Join(outDir, fmt.Sprintf("%s_%s_%%03d.ts", safeName, timestamp))
		args = append(args, videoCodecArgs...)
		args = append(args, "-c:a", "copy", "-f", "segment", "-segment_time", fmt.Sprintf("%d", effectiveSegment*60), "-reset_timestamps", "1", outPath)
	} else {
		outPath = filepath.Join(outDir, fmt.Sprintf("%s_%s.ts", safeName, timestamp))
		args = append(args, videoCodecArgs...)
		args = append(args, "-c:a", "copy", "-f", "mpegts", outPath)
	}

	fileName := fmt.Sprintf("%s_%s.png", platformName, roomID)
	coverDir := filepath.Join(".", "covers")
	os.MkdirAll(coverDir, os.ModePerm)
	coverPath := filepath.Join(coverDir, fileName)

	modeDesc := fmt.Sprintf("录屏=%v 截屏=%v", flags.Record, flags.Screenshot)
	log.Printf("\n🟢 [开始录制] 平台: %s | 主播: %s | 画质: %s | %s\n   📂 TS视频存至: %s", platformName, anchorName, formatBuiltinQualityName(quality), modeDesc, outPath)
	if flags.Screenshot {
		log.Printf("   📸 旁路截图机制已启动")
	}

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
	if flags.Screenshot {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var lastModTime time.Time
			ticker := time.NewTicker(20 * time.Second)
			defer ticker.Stop()

			coverCount := 1
			filePrefix := fmt.Sprintf("%s_%s", safeName, timestamp)

			time.Sleep(5 * time.Second)
			extractBuiltinCoverFromLocalFile(outDir, filePrefix, coverPath, anchorName)

			for {
				select {
				case <-recordCtx.Done(): // 收到严格终止指令，立刻结束旁路监测
					return
				case <-ticker.C:
					extracted := extractBuiltinCoverFromLocalFile(outDir, filePrefix, coverPath, anchorName)

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
									coverCount++
								}

								key := platformName + "_" + roomID
								if existing, ok := builtinStatusMap.Load(key); ok {
									task := *(existing.(*BuiltinTaskStatus))
									task.Avatar = fmt.Sprintf("/covers/%s?t=%d", fileName, time.Now().UnixMilli())
									builtinStatusMap.Store(key, &task)
									triggerBuiltinBroadcast()
								}

								// 仅截屏模式：抽帧成功后清理已冷却的临时 TS
								if !flags.Record {
									cleanupScreenshotTempSegments(outDir, filePrefix, false)
								}
							}
						}
					} else if !flags.Record {
						// 抽帧失败也要清冷却片，避免整场直播堆积临时 TS
						cleanupScreenshotTempSegments(outDir, filePrefix, false)
					}
				}
			}
		}()
	}

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

	// 仅截屏：结束后强制清空本场全部残留临时 TS（含 2 分钟内的在写文件）
	if !flags.Record {
		cleanupScreenshotTempSegments(outDir, fmt.Sprintf("%s_%s", safeName, timestamp), true)
	}

	// 自动清理小于 1KB 的失效封面图残余
	if info, err := os.Stat(coverPath); err == nil && info.Size() < 1024 {
		os.Remove(coverPath)
	}

	updateBuiltinStatus(platformName, roomID, anchorName, avatar, quality, "未开播等待中")
}

// cleanupScreenshotTempSegments 删除仅截屏模式下的临时 TS。
// force=false：仅删除已冷却（>2 分钟）的分片，避免误删仍在写入的文件；
// force=true：会话结束时强制清空该前缀下全部残留，防止泄漏。
func cleanupScreenshotTempSegments(dir, filePrefix string, force bool) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasPrefix(f.Name(), filePrefix) || !strings.HasSuffix(f.Name(), ".ts") {
			continue
		}
		if !force {
			info, err := f.Info()
			if err != nil || time.Since(info.ModTime()) < 2*time.Minute {
				continue
			}
		}
		_ = os.Remove(filepath.Join(dir, f.Name()))
	}
}

// wrapperStartMonitorIfNotRunning 将单个监控目标封入守护协程，并实现任务防重载冲突及心跳检测重连功能
