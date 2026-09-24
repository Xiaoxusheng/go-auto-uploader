package highlight

import "testing"

func TestHysteresisHoldsThroughDip(t *testing.T) {
	// 单阈值 1.5 会把 0.5s 的凹陷切成两段；迟滞 1.5/0.8*1.5=1.2 应粘住
	scores := []float64{0, 2, 2, 1.3, 2, 2, 0}
	m := HysteresisMask(scores, 1.5, 1.2)
	// 期望 0 1 1 1 1 1 0
	want := []int{0, 1, 1, 1, 1, 1, 0}
	for i := range want {
		if m[i] != want[i] {
			t.Fatalf("mask=%v want=%v", m, want)
		}
	}
	segs := MaskSegments(m)
	if len(segs) != 1 || segs[0].Start != 1 || segs[0].End != 6 {
		t.Fatalf("应粘成一段 [1,6)，实际 %+v", segs)
	}

	// 无迟滞时同一曲线切成 2 段
	segs2 := RawSegments(scores, 1.5)
	if len(segs2) != 2 {
		t.Fatalf("单阈值应断成 2 段，实际 %+v", segs2)
	}
}

func TestHysteresisExitBelowCloses(t *testing.T) {
	scores := []float64{0, 2, 2, 0.5, 2, 2, 0}
	m := HysteresisMask(scores, 1.5, 1.2)
	// 0.5 < 1.2，应退出
	want := []int{0, 1, 1, 0, 1, 1, 0}
	for i := range want {
		if m[i] != want[i] {
			t.Fatalf("mask=%v want=%v", m, want)
		}
	}
}

func TestSelectWithExitRatio(t *testing.T) {
	scores := make([]float64, 40)
	for i := range scores {
		scores[i] = 0
	}
	for i := 10; i < 30; i++ {
		scores[i] = 3
	}
	scores[18] = 0.5 // 凹陷 < exit(0.96)，应退出
	o := DefaultOptions()
	o.Threshold = 1.2
	o.ExitRatio = 0.8 // exit=0.96
	o.MinDuration = 5
	o.MergeGap = 0 // 不合并，单独验证迟滞
	o.Pad = 0
	o.MaxDuration = 0
	o.MaxPerClip = 0
	segs := Select(scores, o)
	if len(segs) != 2 {
		t.Fatalf("凹陷低于 exit 应断两段，实际 %+v", segs)
	}
	scores[18] = 1.1 // 高于 exit 0.96，应粘住
	segs = Select(scores, o)
	if len(segs) != 1 {
		t.Fatalf("凹陷高于 exit 应粘成一段，实际 %+v", segs)
	}
}

func TestSecondPRFBasic(t *testing.T) {
	y := []int{1, 1, 0, 0, 1}
	p := []int{1, 0, 0, 1, 1}
	m := SecondPRF(y, p)
	if m.TP != 2 || m.FP != 1 || m.FN != 1 {
		t.Fatalf("计数不对: %+v", m)
	}
	if m.P != 2.0/3 || m.R != 2.0/3 {
		t.Fatalf("P/R 不对: %+v", m)
	}
}

func TestSegmentPRFMatchesWhenAligned(t *testing.T) {
	y := make([]int, 100)
	p := make([]int, 100)
	for i := 10; i < 40; i++ {
		y[i], p[i] = 1, 1
	}
	for i := 60; i < 70; i++ {
		y[i] = 1
	}
	for i := 55; i < 75; i++ { // 与上一段 IoU 高
		p[i] = 1
	}
	m := SegmentPRF(y, p, 0.5, 1)
	if m.Hit != 2 || m.GT != 2 || m.Pred != 2 {
		t.Fatalf("应命中 2/2: %+v", m)
	}
}

func TestSegmentPRFFragmentsPenalized(t *testing.T) {
	y := make([]int, 100)
	p := make([]int, 100)
	for i := 10; i < 40; i++ {
		y[i] = 1
	}
	// 切成 5 个 ~5s 碎片：与 30s 真值的 IoU 均 < 0.5，段级应全军覆没
	// （这正是「秒级 F1 虚高、段级崩溃」的机制）
	for _, s := range [][2]int{{10, 15}, {16, 21}, {22, 27}, {28, 33}, {34, 40}} {
		for i := s[0]; i < s[1]; i++ {
			p[i] = 1
		}
	}
	m := SegmentPRF(y, p, 0.5, 1)
	if m.Hit != 0 || m.Pred != 5 || m.GT != 1 {
		t.Fatalf("碎片 IoU 不足应 hit=0 pred=5: %+v", m)
	}
	// 合并成一段后应命中
	for _, s := range [][2]int{{10, 15}, {16, 21}, {22, 27}, {28, 33}, {34, 40}} {
		for i := s[0]; i < s[1]; i++ {
			p[i] = 1
		}
	}
	for i := 10; i < 40; i++ {
		p[i] = 1
	}
	m2 := SegmentPRF(y, p, 0.5, 1)
	if m2.Hit != 1 {
		t.Fatalf("拼回后应命中: %+v", m2)
	}
}

func TestSelectThenMetricsRoundTrip(t *testing.T) {
	// 阈值抖动切出的碎片，经 Select 合并后段级应恢复
	scores := make([]float64, 80)
	for i := 0; i < len(scores); i++ {
		scores[i] = 0
	}
	for i := 20; i < 50; i++ {
		scores[i] = 3
	}
	scores[25], scores[26] = 0, 0
	scores[35], scores[36], scores[37] = 0, 0, 0
	o := DefaultOptions()
	o.Threshold = 1.2
	o.MergeGap = 12
	o.MinDuration = 8
	o.Pad = 0
	o.MaxDuration = 0
	o.MaxPerClip = 0
	segs := Select(scores, o)
	y := make([]int, 80)
	for i := 20; i < 50; i++ {
		y[i] = 1
	}
	pred := SegmentsToMask(segs, 80)
	m := SegmentPRF(y, pred, 0.5, 1)
	if m.Hit != 1 {
		t.Fatalf("Select 后应命中真值段: segs=%+v m=%+v", segs, m)
	}
}
