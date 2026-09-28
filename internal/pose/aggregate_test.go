package pose

import "testing"

func win(n int, vis, face, ext float64) []FrameFeatures {
	out := make([]FrameFeatures, n)
	for i := range out {
		out[i] = FrameFeatures{Detected: true, VisRatio: vis, FaceFrac: face, ExtH: ext}
	}
	return out
}

func TestAggregateWindowLabels(t *testing.T) {
	lowDet := append(win(1, 0.9, 0.05, 0.5), make([]FrameFeatures, 7)...)
	cases := []struct {
		name  string
		feats []FrameFeatures
		want  string
	}{
		{"空窗", nil, "none"},
		{"全无人", make([]FrameFeatures, 8), "none"},
		{"检出率低于 detmin", lowDet, "none"},
		{"全身舞", win(8, 0.9, 0.05, 0.5), "dance"},
		{"近景聊天", win(8, 0.9, 0.5, 0.5), "closeup"},
		{"中间态", win(8, 0.4, 0.05, 0.5), "other"},
	}
	for _, c := range cases {
		got := AggregateWindow(c.feats, PrelabelDetMin, PrelabelVisMin, PrelabelFaceMax)
		if got.Label != c.want {
			t.Errorf("%s: label=%q want %q", c.name, got.Label, c.want)
		}
	}
}

// ext 带已从判定中移除（§22：无判别力且砍召回）。
// ext 取任何值都不该改变标签 —— 这条回归锁住"评估/预标口径与线上不一致"的老问题。
func TestAggregateWindowIgnoresExt(t *testing.T) {
	for _, ext := range []float64{0, 0.1, 0.3, 0.5, 0.8, 1.0, 1.5, 5} {
		got := AggregateWindow(win(8, 0.9, 0.05, ext), PrelabelDetMin, PrelabelVisMin, PrelabelFaceMax)
		if got.Label != "dance" {
			t.Errorf("ext=%.2f 时 label=%q，ext 带应已移除（恒为 dance）", ext, got.Label)
		}
	}
}

func TestAggregateWindowStats(t *testing.T) {
	f := make([]FrameFeatures, 10)
	for i := 0; i < 4; i++ {
		f[i] = FrameFeatures{Detected: true, VisRatio: 0.8, FaceFrac: 0.1, ExtH: 0.5}
	}
	got := AggregateWindow(f, PrelabelDetMin, PrelabelVisMin, PrelabelFaceMax)
	if got.DetRate != 0.4 {
		t.Errorf("DetRate=%.3f want 0.4", got.DetRate)
	}
	if got.VisMean != 0.8 || got.FaceMean != 0.1 {
		t.Errorf("检出帧均值应只统计检出帧: vis=%.3f face=%.3f", got.VisMean, got.FaceMean)
	}
	if got.Label != "dance" {
		t.Errorf("label=%q want dance（det率 0.4≥0.2 且 vis/face 达标）", got.Label)
	}
}
