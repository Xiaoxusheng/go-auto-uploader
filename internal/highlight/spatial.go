package highlight

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"upload/internal/procutil"

)

// 空间分块特征 —— 压缺陷 D/E（礼物特效 / 切近景 / 空镜伪运动）。
//
// 全帧运动量（YAVG）挡不住「整屏像素同时变化」和「怼脸近景」：两者 z 很高但不是跳舞。
// 3×3 块间 std（bstd）才是对症信号：跳舞是肢体局部扫动（块间差异大），
// 礼物特效/场景切换是整屏同步变化（块间差异小）。金标 AUC：bstd 0.917 vs full 0.833
// （_diag/train/_expand_de_auc.py，只做跳舞高光）。
//
// 拓扑与 grid_probe.py 对齐：一次解码 split 成 9 块 crop，各自 `metadata=print:file=`。
// 多实例写独立文件时 key 同名不串台（§19 band_probe 已验证）；不要改成 stderr 多实例。
const (
	spatialGW = 160
	spatialGH = 90
	spatialBW = 53
	spatialBH = 30
	spatialN  = 9
)

var reYAVGFile = regexp.MustCompile(`YAVG=([0-9.]+)`)

// Blocks 是按秒对齐的 3×3 块运动量矩阵：外层为秒，内层固定 9 块（行优先 b0..b8）。
type Blocks [][]float64

// Len 返回秒数。
func (b Blocks) Len() int {
	if b == nil {
		return 0
	}
	return len(b)
}

// WindowBStd 返回 [start,end) 内「各块时间均值」的总体标准差。
//
// 与 grid_auc.window_feats 的 bstd 同定义：先对时间取均值得到 9 维块向量，再对 9 维求 std。
// 跳舞（局部肢体）→ 高；礼物/整屏切换/偏脸近景 → 低。
func WindowBStd(b Blocks, start, end int) float64 {
	if len(b) == 0 {
		return 0
	}
	if start < 0 {
		start = 0
	}
	if end > len(b) {
		end = len(b)
	}
	if end-start < 1 {
		return 0
	}
	var means [spatialN]float64
	n := float64(end - start)
	for i := start; i < end; i++ {
		row := b[i]
		for k := 0; k < spatialN && k < len(row); k++ {
			means[k] += row[k]
		}
	}
	for k := 0; k < spatialN; k++ {
		means[k] /= n
	}
	return stddev(means[:])
}

// SuppressByBStd 丢掉 bstd 过低的段（后处理空间门槛）。
// minBStd<=0 或无空间数据时原样返回。
func SuppressByBStd(segs []Segment, b Blocks, minBStd float64) []Segment {
	if minBStd <= 0 || len(b) == 0 {
		return segs
	}
	out := make([]Segment, 0, len(segs))
	for _, s := range segs {
		if WindowBStd(b, s.Start, s.End) >= minBStd {
			out = append(out, s)
		}
	}
	return out
}

// WindowAC1 返回段内全帧运动量的 lag-1 自相关。
//
// 跳舞是持续摆动（相邻秒强相关）；礼物特效是短促爆发（相关低）。
// grid_de 金标：dance vs gift AUC 0.87，vs closeup 0.79，且**只需 Motion、不依赖空间块**。
// 窗长 <3 或零方差时返回 0。
func WindowAC1(motion []float64, start, end int) float64 {
	if start < 0 {
		start = 0
	}
	if end > len(motion) {
		end = len(motion)
	}
	n := end - start
	if n < 3 {
		return 0
	}
	var sumX, sumY, sumXX, sumYY, sumXY float64
	for i := start; i < end-1; i++ {
		x, y := motion[i], motion[i+1]
		sumX += x
		sumY += y
		sumXX += x * x
		sumYY += y * y
		sumXY += x * y
	}
	m := float64(n - 1)
	cov := sumXY/m - (sumX/m)*(sumY/m)
	vx := sumXX/m - (sumX/m)*(sumX/m)
	vy := sumYY/m - (sumY/m)*(sumY/m)
	if vx < 1e-18 || vy < 1e-18 {
		return 0
	}
	r := cov / math.Sqrt(vx*vy)
	if r < -1 {
		r = -1
	}
	if r > 1 {
		r = 1
	}
	return r
}

// WindowCenterRatio = 中心块 b4 / 四角均值（+eps）。
// 舞者居中局部扫动 → 高；整屏礼物/均匀近景 → 低。grid_de：与 ac1 秩平均后
// dance vs D/E AUC 0.92（单用 rule_close 亦有 0.70）。
func WindowCenterRatio(b Blocks, start, end int) float64 {
	if len(b) == 0 {
		return 0
	}
	if start < 0 {
		start = 0
	}
	if end > len(b) {
		end = len(b)
	}
	if end-start < 1 {
		return 0
	}
	var center, c0, c2, c6, c8 float64
	n := float64(end - start)
	for i := start; i < end; i++ {
		row := b[i]
		if len(row) < 9 {
			continue
		}
		center += row[4]
		c0 += row[0]
		c2 += row[2]
		c6 += row[6]
		c8 += row[8]
	}
	corners := (c0 + c2 + c6 + c8) / (4 * n)
	return (center / n) / (corners + 0.5)
}

// SuppressByAC1 丢掉自相关过低的段（压礼物短促爆发）。min<=0 关闭。
func SuppressByAC1(segs []Segment, motion []float64, minAC1 float64) []Segment {
	if minAC1 <= 0 || len(motion) == 0 {
		return segs
	}
	out := make([]Segment, 0, len(segs))
	for _, s := range segs {
		if WindowAC1(motion, s.Start, s.End) >= minAC1 {
			out = append(out, s)
		}
	}
	return out
}

// SuppressByClose 丢掉中心集中度过低的段。min<=0 或无块数据时关闭。
func SuppressByClose(segs []Segment, b Blocks, minClose float64) []Segment {
	if minClose <= 0 || len(b) == 0 {
		return segs
	}
	out := make([]Segment, 0, len(segs))
	for _, s := range segs {
		if WindowCenterRatio(b, s.Start, s.End) >= minClose {
			out = append(out, s)
		}
	}
	return out
}

// SelectWithBlocks = Select + 空间/时序后处理门槛（o.MinBStd / MinAC1 / MinClose）。
// 门槛为 0 表示关闭；线上未提空间块时 MinBStd/MinClose 自动失效。
// motion 可为 nil（跳过 MinAC1）。
func SelectWithBlocks(scores []float64, motion []float64, blocks Blocks, o Options) []Segment {
	segs := Select(scores, o)
	segs = SuppressByBStd(segs, blocks, o.MinBStd)
	segs = SuppressByAC1(segs, motion, o.MinAC1)
	segs = SuppressByClose(segs, blocks, o.MinClose)
	return segs
}

// BlocksFromFeatures 从特征缓存的 b0..b8 列重建 Blocks；缺列返回 nil。
func BlocksFromFeatures(f *Features) Blocks {
	if f == nil || f.Seconds <= 0 {
		return nil
	}
	cols := make([][]float64, spatialN)
	for k := 0; k < spatialN; k++ {
		c := f.Column("b" + strconv.Itoa(k))
		if c == nil {
			return nil
		}
		cols[k] = c
	}
	out := make(Blocks, f.Seconds)
	for i := 0; i < f.Seconds; i++ {
		row := make([]float64, spatialN)
		for k := 0; k < spatialN; k++ {
			row[k] = cols[k][i]
		}
		out[i] = row
	}
	return out
}

// AttachBlocks 把 Blocks 写入 Features 的 b0..b8 列，便于 export 一并导出。
func AttachBlocks(f *Features, b Blocks) {
	if f == nil || len(b) == 0 {
		return
	}
	n := f.Seconds
	if n > len(b) {
		n = len(b)
	}
	if f.Columns == nil {
		f.Columns = make(map[string][]float64)
	}
	for k := 0; k < spatialN; k++ {
		name := "b" + strconv.Itoa(k)
		col := make([]float64, f.Seconds)
		for i := 0; i < f.Seconds; i++ {
			if i < len(b) && k < len(b[i]) {
				col[i] = b[i][k]
			}
		}
		f.Columns[name] = col
		found := false
		for _, existing := range f.Names {
			if existing == name {
				found = true
				break
			}
		}
		if !found {
			f.Names = append(f.Names, name)
		}
	}
	_ = n
}

// ExtractBlocks 跑一次 ffmpeg，输出 3×3 块运动量并按秒对齐。
// 与 Probe 的全帧 YAVG 同滤镜族（fps=2,scale=160:90,tblend=difference,signalstats）。
func ExtractBlocks(ctx context.Context, ffmpegBin, src string, threads int) (Blocks, error) {
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	tmp, err := os.MkdirTemp("", "hl-spatial-*")
	if err != nil {
		return nil, fmt.Errorf("创建空间临时目录失败: %w", err)
	}
	defer os.RemoveAll(tmp)

	keys := make([]string, 0, spatialN)
	for i := 0; i < spatialN; i++ {
		keys = append(keys, "b"+strconv.Itoa(i))
	}
	parts := make([]string, 0, 2+spatialN)
	parts = append(parts, fmt.Sprintf("[0:v]fps=2,scale=%d:%d,split=%d", spatialGW, spatialGH, spatialN))
	var labels []string
	for _, k := range keys {
		labels = append(labels, "["+k+"]")
	}
	parts[0] += strings.Join(labels, "")

	const sig = "tblend=difference,signalstats"
	for i, k := range keys {
		r, c := i/3, i%3
		crop := fmt.Sprintf("crop=%d:%d:%d:%d", spatialBW, spatialBH, c*spatialBW, r*spatialBH)
		f := filepath.Join(tmp, k+".txt")
		parts = append(parts, fmt.Sprintf("[%s]%s,%s,metadata=print:key=lavfi.signalstats.YAVG:file=%s[o%s]",
			k, crop, sig, f, k))
	}
	graph := strings.Join(parts, ";")

	args := []string{"-hide_banner", "-nostdin", "-nostats", "-loglevel", "error"}
	if threads > 0 {
		args = append(args, "-threads", strconv.Itoa(threads))
	}
	args = append(args, "-i", src, "-an", "-filter_complex", graph)
	for _, k := range keys {
		args = append(args, "-map", "[o"+k+"]")
	}
	args = append(args, "-f", "null", "-")

	cmd := exec.CommandContext(ctx, ffmpegBin, args...)
	procutil.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		extra := strings.TrimSpace(string(out))
		if len(extra) > 400 {
			extra = extra[len(extra)-400:]
		}
		return nil, fmt.Errorf("ffmpeg 空间提取失败: %w | %s", err, extra)
	}

	series := make([][]float64, spatialN)
	seconds := 0
	for i, k := range keys {
		vals, perr := readYAVGFile(filepath.Join(tmp, k+".txt"))
		if perr != nil {
			return nil, perr
		}
		series[i] = bucketBySecond(vals, maxSecond(vals))
		if len(series[i]) > seconds {
			seconds = len(series[i])
		}
	}
	if seconds == 0 {
		return nil, fmt.Errorf("空间提取未解析到采样点")
	}
	// 对齐到最短长度，避免某块缺尾导致越界
	for i := range series {
		if len(series[i]) < seconds {
			seconds = len(series[i])
		}
	}
	outBlocks := make(Blocks, seconds)
	for s := 0; s < seconds; s++ {
		row := make([]float64, spatialN)
		for k := 0; k < spatialN; k++ {
			row[k] = series[k][s]
		}
		outBlocks[s] = row
	}
	return outBlocks, nil
}

func maxSecond(samples []sample) int {
	seconds := 0
	for _, s := range samples {
		if n := int(s.t) + 1; n > seconds {
			seconds = n
		}
	}
	return seconds
}

func readYAVGFile(path string) ([]sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读空间采样失败 %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	var out []sample
	var cur float64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if m := rePtsTime.FindStringSubmatch(line); m != nil {
			cur, _ = strconv.ParseFloat(m[1], 64)
			continue
		}
		if m := reYAVGFile.FindStringSubmatch(line); m != nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil && !math.IsNaN(v) {
				out = append(out, sample{cur, v})
			}
		}
	}
	return out, nil
}
