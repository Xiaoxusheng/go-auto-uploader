package highlight

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type sample struct {
	t float64
	v float64
}

var (
	rePtsTime = regexp.MustCompile(`pts_time:([0-9.]+)`)
	reYAVG    = regexp.MustCompile(`YAVG=([0-9.]+)`)
	reRMS     = regexp.MustCompile(`RMS_level=(-?[0-9.]+)`)
)

// 视频缩到 160x90、2 帧/秒再做帧差；音频降到 8kHz、每秒一块测 RMS。
// 缩分辨率能省下滤镜开销，但瓶颈在 H.264 解码，省不掉。
const (
	motionFilter = "fps=2,scale=160:90,tblend=all_mode=difference,signalstats,metadata=print:key=lavfi.signalstats.YAVG"
	audioFilter  = "aresample=8000,asetnsamples=n=8000,astats=metadata=1:reset=1,ametadata=print:key=lavfi.astats.Overall.RMS_level"
)

// probeStderrTail 失败时保留的 stderr 行数。
//
// ffmpeg 的关键报错通常压在末尾，而正常的采样点输出（每秒好几行）会把它彻底淹没。
// 不透传这几行的话，失败就只剩一个 "exit status 255"，完全无法诊断 ——
// 线上就吃过这个亏：只能把整条命令搬到服务器上手动复现才知道文件其实是好的。
const probeStderrTail = 8

// Probe 跑一次 ffmpeg，同时解出画面运动量与音频能量，并聚合到「每秒一个值」。
//
// 实测约 6~7 倍实时（1080x1920 竖屏 30fps 素材，899 秒跑 2 分 15 秒），
// 1 小时切片约 9 分钟，对离线后处理足够。
// threads <= 0 时不传 -threads，交给 ffmpeg 自行决定。
func Probe(ctx context.Context, ffmpegBin, src string, threads int) (*Series, error) {
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	args := []string{"-hide_banner", "-nostdin"}
	if threads > 0 {
		args = append(args, "-threads", strconv.Itoa(threads))
	}
	args = append(args,
		"-i", src,
		"-map", "0:v:0", "-vf", motionFilter,
		"-map", "0:a:0?", "-af", audioFilter,
		"-f", "null", "-",
	)

	cmd := exec.CommandContext(ctx, ffmpegBin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("获取 ffmpeg 输出失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 ffmpeg 失败: %w", err)
	}

	var (
		motion, audio []sample
		curVT, curAT  float64
		tail          = newTailBuffer(probeStderrTail)
	)
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// 两路 metadata 的 filter 名前缀不同，靠它区分归属，避免时间戳串台。
		// Parsed_ametadata_ 不含 Parsed_metadata_ 子串，Contains 判定是安全的。
		isAudio := strings.Contains(line, "Parsed_ametadata_")
		isVideo := !isAudio && strings.Contains(line, "Parsed_metadata_")
		if !isAudio && !isVideo {
			// 采样点以外的行才可能是报错，而采样点行量极大，不该挤占缓冲
			tail.add(line)
			continue
		}
		if m := rePtsTime.FindStringSubmatch(line); m != nil {
			t, _ := strconv.ParseFloat(m[1], 64)
			if isAudio {
				curAT = t
			} else {
				curVT = t
			}
			continue
		}
		if m := reYAVG.FindStringSubmatch(line); m != nil && isVideo {
			if v, perr := strconv.ParseFloat(m[1], 64); perr == nil {
				motion = append(motion, sample{curVT, v})
			}
			continue
		}
		if m := reRMS.FindStringSubmatch(line); m != nil && isAudio {
			if v, perr := strconv.ParseFloat(m[1], 64); perr == nil {
				audio = append(audio, sample{curAT, v})
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		if extra := tail.String(); extra != "" {
			return nil, fmt.Errorf("ffmpeg 分析失败: %w | %s", err, extra)
		}
		return nil, fmt.Errorf("ffmpeg 分析失败: %w", err)
	}

	seconds := 0
	for _, s := range motion {
		if n := int(s.t) + 1; n > seconds {
			seconds = n
		}
	}
	for _, s := range audio {
		if n := int(s.t) + 1; n > seconds {
			seconds = n
		}
	}
	if seconds == 0 {
		return nil, fmt.Errorf("未解析到采样点：文件可能没有视频流，或 ffmpeg 缺少 signalstats/astats 滤镜")
	}

	return &Series{
		Motion: bucketBySecond(motion, seconds),
		Audio:  bucketBySecond(audio, seconds),
	}, nil
}

// bucketBySecond 把不规则采样点按秒取均值；空档填前值，
// 避免音频/视频短暂中断时该秒被当成 0 而误判成「静音」。
func bucketBySecond(samples []sample, seconds int) []float64 {
	sum := make([]float64, seconds)
	cnt := make([]int, seconds)
	for _, s := range samples {
		i := int(s.t)
		if i < 0 || i >= seconds {
			continue
		}
		sum[i] += s.v
		cnt[i]++
	}
	out := make([]float64, seconds)
	last := 0.0
	haveLast := false
	for i := range out {
		if cnt[i] > 0 {
			out[i] = sum[i] / float64(cnt[i])
			last, haveLast = out[i], true
		} else if haveLast {
			out[i] = last
		}
	}
	return out
}

// tailBuffer 只保留最后 max 行文本，用于命令失败时还原上下文。
// 抽成独立类型是为了能纯单测（真跑 ffmpeg 造一个「成功但末尾报错」的场景不现实）。
type tailBuffer struct {
	lines []string
	max   int
}

func newTailBuffer(max int) *tailBuffer {
	if max < 0 {
		max = 0
	}
	return &tailBuffer{lines: make([]string, 0, max), max: max}
}

// add 追加一行，超出容量时丢弃最旧的一行。
func (b *tailBuffer) add(line string) {
	if b == nil || b.max == 0 {
		return
	}
	if len(b.lines) == b.max {
		copy(b.lines, b.lines[1:])
		b.lines = b.lines[:b.max-1]
	}
	b.lines = append(b.lines, line)
}

// String 用 " / " 连接保留下来的行；无内容时返回空串。
func (b *tailBuffer) String() string {
	if b == nil || len(b.lines) == 0 {
		return ""
	}
	return strings.Join(b.lines, " / ")
}
