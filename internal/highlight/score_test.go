package highlight

import (
	"math"
	"testing"
)

func TestRobustZConstantSeries(t *testing.T) {
	x := make([]float64, 100)
	for i := range x {
		x[i] = 5
	}
	for i, v := range robustZ(x, 1) {
		if v != 0 {
			t.Fatalf("常数序列的 z 分应全为 0，第 %d 个是 %v", i, v)
		}
	}
}

// 大部分时间完全静止、只有一小段活跃时，MAD 会塌成 0，
// 必须靠标准差兜底才不至于把整段判成「毫无波动」。
func TestRobustZFallbackWhenMADCollapses(t *testing.T) {
	x := make([]float64, 200)
	for i := range x {
		x[i] = 10
	}
	for i := 100; i < 130; i++ {
		x[i] = 40
	}
	z := robustZ(x, 5)
	if z[115] < 1.5 {
		t.Fatalf("MAD 塌陷时应回退到标准差并识别出峰值，实际 z=%v", z[115])
	}
	if math.Abs(z[20]) > 0.5 {
		t.Fatalf("平稳处 z 分应接近 0，实际 %v", z[20])
	}
}

func TestRobustZDetectsPeakInNoisySignal(t *testing.T) {
	x := make([]float64, 200)
	for i := range x {
		x[i] = 10 + float64(i%3) // 制造一点噪声，避免 MAD 为 0
	}
	for i := 100; i < 130; i++ {
		x[i] = 40
	}
	z := robustZ(x, 5)
	if z[115] < 2 {
		t.Fatalf("峰值处 z 分应 > 2，实际 %v", z[115])
	}
}

func TestSelectMergesFragments(t *testing.T) {
	// 模拟跳舞时分数在阈值附近抖动切出碎片，验证链式合并能拼回一整段。
	scores := make([]float64, 300)
	for i := range scores {
		scores[i] = -0.5
	}
	for i := 100; i < 200; i++ {
		if i%7 == 0 {
			scores[i] = 1.2 // 偶尔掉到阈值以下
			continue
		}
		scores[i] = 2.5
	}
	o := DefaultOptions()
	o.Threshold = 1.5
	o.MinDuration = 15
	o.MergeGap = 20
	segs := Select(scores, o)
	if len(segs) != 1 {
		t.Fatalf("抖动曲线应被链式合并为 1 段，实际 %d 段: %+v", len(segs), segs)
	}
	if segs[0].Start > 100 || segs[0].End < 195 {
		t.Fatalf("合并段范围异常: %+v", segs[0])
	}
}

// MergeGap 太小会在候选间隔正好等于 gap 处断链，把一整支舞切成两段。
func TestSelectBreaksWhenMergeGapTooSmall(t *testing.T) {
	scores := make([]float64, 300)
	for i := range scores {
		scores[i] = 0
	}
	for i := 100; i < 140; i++ {
		scores[i] = 3
	}
	for i := 150; i < 190; i++ { // 与上一段间隔 10 秒
		scores[i] = 3
	}
	o := DefaultOptions()
	o.MinDuration = 15
	o.Pad = 0

	o.MergeGap = 10 // 间隔正好 =10，不满足 < 条件 → 断链
	if segs := Select(scores, o); len(segs) != 2 {
		t.Fatalf("gap=10 时应断成 2 段，实际 %d: %+v", len(segs), segs)
	}
	o.MergeGap = 20 // 放大后应合并
	if segs := Select(scores, o); len(segs) != 1 {
		t.Fatalf("gap=20 时应合并为 1 段，实际 %d: %+v", len(segs), segs)
	}
}

func TestSelectDropsShortSegments(t *testing.T) {
	scores := make([]float64, 300)
	for i := 100; i < 105; i++ { // 只有 5 秒
		scores[i] = 3
	}
	o := DefaultOptions()
	o.MinDuration = 15
	if segs := Select(scores, o); len(segs) != 0 {
		t.Fatalf("5 秒的段应被最小时长过滤掉，实际 %+v", segs)
	}
}

func TestSelectRespectsMaxDuration(t *testing.T) {
	scores := make([]float64, 600)
	for i := 100; i < 500; i++ { // 400 秒
		scores[i] = 3
	}
	o := DefaultOptions()
	o.MaxDuration = 120
	o.Pad = 0
	segs := Select(scores, o)
	if len(segs) != 1 {
		t.Fatalf("应剩 1 段，实际 %d", len(segs))
	}
	if d := segs[0].Duration(); d != 120 {
		t.Fatalf("应被限长到 120 秒，实际 %d", d)
	}
}

func TestSelectMaxPerClipAndOrdering(t *testing.T) {
	scores := make([]float64, 1000)
	for k := 0; k < 5; k++ {
		base := 100 + k*150
		for i := base; i < base+40; i++ {
			scores[i] = 3
		}
	}
	o := DefaultOptions()
	o.MaxPerClip = 2
	o.Pad = 0
	segs := Select(scores, o)
	if len(segs) != 2 {
		t.Fatalf("MaxPerClip=2 应只留 2 段，实际 %d", len(segs))
	}
	if segs[0].Start > segs[1].Start {
		t.Fatalf("结果应按时间升序，实际 %+v", segs)
	}
}

func TestRawSegments(t *testing.T) {
	scores := []float64{0, 2, 2, 0, 0, 3, 3, 3, 0}
	raw := RawSegments(scores, 1.5)
	if len(raw) != 2 {
		t.Fatalf("应切出 2 段，实际 %d: %+v", len(raw), raw)
	}
	if raw[0].Start != 1 || raw[0].End != 3 {
		t.Fatalf("第 1 段范围错误: %+v", raw[0])
	}
	if raw[1].Start != 5 || raw[1].End != 8 {
		t.Fatalf("第 2 段范围错误: %+v", raw[1])
	}
}

func TestBestWindow(t *testing.T) {
	scores := []float64{0, 0, 0, 5, 5, 5, 0, 0, 0}
	if w := bestWindow(scores, 0, 9, 3); w.Start != 3 || w.End != 6 {
		t.Fatalf("最高窗口应是 3-6，实际 %+v", w)
	}
}

func TestMedian(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{[]float64{1, 2, 3}, 2},
		{[]float64{1, 2, 3, 4}, 2.5},
		{[]float64{5}, 5},
		{nil, 0},
	}
	for _, c := range cases {
		if got := median(c.in); got != c.want {
			t.Fatalf("median(%v) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

// 运动量有峰、音频全程平坦时，综合分峰值应完全由运动量贡献。
func TestScoreUsesMotionWhenAudioFlat(t *testing.T) {
	s := &Series{Motion: make([]float64, 200), Audio: make([]float64, 200)}
	for i := range s.Motion {
		s.Motion[i] = 10
		s.Audio[i] = -30
	}
	for i := 100; i < 130; i++ {
		s.Motion[i] = 40
	}
	o := DefaultOptions()
	segs := Select(Score(s, o), o)
	if len(segs) != 1 {
		t.Fatalf("应检出 1 段，实际 %d: %+v", len(segs), segs)
	}
	if segs[0].Start > 100 || segs[0].End < 130 {
		t.Fatalf("检出范围应覆盖 100-130，实际 %+v", segs[0])
	}
}

func TestSeriesLen(t *testing.T) {
	s := &Series{Motion: make([]float64, 10), Audio: make([]float64, 7)}
	if s.Len() != 7 {
		t.Fatalf("Len 应取较短者 7，实际 %d", s.Len())
	}
	var nilS *Series
	if nilS.Len() != 0 {
		t.Fatalf("nil Series 的 Len 应为 0")
	}
}

func TestTail(t *testing.T) {
	if got := tail("abc", 10); got != "abc" {
		t.Fatalf("短串应原样返回，实际 %q", got)
	}
	if got := tail("a\nb\ncdef", 4); got != "cdef" {
		t.Fatalf("应取末尾 4 字符，实际 %q", got)
	}
}
