package main

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"upload/internal/pose"
)

// TestUeStreamerName 主播名解析与 api/http poseClipRe 同口径。
func TestUeStreamerName(t *testing.T) {
	cases := map[string]string{
		"小妤_2026-09-25_20-13-29_010":   "小妤",
		"年年_2026-09-26_16-30-53_007":   "年年",
		"D.an_2026-09-26_09-00-01_003": "D.an",
		"gorani-2":                     "",
	}
	for clip, want := range cases {
		if got := ueStreamerName(clip); got != want {
			t.Errorf("ueStreamerName(%q)=%q want %q", clip, got, want)
		}
	}
}

// TestUeGatePass 生产门口径：det率<detmin 非舞；vis≥visMin 且 face≤faceMax 舞。
func TestUeGatePass(t *testing.T) {
	g := ueGate{VisMin: 0.70, FaceMax: 0.12, DetMin: 0.30}
	cases := []struct {
		st   pose.WindowStats
		want bool
	}{
		{pose.WindowStats{DetRate: 0.9, VisMean: 0.8, FaceMean: 0.05}, true},
		{pose.WindowStats{DetRate: 0.29, VisMean: 0.8, FaceMean: 0.05}, false},
		{pose.WindowStats{DetRate: 0.9, VisMean: 0.69, FaceMean: 0.05}, false},
		{pose.WindowStats{DetRate: 0.9, VisMean: 0.8, FaceMean: 0.13}, false},
	}
	for i, c := range cases {
		if got := ueGatePass(c.st, g); got != c.want {
			t.Errorf("case %d: ueGatePass(%+v)=%v want %v", i, c.st, got, c.want)
		}
	}
}

// TestUeLoadLiveGate 生产 config 读得到用现值；读不到回退定标点。
func TestUeLoadLiveGate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"builtin":{"highlight_pose_gate":{"vis_min":0.71,"face_max":0.11,"det_min":0.31}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	g := ueLoadLiveGate(p)
	if g.Source != "live-config" || g.VisMin != 0.71 || g.FaceMax != 0.11 || g.DetMin != 0.31 {
		t.Errorf("live-config 解析错: %+v", g)
	}
	g = ueLoadLiveGate(filepath.Join(t.TempDir(), "missing.json"))
	if g.Source != "fallback" || g.VisMin != 0.70 || g.FaceMax != 0.12 || g.DetMin != 0.30 {
		t.Errorf("fallback 解析错: %+v", g)
	}
}

// TestUeClassifyVerdict 门∧头最终判定：合成头按 det_rate(特征0) 二分。
func TestUeClassifyVerdict(t *testing.T) {
	modelJSON := `{"version":"test","features":["det_rate"],"learning_rate":1,"init_logodds":0,
		"trees":[{"feature":[0,0,0],"threshold":[0.5,0,0],"children_left":[1,-1,-1],
		"children_right":[2,-1,-1],"leaf_value":[0,-10,10]}]}`
	mp := filepath.Join(t.TempDir(), "head.json")
	if err := os.WriteFile(mp, []byte(modelJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := pose.LoadHeadModel(mp)
	if err != nil {
		t.Fatal(err)
	}
	g := ueGate{VisMin: 0.70, FaceMax: 0.12, DetMin: 0.30}
	mkWin := func(det int, vis, face float64) []pose.FrameFeatures {
		win := make([]pose.FrameFeatures, 8)
		for i := range win {
			win[i] = pose.FrameFeatures{Detected: i < det, VisRatio: vis, FaceFrac: face, ExtH: 0.5, Aspect: 2}
		}
		return win
	}
	cases := []struct {
		name      string
		det       int
		vis, face float64
		ok        bool
		cls       string
		prob      float64
	}{
		{"头也判舞→keep", 8, 0.8, 0.05, true, "keep", 1},
		{"头拒绝→reject", 4, 0.8, 0.05, true, "reject", 0},
		{"face 超限→门外", 8, 0.8, 0.5, false, "", 0},
		{"vis 不足→门外", 8, 0.3, 0.05, false, "", 0},
	}
	for _, c := range cases {
		w, ok := ueClassifyVerdict("甲_2026-09-27_10-00-00_001", 0, mkWin(c.det, c.vis, c.face), g, m)
		if ok != c.ok {
			t.Errorf("%s: ok=%v want %v", c.name, ok, c.ok)
			continue
		}
		if !c.ok {
			continue
		}
		if w.Cls != c.cls {
			t.Errorf("%s: cls=%q want %q", c.name, w.Cls, c.cls)
		}
		if math.Abs(w.HeadProb-c.prob) > 0.01 {
			t.Errorf("%s: head_prob=%v want ≈%v", c.name, w.HeadProb, c.prob)
		}
		if w.Streamer != "甲" || !w.GatePass {
			t.Errorf("%s: streamer/gate_pass 字段错: %+v", c.name, w)
		}
	}
}

// TestUeFairTake 主播轮转均衡：预算封顶、 plentiful 时各主播差 ≤1、同 seed 可复现。
func TestUeFairTake(t *testing.T) {
	mk := func(n int, s string) []ueWindow {
		ws := make([]ueWindow, n)
		for i := range ws {
			ws[i] = ueWindow{Clip: s, Sec: i, Streamer: s}
		}
		return ws
	}
	t.Run("预算封顶", func(t *testing.T) {
		got := ueFairTake(map[string][]ueWindow{"甲": mk(2, "甲")}, 8, rand.New(rand.NewSource(1)))
		if len(got) != 2 {
			t.Fatalf("池尽应止于 2，得 %d", len(got))
		}
	})
	t.Run("充足时均衡", func(t *testing.T) {
		got := ueFairTake(map[string][]ueWindow{"甲": mk(10, "甲"), "乙": mk(10, "乙"), "丙": mk(10, "丙")}, 9, rand.New(rand.NewSource(1)))
		cnt := map[string]int{}
		for _, w := range got {
			cnt[w.Streamer]++
		}
		for s, n := range cnt {
			if n != 3 {
				t.Errorf("%s 得 %d 窗，均衡应各 3", s, n)
			}
		}
	})
	t.Run("稀缺先退场不空转", func(t *testing.T) {
		got := ueFairTake(map[string][]ueWindow{"甲": mk(10, "甲"), "乙": mk(3, "乙"), "丙": mk(1, "丙")}, 8, rand.New(rand.NewSource(1)))
		cnt := map[string]int{}
		for _, w := range got {
			cnt[w.Streamer]++
		}
		if cnt["甲"] != 4 || cnt["乙"] != 3 || cnt["丙"] != 1 {
			t.Errorf("轮转结果 4/3/1，得 %v", cnt)
		}
	})
	t.Run("同 seed 可复现", func(t *testing.T) {
		by := func() map[string][]ueWindow { return map[string][]ueWindow{"甲": mk(6, "甲"), "乙": mk(6, "乙")} }
		a := ueFairTake(by(), 6, rand.New(rand.NewSource(42)))
		b := ueFairTake(by(), 6, rand.New(rand.NewSource(42)))
		aj, _ := json.Marshal(a)
		bj, _ := json.Marshal(b)
		if string(aj) != string(bj) {
			t.Errorf("同 seed 输出不一致:\n%s\n%s", aj, bj)
		}
	})
}

// 冻结 v3 主播级红线（2026-09-28 污染事件加固）：其片不得进入飞轮采样。
func TestUncertaintyExportFrozenStreamerExcluded(t *testing.T) {
	for _, clip := range []string{
		"倦_2026-09-27_17-22-29_000",
		"倦_2026-09-28_16-44-01_003",
		"小皮_2026-09-28_09-43-44_003",
		"颜兮_2026-09-27_18-32-21_022",
	} {
		if !ueFrozenExcluded(clip) {
			t.Errorf("冻结 v3 主播片未被排除: %s", clip)
		}
	}
	for _, clip := range []string{
		"D.an_2026-09-28_09-48-07_007",
		"小欣耶耶🐰⁰⁹_2026-09-28_11-02-33_001", // 含 emoji/上标的主播名不受影响
		"无名片", // 解析失败不误伤（返回 ""，不在冻结表）
	} {
		if ueFrozenExcluded(clip) {
			t.Errorf("普通片被误排除: %s", clip)
		}
	}
}
