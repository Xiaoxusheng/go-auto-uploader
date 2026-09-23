package highlight

import (
	"math"
	"sort"
)

// smooth 滑动平均；窗口在两端自动收缩，不产生边缘塌陷。
func smooth(x []float64, w int) []float64 {
	if w < 1 {
		w = 1
	}
	out := make([]float64, len(x))
	half := w / 2
	for i := range x {
		lo, hi := i-half, i+half+1
		if lo < 0 {
			lo = 0
		}
		if hi > len(x) {
			hi = len(x)
		}
		var sum float64
		for _, v := range x[lo:hi] {
			sum += v
		}
		out[i] = sum / float64(hi-lo)
	}
	return out
}

// median 返回中位数（不修改入参）。
func median(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	c := append([]float64(nil), x...)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

// robustZ 用中位数与 MAD 做自适应归一化，返回「相对本段自身」的偏离度。
//
// 这是整套方案里最关键的一步：不同主播、不同场景的绝对运动量差了几个量级
// （聊天 vs 跳舞 vs 游戏），用绝对阈值必然要逐个调参；减中位数、除 MAD 之后
// 得到的 z 分只反映「这一段比它自己的常态活跃多少」，跨场景通用。
// 1.4826 是让 MAD 与标准差可比的换算常数。
func robustZ(x []float64, smoothWindow int) []float64 {
	sm := smooth(x, smoothWindow)
	med := median(sm)
	dev := make([]float64, len(sm))
	for i, v := range sm {
		dev[i] = math.Abs(v - med)
	}
	mad := median(dev)
	scale := 1.4826 * mad
	if scale < 1e-9 {
		// MAD 塌成 0：超过一半的样本与中位数完全相同（例如视频长时间完全静止）。
		// 此时 MAD 失去分辨力，回退到标准差；仍为 0 才判定为「整段毫无波动」。
		scale = stddev(sm)
	}
	out := make([]float64, len(sm))
	if scale < 1e-9 {
		return out
	}
	for i, v := range sm {
		out[i] = (v - med) / scale
	}
	return out
}

// stddev 返回总体标准差，仅作为 MAD 塌陷时的兜底尺度。
func stddev(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	m := mean(x)
	var sum float64
	for _, v := range x {
		d := v - m
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(x)))
}

// ScoreDetail 返回两路自适应 z 分曲线与加权后的综合分曲线，便于分别观察各因子贡献。
func ScoreDetail(s *Series, o Options) (zm, za, total []float64) {
	n := s.Len()
	if n == 0 {
		return nil, nil, nil
	}
	zm = robustZ(s.Motion[:n], o.SmoothWindow)
	za = robustZ(s.Audio[:n], o.SmoothWindow)

	sum := o.MotionWeight + o.AudioWeight
	if sum <= 0 {
		sum = 1
	}
	total = make([]float64, n)
	for i := range total {
		total[i] = (zm[i]*o.MotionWeight + za[i]*o.AudioWeight) / sum
	}
	return zm, za, total
}

// Score 把双因子合成一条综合分曲线。
func Score(s *Series, o Options) []float64 {
	_, _, total := ScoreDetail(s, o)
	return total
}

// RawSegments 只做阈值切分，不做合并/过滤/限长，用于观察「原始命中」长什么样。
func RawSegments(scores []float64, threshold float64) []Segment {
	var out []Segment
	for i := 0; i < len(scores); {
		if scores[i] <= threshold {
			i++
			continue
		}
		j := i
		for j < len(scores) && scores[j] > threshold {
			j++
		}
		out = append(out, Segment{Start: i, End: j})
		i = j
	}
	return out
}

// Select 从分数曲线挑出高光段：
// 阈值切分 → 合并邻近 → 丢弃过短 → 限长 → 补边 → 取分数最高的前 N 个。
//
// 合并是「链式」的：只要相邻命中段的间隔小于 MergeGap 就不断往后串，
// 因此跳舞时分数在阈值附近抖动切出的大量碎片会被重新拼回一整段。
func Select(scores []float64, o Options) []Segment {
	var merged []Segment
	for _, s := range RawSegments(scores, o.Threshold) {
		if n := len(merged); n > 0 && s.Start-merged[n-1].End < o.MergeGap {
			merged[n-1].End = s.End
			continue
		}
		merged = append(merged, s)
	}

	var out []Segment
	for _, s := range merged {
		if s.Duration() < o.MinDuration {
			continue
		}
		if o.MaxDuration > 0 && s.Duration() > o.MaxDuration {
			s = bestWindow(scores, s.Start, s.End, o.MaxDuration)
		}
		st, en := s.Start-o.Pad, s.End+o.Pad
		if st < 0 {
			st = 0
		}
		if en > len(scores) {
			en = len(scores)
		}
		s.Start, s.End = st, en
		s.Score = mean(scores[st:en])
		out = append(out, s)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if o.MaxPerClip > 0 && len(out) > o.MaxPerClip {
		out = out[:o.MaxPerClip]
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// bestWindow 在 [lo,hi) 内找平均分最高的定长窗口。
func bestWindow(scores []float64, lo, hi, w int) Segment {
	if w >= hi-lo {
		return Segment{Start: lo, End: hi}
	}
	best := Segment{Start: lo, End: lo + w}
	var sum float64
	for j := lo; j < lo+w; j++ {
		sum += scores[j]
	}
	bestSum := sum
	for i := lo + 1; i+w <= hi; i++ {
		sum += scores[i+w-1] - scores[i-1]
		if sum > bestSum {
			bestSum = sum
			best = Segment{Start: i, End: i + w}
		}
	}
	return best
}

// mean 求均值，空切片返回 0。
func mean(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	var sum float64
	for _, v := range x {
		sum += v
	}
	return sum / float64(len(x))
}
