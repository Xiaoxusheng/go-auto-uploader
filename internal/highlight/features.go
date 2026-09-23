package highlight

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 全字段特征提取 —— 供离线训练与评估使用，主链路暂不依赖。
//
// 与 Probe 的唯一区别：Probe 在 metadata/ametadata 上带了 `:key=` 过滤，只 print
// 一个字段（YAVG / RMS_level）；本文件把 key 过滤去掉，于是**同一次解码**就能拿到
// signalstats 的 27 个字段与 astats Overall 的 40 余个字段，而耗时几乎不变 ——
// 真正的瓶颈是 H.264 解码，滤镜多算几个标量是免费的。
//
// 刻意维持「一个 metadata 实例 + 一个 ametadata 实例」的拓扑：
// 一旦同名 filter 挂多个实例，ffmpeg 会输出 `Parsed_metadata_0_` / `Parsed_metadata_1_`，
// 二者都 Contains("Parsed_metadata_")，无法区分归属（probe.go 注释记录了这个坑）。
// 所以扩展特征只能靠「放开 key 过滤」，不能靠「多挂实例」。
const (
	fullVideoFilter = "fps=2,scale=160:90,tblend=all_mode=difference,signalstats,metadata=print"
	fullAudioFilter = "aresample=8000,asetnsamples=n=8000,astats=metadata=1:reset=1,ametadata=print"
)

var (
	reFullPtsTime = regexp.MustCompile(`pts_time:([0-9.]+)`)
	reFullKV      = regexp.MustCompile(`^(lavfi\.[A-Za-z0-9_.]+)=(-?[0-9.eE+-]+)$`)
)

// Features 是按秒对齐的多维特征矩阵。
//
// 用「列名 → 每秒值」而不是「每秒 → 列名」，因为训练侧（Python/pandas）更习惯前者，
// 且 JSON 序列化后体积更小。缺失的秒用前值填充（与 bucketBySecond 行为一致），
// 避免音视频短暂中断的秒被当成 0 而误判成「静音」或「静止」。
type Features struct {
	Seconds int                  `json:"seconds"`
	Names   []string             `json:"names"`
	Columns map[string][]float64 `json:"columns"`
}

// Column 返回某列；不存在时返回 nil。
func (f *Features) Column(name string) []float64 {
	if f == nil || f.Columns == nil {
		return nil
	}
	return f.Columns[name]
}

// Series 把全字段特征降回双通道，供现有 Score/Select 复用。
//
// 这一点很重要：保证「当前线上基线」与「新特征模型」跑在完全相同的
// 运动量/音频定义上，否则指标差异里会混入特征定义的差异，对比就不成立。
func (f *Features) Series() *Series {
	if f == nil {
		return nil
	}
	return &Series{
		Motion: f.Column("v_YAVG"),
		Audio:  f.Column("a_RMS_level"),
	}
}

// Len 返回按秒对齐后的可用长度。
func (f *Features) Len() int {
	if f == nil {
		return 0
	}
	return f.Seconds
}

// featureName 把 ffmpeg 的 metadata key 规范化成列名，不关心的字段返回空串。
//
//	lavfi.signalstats.YAVG           → v_YAVG
//	lavfi.astats.Overall.RMS_level   → a_RMS_level
//
// per-channel 的字段（lavfi.astats.1.RMS_level）与 U/V/SAT/HUE 之外的通道统计一律丢弃：
// Overall 已经覆盖同样的信息，多声道还会产生重复列。
func featureName(key string) string {
	if s := strings.TrimPrefix(key, "lavfi.signalstats."); s != key {
		return "v_" + s
	}
	if s := strings.TrimPrefix(key, "lavfi.astats.Overall."); s != key {
		return "a_" + s
	}
	return ""
}

// ExtractFeatures 跑一次 ffmpeg，提取全部可用特征并按秒对齐。
func ExtractFeatures(ctx context.Context, ffmpegBin, src string, threads int) (*Features, error) {
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	args := []string{"-hide_banner", "-nostdin"}
	if threads > 0 {
		args = append(args, "-threads", strconv.Itoa(threads))
	}
	args = append(args,
		"-i", src,
		"-map", "0:v:0", "-vf", fullVideoFilter,
		"-map", "0:a:0?", "-af", fullAudioFilter,
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

	type acc struct {
		sum float64
		n   int
	}
	buckets := make(map[int]map[string]*acc)
	curVT, curAT := 0.0, 0.0
	haveVT, haveAT := false, false
	tail := newTailBuffer(probeStderrTail)

	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// 只处理 metadata / ametadata 的采样行，其余留作失败时的上下文。
		if !strings.Contains(line, "Parsed_metadata_") && !strings.Contains(line, "Parsed_ametadata_") {
			tail.add(line)
			continue
		}
		key, val, ok := parseMetadataLine(line)
		if !ok {
			// 帧头行（frame:N pts:... pts_time:T），记录当前时间戳供后续字段归属。
			if m := reFullPtsTime.FindStringSubmatch(line); m != nil {
				if t, perr := strconv.ParseFloat(m[1], 64); perr == nil {
					if strings.Contains(line, "Parsed_ametadata_") {
						curAT, haveAT = t, true
					} else {
						curVT, haveVT = t, true
					}
				}
			}
			continue
		}
		name := featureName(key)
		if name == "" {
			continue
		}
		t, have := curVT, haveVT
		if strings.HasPrefix(name, "a_") {
			t, have = curAT, haveAT
		}
		if !have {
			continue
		}
		if math.IsNaN(val) || math.IsInf(val, 0) {
			continue
		}
		sec := int(t)
		if sec < 0 {
			continue
		}
		m := buckets[sec]
		if m == nil {
			m = make(map[string]*acc, 64)
			buckets[sec] = m
		}
		a := m[name]
		if a == nil {
			a = &acc{}
			m[name] = a
		}
		a.sum += val
		a.n++
	}

	if err := cmd.Wait(); err != nil {
		if extra := tail.String(); extra != "" {
			return nil, fmt.Errorf("ffmpeg 分析失败: %w | %s", err, extra)
		}
		return nil, fmt.Errorf("ffmpeg 分析失败: %w", err)
	}

	seconds := 0
	for sec := range buckets {
		if n := sec + 1; n > seconds {
			seconds = n
		}
	}
	if seconds == 0 {
		return nil, fmt.Errorf("未解析到采样点：文件可能没有视频流，或 ffmpeg 缺少 signalstats/astats 滤镜")
	}

	// 收集出现过的列名并排序，保证同一素材每次跑出来的列顺序一致（缓存可比对）。
	nameSet := make(map[string]struct{}, 64)
	for _, m := range buckets {
		for name := range m {
			nameSet[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)

	cols := make(map[string][]float64, len(names))
	for _, name := range names {
		col := make([]float64, seconds)
		last := 0.0
		haveLast := false
		for sec := 0; sec < seconds; sec++ {
			if m := buckets[sec]; m != nil {
				if a := m[name]; a != nil && a.n > 0 {
					col[sec] = a.sum / float64(a.n)
					last, haveLast = col[sec], true
					continue
				}
			}
			if haveLast {
				col[sec] = last
			}
		}
		cols[name] = col
	}

	return &Features{Seconds: seconds, Names: names, Columns: cols}, nil
}

// parseMetadataLine 从一行 metadata 输出里取出 key 与值。
//
// 输入形如：
//
//	[Parsed_metadata_4 @ 0x...] lavfi.signalstats.YAVG=3.29903
//
// 返回的 key 是原始 lavfi key（未规范化），由调用方决定是否关心。
func parseMetadataLine(line string) (string, float64, bool) {
	i := strings.Index(line, "] ")
	if i < 0 {
		return "", 0, false
	}
	body := strings.TrimSpace(line[i+2:])
	m := reFullKV.FindStringSubmatch(body)
	if m == nil {
		return "", 0, false
	}
	v, err := strconv.ParseFloat(m[2], 64)
	if err != nil {
		return "", 0, false
	}
	return m[1], v, true
}

// LoadFeatures 从磁盘读特征缓存。
func LoadFeatures(path string) (*Features, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Features
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("解析特征缓存失败: %w", err)
	}
	if f.Seconds <= 0 {
		return nil, fmt.Errorf("特征缓存为空: %s", path)
	}
	return &f, nil
}

// SaveFeatures 原子写入特征缓存（临时文件 + 改名），
// 避免训练脚本读到写了一半的缓存。
func SaveFeatures(path string, f *Features) error {
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("序列化特征失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("写入特征缓存失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("提交特征缓存失败: %w", err)
	}
	return nil
}
