package highlight

import (
	"math"
	"sort"
)

// MannWhitneyAUC 计算二分类 AUC（labels: 1=正）。平局记 0.5。
// 与 grid_auc.py / _spatial_stat_search.py 同式，供 hleval de-auc 复用。
func MannWhitneyAUC(scores []float64, labels []int) float64 {
	n := len(scores)
	if n == 0 || len(labels) != n {
		return 0
	}
	type pair struct {
		s float64
		y int
	}
	ps := make([]pair, n)
	nPos, nNeg := 0, 0
	for i := range ps {
		ps[i] = pair{scores[i], labels[i]}
		if labels[i] == 1 {
			nPos++
		} else {
			nNeg++
		}
	}
	if nPos == 0 || nNeg == 0 {
		return 0
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].s < ps[j].s })
	// 平均秩（1-based）
	ranks := make([]float64, n)
	for i := 0; i < n; {
		j := i
		for j+1 < n && ps[j+1].s == ps[i].s {
			j++
		}
		avg := float64(i+j+2) / 2
		for k := i; k <= j; k++ {
			ranks[k] = avg
		}
		i = j + 1
	}
	sumPos := 0.0
	for i := 0; i < n; i++ {
		if ps[i].y == 1 {
			sumPos += ranks[i]
		}
	}
	return (sumPos - float64(nPos)*(float64(nPos)+1)/2) / float64(nPos*nNeg)
}

// WindowBStdP90 段内逐秒块间 std 的 90 分位。
// grid_de 扩标前曾误报高于均值版；扩标后与 bstd_time 同级，仅作探索特征。
func WindowBStdP90(b Blocks, start, end int) float64 {
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
	vals := make([]float64, 0, end-start)
	for i := start; i < end; i++ {
		vals = append(vals, stddev(b[i]))
	}
	sort.Float64s(vals)
	// 最近秩 p90：ceil(0.9n)-1，小样本取到较高一侧
	idx := int(math.Ceil(0.9*float64(len(vals)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(vals) {
		idx = len(vals) - 1
	}
	return vals[idx]
}
