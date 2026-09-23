// Package highlight 从直播录像切片里自动挑出「高光」片段并裁切拼接。
//
// 判定用双因子：画面运动量（相邻帧差 YAVG）+ 音频能量（RMS）。
// 两者都经「中位数 + MAD」自适应归一化，衡量的是「这一段比它自己的常态活跃多少」，
// 因此跨主播、跨场景（聊天 / 跳舞 / 游戏）不需要逐个调绝对阈值——
// 绝对运动量在不同直播间能差几个量级。
//
// 实测结论（真实跳舞直播素材）：跳舞段运动量 z 达 3.1，而音频 z 反而降到 -0.8。
// 主播跳舞时停止说话，音频能量实际是「说话」的代理，与「跳舞」负相关，
// 所以默认权重偏向运动量 0.8 : 0.2；音频只作为唱歌/BGM 类直播间的辅助信号。
//
// 本包只依赖标准库，不 import internal/app 或 internal/recorder，便于单测与复用。
package highlight

import "time"

// Options 打分与裁切参数。
type Options struct {
	// MotionWeight / AudioWeight 双因子权重。
	MotionWeight float64
	AudioWeight  float64
	// Threshold 综合分阈值，单位是自适应 z 分（不是绝对量）。
	Threshold float64
	// MinDuration 最短高光（秒），短于此丢弃。
	MinDuration int
	// MaxDuration 单个高光最长（秒），超长则保留分数最高的窗口。
	MaxDuration int
	// MaxPerClip 每个切片最多产出几个高光。
	MaxPerClip int
	// MergeGap 相邻候选段间隔小于此值则合并（秒）。
	// 这是鲁棒性的关键参数：跳舞时分数在阈值附近抖动会切出大量碎片，
	// gap 太小会断链，把一整支舞切成互不相连的几段。
	MergeGap int
	// SmoothWindow 滑动平均窗口（秒），用于压抖动。
	SmoothWindow int
	// Pad 每段前后各扩几秒，让高光有头有尾。
	Pad int
	// Threads ffmpeg 解码线程数，用于限制对录制进程的 CPU 抢占；<=0 表示交给 ffmpeg 自动。
	Threads int
}

// DefaultOptions 返回经验默认值（基于真实素材校准，勿随意改动阈值方向）。
func DefaultOptions() Options {
	return Options{
		MotionWeight: 0.8,
		AudioWeight:  0.2,
		Threshold:    1.5,
		MinDuration:  15,
		MaxDuration:  180,
		MaxPerClip:   3,
		MergeGap:     20,
		SmoothWindow: 5,
		Pad:          5,
		Threads:      2,
	}
}

// Series 是分析得到的双通道时序数据，按秒对齐。
type Series struct {
	Motion []float64 `json:"motion"` // 画面运动量（帧差 YAVG 的每秒均值），越大动作越剧烈
	Audio  []float64 `json:"audio"`  // 音频 RMS（dBFS，负值），越大越响
}

// Len 返回按秒对齐后的可用长度（取两路较短者）。
func (s *Series) Len() int {
	if s == nil {
		return 0
	}
	n := len(s.Motion)
	if len(s.Audio) < n {
		n = len(s.Audio)
	}
	return n
}

// Segment 是一个高光段，单位为秒（相对切片起点）。
type Segment struct {
	Start int     `json:"start"`
	End   int     `json:"end"`
	Score float64 `json:"score"`
}

// Duration 返回段长（秒）。
func (s Segment) Duration() int { return s.End - s.Start }

// Result 是一次完整分析的产出。
type Result struct {
	// Segments 最终高光段；为空表示整段都很平、没有明显峰值。
	Segments []Segment
	// Peak 综合分曲线峰值（自适应 z 分），可用于判断这次检出有多「勉强」。
	Peak float64
	// AnalyzedSeconds 实际参与分析的秒数。
	AnalyzedSeconds int
	// Elapsed 分析（含 ffmpeg 解码）耗时。
	Elapsed time.Duration
}
