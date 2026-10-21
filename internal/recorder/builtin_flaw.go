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

	"upload/internal/procutil"
)

// ============================================================================
// 花屏（色度丢失）检测与自动重录
// ============================================================================
//
// 背景：录制走 `-c:v copy` 直通落盘（见 builtin_ffmpeg.go 的 videoCodecArgs），
// ffmpeg 全程不解码视频 —— 上游推流端编码异常、或网络丢包导致参考帧缺失时，
// 坏数据会被原样写进 .ts，ffmpeg 不报任何错。播放时表现为整块绿/紫/粉的
// 色度丢失（俗称花屏），整段录像报废。所以检测只能自己抽帧做，不能靠 stderr。
//
// 思路：从当前正在写的分片尾部抽样若干帧，缩到 64x36 后统计「异常纯色像素占比」。
// 正常直播画面几乎不可能整帧都是同一纯色，而色度丢失型花屏恰好就是整帧纯色，
// 因此该特征区分度极高。
//
// 实测标定（_diag/flaw_poc.py，ffmpeg 抽帧 → rgb24 → 占比统计）：
//
//	真实直播 ts（正常）       green=0.000
//	绿幕直播间（20% 人物）    green=0.792   ← 阈值 0.90 的安全边界
//	整屏绿屏（花屏）          green=1.000
const (
	flawProbeW = 64 // 抽样帧宽
	flawProbeH = 36 // 抽样帧高

	// flawProbeFSZ rgb24 单帧字节数
	flawProbeFSZ = flawProbeW * flawProbeH * 3

	// flawTailBytes 只读分片尾部这么多字节。
	//
	// ⚠️ 必须是 12MB，不能更小：TS 从任意位置截断后，H.264 的 SPS/PPS 只在 I 帧
	// 前出现，尾部窗口若覆盖不到 I 帧就一帧都解不出来（实测 2MB 时报
	// `non-existing PPS 0 referenced` → `no frame!`，12MB 才稳定有 I 帧）。
	// 这个取值与 extractBuiltinCoverFromLocalFile 的抽帧读取量一致，是实测出来的。
	flawTailBytes = 12 << 20 // 12MB

	// flawMinSegBytes 小于此大小的分片跳过检测（刚切出来的新片还没数据）
	flawMinSegBytes = 512 << 10 // 512KB

	// flawProbeFrames 每次参与判定的帧数（取最靠近文件尾部的若干帧）
	flawProbeFrames = 3

	// flawMaxProbeFrames 单次抽样最多解这么多帧（fps=1，即最多覆盖 2 分钟），
	// 防止低码率长分片把输出撑大。
	flawMaxProbeFrames = 120

	// flawProbeTimeout 单次探测的 ffmpeg 硬超时。服务器弱机 + 只解 3 帧，20s 绰绰有余；
	// 超时兜底是为了防止 ffmpeg 卡死把检测协程拖住（进而拖住整个录制循环）。
	flawProbeTimeout = 20 * time.Second

	// flawRestartCooldown 两次「花屏重录」之间的最小间隔。重录本身要重新拉流、
	// 新分片还要攒够数据才能再检测，冷却期避免状态抖动。
	flawRestartCooldown = 60 * time.Second
)

// errFlawSegmentTooSmall 分片还没攒够数据。属正常情况（刚切出来的新片），
// 调用方应静默跳过而不是当成探测故障刷日志。
var errFlawSegmentTooSmall = errors.New("分片过小")

// FlawVerdict 一次花屏检测的结论。
type FlawVerdict struct {
	Flawed bool    // 是否判定为花屏
	Ratio  float64 // 命中的最大异常纯色像素占比（0~1）
	Kind   string  // "green"（绿屏）/ "magenta"（紫粉屏）/ ""
	Frames int     // 成功分析的抽样帧数
	Err    error   // 探测错误：分片太小、抽不到帧、ffmpeg 失败等
}

// String 便于日志输出。
func (v FlawVerdict) String() string {
	if v.Err != nil {
		return fmt.Sprintf("检测失败(%v)", v.Err)
	}
	if !v.Flawed {
		return fmt.Sprintf("正常(峰值占比 %.2f, %d 帧)", v.Ratio, v.Frames)
	}
	return fmt.Sprintf("花屏/%s(占比 %.2f, %d 帧)", v.Kind, v.Ratio, v.Frames)
}

// probeChromaFlaw 从 ts 分片尾部抽样 wantFrames 帧，判定是否为色度丢失型花屏。
//
// 读尾部 12MB（见 flawTailBytes 的说明），按 fps=1 抽帧后只取**最靠近文件尾部**
// 的 wantFrames 帧参与判定 —— 尾部帧才代表「当下画面」。
// ratioThreshold 为异常纯色像素占比阈值；wantFrames < 1 时回落 1。
// 任何探测异常都通过 FlawVerdict.Err 返回，绝不 panic、绝不阻塞录制主循环。
func probeChromaFlaw(tsPath string, wantFrames int, ratioThreshold float64) FlawVerdict {
	if wantFrames < 1 {
		wantFrames = 1
	}
	if ratioThreshold <= 0 || ratioThreshold > 1 {
		ratioThreshold = 0.9
	}

	buf, err := readTail(tsPath, flawTailBytes)
	if err != nil {
		return FlawVerdict{Err: err}
	}
	if len(buf) < flawMinSegBytes {
		return FlawVerdict{Err: fmt.Errorf("%w（%d 字节）", errFlawSegmentTooSmall, len(buf))}
	}

	ctx, cancel := context.WithTimeout(context.Background(), flawProbeTimeout)
	defer cancel()

	// -threads 1：检测是后台任务，服务器弱机上绝不能跟录制抢 CPU。
	// +genpts+igndts：TS 尾部切片时间戳常不连续，与封面抽帧同款兜底（否则 exit 69）。
	cmd := exec.CommandContext(ctx, builtinFfmpegPath,
		"-v", "error",
		"-fflags", "+genpts+igndts",
		"-threads", "1",
		"-i", "pipe:0",
		"-vf", fmt.Sprintf("fps=1,scale=%d:%d", flawProbeW, flawProbeH),
		"-frames:v", fmt.Sprintf("%d", flawMaxProbeFrames),
		"-f", "rawvideo",
		"-pix_fmt", "rgb24",
		"pipe:1",
	)
	procutil.HideWindow(cmd)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return FlawVerdict{Err: err}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return FlawVerdict{Err: err}
	}
	if err := cmd.Start(); err != nil {
		return FlawVerdict{Err: err}
	}

	// ⚠️ 写 stdin 与读 stdout 必须并发，否则死锁：
	// rawvideo 输出（最多 120 帧 ≈ 830KB）远超管道缓冲区，ffmpeg 写 stdout 会阻塞，
	// 进而停止读取 stdin，而我们还堵在 stdin.Write 上 → 双方互等，
	// 最终被 ctx 超时杀掉（Windows 上表现为 exit status 1 且 stderr 为空，极难排查）。
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = stdin.Write(buf)
		_ = stdin.Close()
	}()

	raw, readErr := io.ReadAll(stdout)
	wg.Wait()
	waitErr := cmd.Wait()

	n := len(raw) / flawProbeFSZ
	if n == 0 {
		if readErr != nil {
			return FlawVerdict{Err: readErr}
		}
		if waitErr != nil {
			return FlawVerdict{Err: fmt.Errorf("抽帧失败: %v | %s", waitErr, tailStr(stderr.String(), 160))}
		}
		return FlawVerdict{Err: fmt.Errorf("未取到任何帧")}
	}

	// 只取最靠近文件尾部（=最贴近当下画面）的若干帧判定。
	// 12MB 窗口可能覆盖十几秒，取尾部帧才能反映「现在是否还在花屏」。
	start := n - wantFrames
	if start < 0 {
		start = 0
	}
	used := n - start

	v := FlawVerdict{Frames: used}
	var hit, greenHit, magentaHit int
	for i := start; i < n; i++ {
		green, magenta := analyzeFlawFrame(raw[i*flawProbeFSZ : (i+1)*flawProbeFSZ])
		if green > v.Ratio {
			v.Ratio = green
		}
		if magenta > v.Ratio {
			v.Ratio = magenta
		}
		if green >= ratioThreshold || magenta >= ratioThreshold {
			hit++
			if green >= magenta {
				greenHit++
			} else {
				magentaHit++
			}
		}
	}

	// 判定：≥2/3 参与帧命中；只解出 1 帧时该帧命中即可。
	if used >= 2 {
		v.Flawed = hit*3 >= used*2
	} else {
		v.Flawed = hit == 1
	}
	if v.Flawed {
		if greenHit >= magentaHit {
			v.Kind = "green"
		} else {
			v.Kind = "magenta"
		}
	}
	return v
}

// analyzeFlawFrame 统计单帧里「异常纯色」像素的占比。
//
// 判据（RGB 通道，阈值经 POC 实测标定）：
//   - 绿屏：G 明显主导 —— G>90 且比 R/B 都高 40 以上
//   - 紫粉屏：R、B 同时高而 G 低 —— min(R,B)>90 且比 G 高 40 以上
//
// 正常画面里这两种颜色都可能局部出现（绿幕、粉色调灯光），但不可能整帧都是。
func analyzeFlawFrame(f []byte) (green, magenta float64) {
	n := flawProbeW * flawProbeH
	if len(f) < n*3 {
		return 0, 0
	}
	var g, m int
	for i := 0; i < n; i++ {
		r := int(f[i*3])
		gg := int(f[i*3+1])
		b := int(f[i*3+2])
		switch {
		case gg > 90 && gg-max(r, b) > 40:
			g++
		case min(r, b) > 90 && min(r, b)-gg > 40:
			m++
		}
	}
	return float64(g) / float64(n), float64(m) / float64(n)
}

// readTail 读取文件尾部最多 max 字节；文件比 max 小时读全量。
func readTail(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	var off int64
	if size > max {
		off = size - max
	}
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}

// latestBuiltinSegment 返回目录下指定前缀、最新的、大小达标的 .ts 分片路径。
//
// ⚠️ 必须用 os.Stat 取大小，不能用 DirEntry.Info()：Windows 上 ReadDir 返回的
// DirEntry 带的是目录枚举时的缓存信息，对「正在被 ffmpeg 写入」的文件会一直报
// size=0，导致正在录制的分片被整个跳过（Linux 用 lstat 实时，没这个问题）。
// 这个坑在本地 Windows 上 100% 复现，且现象是「检测静默不工作」，极难排查。
func latestBuiltinSegment(dir, prefix string, minSize int64) string {
	files, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var latest string
	var maxTime time.Time
	for _, f := range files {
		if f.IsDir() || !strings.HasPrefix(f.Name(), prefix) || !strings.HasSuffix(f.Name(), ".ts") {
			continue
		}
		full := filepath.Join(dir, f.Name())
		info, err := os.Stat(full)
		if err != nil || info.Size() < minSize {
			continue
		}
		if info.ModTime().After(maxTime) {
			maxTime = info.ModTime()
			latest = full
		}
	}
	return latest
}

// markBadSegment 把命中的坏分片改名加上 .flaw 后缀。
//
// 用「改名保留」而不是删除：坏分片可能仍有部分可用内容，且用户排查花屏成因时需要样本。
// 后缀不是 .ts，因此不会被上传流程、高光分析、统计口径当作正常录像收走。
// 必须在 ffmpeg 进程退出后再调用（录制中文件被占用，Windows 上改名会失败）。
func markBadSegment(segPath string) {
	if segPath == "" {
		return
	}
	if _, err := os.Stat(segPath); err != nil {
		return
	}
	newPath := segPath + ".flaw"
	if err := os.Rename(segPath, newPath); err != nil {
		log.Printf("⚠️ [花屏] 坏分片标记失败 %s: %v", segPath, err)
		return
	}
	log.Printf("🏷️ [花屏] 坏分片已标记保留: %s", newPath)
}

// ============================================================================
// 花屏重录标记与单场计数
// ============================================================================

var (
	// builtinFlawRestart 花屏触发的「立即重录」标记（key=platform_roomID）。
	// 语义与 builtinConfigRestart 一致：让监控循环知道这是主动重录，
	// 不是断流 —— 不报下播、也不走 30 秒退避。
	builtinFlawRestart sync.Map

	// builtinFlawCount 本场已自动重录次数（key=platform_roomID）。
	// 上游推流端编码异常时重连无效，靠这个上限防止无限重录。
	builtinFlawCount sync.Map
)

// markFlawRestart 标记某任务因花屏被主动中断。
func markFlawRestart(key string) { builtinFlawRestart.Store(key, true) }

// clearFlawRestart 清除标记并返回此前是否存在。
func clearFlawRestart(key string) bool {
	_, ok := builtinFlawRestart.LoadAndDelete(key)
	return ok
}

// flawCount 返回本场已自动重录次数。
func flawCount(key string) int {
	if v, ok := builtinFlawCount.Load(key); ok {
		if n, ok := v.(int); ok {
			return n
		}
	}
	return 0
}

// setFlawCount 记录本场已自动重录次数。
func setFlawCount(key string, n int) { builtinFlawCount.Store(key, n) }

// resetFlawCount 复位本场重录计数（下播/任务移除时调用，下一场从零开始）。
func resetFlawCount(key string) { builtinFlawCount.Delete(key) }
