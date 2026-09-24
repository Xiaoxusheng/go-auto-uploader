package highlight

// 离线评估指标（秒级 P/R/F1 + 段级 IoU）。
// 放在本包而不是 Python：后处理/打分的语义只应有一份实现，
// 否则 Python `fill_gaps` 与 Go `Select` 会悄悄分叉（已踩过：`<=` vs `<`）。

// PR 是秒级二分类指标。
type PR struct {
	P, R, F1 float64
	TP, FP, FN int
}

// SecondPRF 逐秒对比标签与预测。
func SecondPRF(yTrue, yPred []int) PR {
	var tp, fp, fn int
	for i := range yTrue {
		t, p := yTrue[i] != 0, yPred[i] != 0
		switch {
		case t && p:
			tp++
		case !t && p:
			fp++
		case t && !p:
			fn++
		}
	}
	var pr PR
	pr.TP, pr.FP, pr.FN = tp, fp, fn
	if tp+fp > 0 {
		pr.P = float64(tp) / float64(tp+fp)
	}
	if tp+fn > 0 {
		pr.R = float64(tp) / float64(tp+fn)
	}
	if pr.P+pr.R > 0 {
		pr.F1 = 2 * pr.P * pr.R / (pr.P + pr.R)
	}
	return pr
}

// SegPR 段级 IoU@iouTh 一对一贪心匹配后的指标。
type SegPR struct {
	P, R, F1 float64
	GT, Pred, Hit int
}

// Intervals 把 0/1 序列收成 [start,end) 区间；minLen 以下丢弃。
func Intervals(y []int, minLen int) [][2]int {
	if minLen < 1 {
		minLen = 1
	}
	var out [][2]int
	for i := 0; i < len(y); {
		if y[i] == 0 {
			i++
			continue
		}
		j := i
		for j < len(y) && y[j] != 0 {
			j++
		}
		if j-i >= minLen {
			out = append(out, [2]int{i, j})
		}
		i = j
	}
	return out
}

func iou(a, b [2]int) float64 {
	lo := a[0]
	if b[0] > lo {
		lo = b[0]
	}
	hiEnd := a[1]
	if b[1] < hiEnd {
		hiEnd = b[1]
	}
	inter := hiEnd - lo
	if inter < 0 {
		inter = 0
	}
	union := (a[1] - a[0]) + (b[1] - b[0]) - inter
	if union <= 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// SegmentPRF 段级检测：IoU≥ioutTh 记命中，贪心一对一。
// minLen 与 Select.MinDuration 对齐（默认 1 = 不额外丢短）。
func SegmentPRF(yTrue, yPred []int, iouTh float64, minLen int) SegPR {
	gts := Intervals(yTrue, minLen)
	prs := Intervals(yPred, minLen)
	var sp SegPR
	sp.GT, sp.Pred = len(gts), len(prs)
	if len(gts) == 0 && len(prs) == 0 {
		sp.P, sp.R, sp.F1 = 1, 1, 1
		return sp
	}
	if len(gts) == 0 || len(prs) == 0 {
		return sp
	}
	type pair struct {
		s  float64
		gi int
		pi int
	}
	pairs := make([]pair, 0, len(gts)*len(prs))
	for gi, g := range gts {
		for pi, p := range prs {
			pairs = append(pairs, pair{iou(g, p), gi, pi})
		}
	}
	// 按 IoU 降序
	for i := 0; i < len(pairs); i++ {
		for j := i + 1; j < len(pairs); j++ {
			if pairs[j].s > pairs[i].s {
				pairs[i], pairs[j] = pairs[j], pairs[i]
			}
		}
	}
	usedG := make([]bool, len(gts))
	usedP := make([]bool, len(prs))
	for _, pr := range pairs {
		if pr.s < iouTh {
			break
		}
		if usedG[pr.gi] || usedP[pr.pi] {
			continue
		}
		usedG[pr.gi] = true
		usedP[pr.pi] = true
		sp.Hit++
	}
	sp.P = float64(sp.Hit) / float64(len(prs))
	sp.R = float64(sp.Hit) / float64(len(gts))
	if sp.P+sp.R > 0 {
		sp.F1 = 2 * sp.P * sp.R / (sp.P + sp.R)
	}
	return sp
}

// MaskToSegments 预测掩码 → Segment 列表（复用 Select 的区间语义）。
func MaskToSegments(mask []int) []Segment {
	var out []Segment
	for _, iv := range Intervals(mask, 1) {
		out = append(out, Segment{Start: iv[0], End: iv[1]})
	}
	return out
}

// SegmentsToMask 与 MaskToSegments 互逆。
func SegmentsToMask(segs []Segment, n int) []int {
	m := make([]int, n)
	for _, s := range segs {
		for i := s.Start; i < s.End && i < n; i++ {
			if i >= 0 {
				m[i] = 1
			}
		}
	}
	return m
}
