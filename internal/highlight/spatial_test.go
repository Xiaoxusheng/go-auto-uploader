package highlight

import (
	"math"
	"testing"
)

func blocksFrom(rows ...[9]float64) Blocks {
	out := make(Blocks, len(rows))
	for i, r := range rows {
		row := make([]float64, 9)
		copy(row, r[:])
		out[i] = row
	}
	return out
}

// 均匀块（整屏同步）→ bstd≈0；单块高运动（局部肢体）→ bstd 高。
func TestWindowBStdUniformVsLocal(t *testing.T) {
	uniform := blocksFrom(
		[9]float64{5, 5, 5, 5, 5, 5, 5, 5, 5},
		[9]float64{8, 8, 8, 8, 8, 8, 8, 8, 8},
	)
	local := blocksFrom(
		[9]float64{0, 0, 0, 0, 20, 0, 0, 0, 0},
		[9]float64{0, 0, 0, 0, 18, 0, 0, 0, 0},
	)
	if got := WindowBStd(uniform, 0, 2); got > 1e-9 {
		t.Fatalf("uniform bstd=%v, want ~0", got)
	}
	if got := WindowBStd(local, 0, 2); got < 5 {
		t.Fatalf("local bstd=%v, want high", got)
	}
}

func TestSuppressByBStdDropsFake(t *testing.T) {
	// 两段：前段局部（舞），后段均匀（礼物）
	b := blocksFrom(
		[9]float64{0, 0, 0, 0, 20, 0, 0, 0, 0},
		[9]float64{0, 0, 0, 0, 20, 0, 0, 0, 0},
		[9]float64{5, 5, 5, 5, 5, 5, 5, 5, 5},
		[9]float64{5, 5, 5, 5, 5, 5, 5, 5, 5},
	)
	segs := []Segment{{Start: 0, End: 2}, {Start: 2, End: 4}}
	got := SuppressByBStd(segs, b, 1.0)
	if len(got) != 1 || got[0].Start != 0 {
		t.Fatalf("got %+v, want only first segment", got)
	}
	// minBStd=0 关闭门槛
	if off := SuppressByBStd(segs, b, 0); len(off) != 2 {
		t.Fatalf("gate off should keep both, got %d", len(off))
	}
}

func TestSelectWithBlocksAppliesGate(t *testing.T) {
	// 4 秒高分，但空间上全是均匀块 → 应被 bstd 门槛清掉
	scores := []float64{3, 3, 3, 3}
	b := blocksFrom(
		[9]float64{5, 5, 5, 5, 5, 5, 5, 5, 5},
		[9]float64{5, 5, 5, 5, 5, 5, 5, 5, 5},
		[9]float64{5, 5, 5, 5, 5, 5, 5, 5, 5},
		[9]float64{5, 5, 5, 5, 5, 5, 5, 5, 5},
	)
	o := Options{Threshold: 1.0, MinDuration: 1, MergeGap: 1, MinBStd: 2.0}
	if got := SelectWithBlocks(scores, nil, b, o); len(got) != 0 {
		t.Fatalf("expected empty after bstd gate, got %+v", got)
	}
	o.MinBStd = 0
	if got := SelectWithBlocks(scores, nil, b, o); len(got) != 1 {
		t.Fatalf("gate off should keep segment, got %+v", got)
	}
}

func TestWindowAC1SustainedVsBurst(t *testing.T) {
	sustained := []float64{1, 2, 3, 4, 5, 6, 7, 8}
	burst := []float64{0, 20, 0, 20, 0, 20, 0, 20}
	aS := WindowAC1(sustained, 0, 8)
	aB := WindowAC1(burst, 0, 8)
	if aS < 0.9 {
		t.Fatalf("sustained ac1=%v, want high", aS)
	}
	if aB > 0.5 {
		t.Fatalf("burst ac1=%v, want low", aB)
	}
}

func TestWindowCenterRatioLocalCenter(t *testing.T) {
	// 中心热、四角冷 → 高；均匀 → ~1
	centerHot := blocksFrom([9]float64{0, 0, 0, 0, 20, 0, 0, 0, 0})
	uniform := blocksFrom([9]float64{5, 5, 5, 5, 5, 5, 5, 5, 5})
	h := WindowCenterRatio(centerHot, 0, 1)
	u := WindowCenterRatio(uniform, 0, 1)
	if h < 5 {
		t.Fatalf("centerHot ratio=%v, want high", h)
	}
	if u > 2 {
		t.Fatalf("uniform ratio=%v, want modest", u)
	}
}

func TestSuppressByAC1DropsBurst(t *testing.T) {
	motion := []float64{0, 20, 0, 20, 0, 20, 0, 20, 1, 2, 3, 4, 5, 6, 7, 8}
	segs := []Segment{{Start: 0, End: 8}, {Start: 8, End: 16}}
	got := SuppressByAC1(segs, motion, 0.3)
	if len(got) != 1 || got[0].Start != 8 {
		t.Fatalf("got %+v, want only sustained segment", got)
	}
}

func TestBlocksFromFeaturesRoundTrip(t *testing.T) {
	b := blocksFrom([9]float64{1, 2, 3, 4, 5, 6, 7, 8, 9})
	f := &Features{Seconds: 1, Names: []string{"v_YAVG"}, Columns: map[string][]float64{"v_YAVG": {10}}}
	AttachBlocks(f, b)
	got := BlocksFromFeatures(f)
	if got.Len() != 1 {
		t.Fatalf("len=%d", got.Len())
	}
	for k := 0; k < 9; k++ {
		if math.Abs(got[0][k]-float64(k+1)) > 1e-9 {
			t.Fatalf("b%d=%v want %d", k, got[0][k], k+1)
		}
	}
}

func TestWindowBStdMatchesPopulationStd(t *testing.T) {
	// 手算：均值块向量 [1,3] → std = 1（总体）
	b := blocksFrom(
		[9]float64{1, 3, 1, 3, 1, 3, 1, 3, 1},
	)
	got := WindowBStd(b, 0, 1)
	// means = [1,3,1,3,1,3,1,3,1]，mean=15/9，std=sqrt(mean((x-m)^2))
	var vals [9]float64
	copy(vals[:], b[0])
	var sum float64
	for _, v := range vals {
		sum += v
	}
	m := sum / 9
	var ss float64
	for _, v := range vals {
		d := v - m
		ss += d * d
	}
	want := math.Sqrt(ss / 9)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v want %v", got, want)
	}
}
