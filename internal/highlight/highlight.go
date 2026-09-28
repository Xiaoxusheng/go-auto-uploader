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
	// ExitRatio 迟滞退出比：正段内分数降到 Threshold*ExitRatio 以下才退出。
	// 0 表示不用迟滞（单阈值）。实测在含聊天/待机负样本的分布上，
	// 迟滞（进入 th、退出 0.8*th）比纯调 MinDuration/MergeGap 更能压误检
	// （train_exp3 / LABEL_2026-09-24）。合法区间约 0.5~0.95。
	ExitRatio float64
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
	// MinBStd 段级空间门槛：候选段 bstd（3×3 块间 std）低于此值则丢弃。
	// 用于压礼物特效/切近景/空镜等伪运动（缺陷 D/E）。0=关闭。
	// 需配套 Blocks 数据（SelectWithBlocks / ExtractBlocks）。
	MinBStd float64
	// MinAC1 段级时序门槛：运动量 lag-1 自相关低于此值则丢弃（压礼物短促爆发）。
	// 0=关闭。grid_de：dance vs gift AUC 0.87，不依赖空间块。
	MinAC1 float64
	// MinClose 段级中心集中度门槛：WindowCenterRatio 低于此值则丢弃。
	// 0=关闭，需 Blocks。与 MinAC1 秩组合在金标上 dance vs D/E AUC 0.92。
	MinClose float64
	// PoseGate 姿态语义门（可空）：对候选段抽帧跑人体姿态（internal/pose），
	// 砍掉「近景聊天 / 连麦 / 无人特效」类误检段。段级模拟：
	// 非舞秒砍 89% @ 真舞秒损 13%（docs/highlight-spatial-de.md §19）。
	// nil=关闭；纯 Go 构建下姿态推理不可用，门自动失效（段照常保留）。
	PoseGate *PoseGateParams
	// Threads ffmpeg 解码线程数，用于限制对录制进程的 CPU 抢占；<=0 表示交给 ffmpeg 自动。
	Threads int
}

// PoseGateParams 姿态语义门参数（纯数据载体；推理在 internal/pose，cgo 构建才有实现）。
type PoseGateParams struct {
	// Enabled 总开关（config highlight_pose_gate.enable）。
	Enabled bool
	// DetMin 段内姿态检出率下限：低于视为无人/特效场，整段拒。
	DetMin float64
	// VisMin 关键点置信度均值下限（全身入镜才是舞）。
	VisMin float64
	// FaceMax 双眼间距/帧宽上限（脸大=近景聊天）。
	FaceMax float64
	// KeepRatio 段内通过秒占比达到该值才保留段。
	KeepRatio float64
	// FPS 段内抽帧率（默认 5：奈奎斯特 2.5Hz，覆盖舞曲节拍 1.7~2.3Hz，
	// 为将来节拍耦合特征免重抽）。
	FPS int
	// DllPath/ModelPath onnxruntime.dll 与 yolov8n-pose.onnx 路径（空=exe 同目录默认名）。
	DllPath, ModelPath string
	// 学习型门头段级投票（灰度）：enable 时段内头判舞窗占比 ≥ Frac 才保留段。
	HeadFilterEnable bool
	HeadModel        string
	HeadFrac         float64
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
		ExitRatio:    0,
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
