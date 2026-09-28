package pose

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// golden 路径：仓库工作树内。缺失则跳过（其他机器/CI 无该产物）。
// 环境变量 HEAD_PARITY_GOLDEN / HEAD_PARITY_MODEL 可覆盖（验收新头版本用），默认生产 v2 产物。
const defaultGoldenPath = "D:/upload/_diag/train/gate_head/golden_parity.json"
const defaultHeadPath = "D:/upload/_diag/train/gate_head/gate_head_v2_trees.json"

func goldenPath() string {
	if v := os.Getenv("HEAD_PARITY_GOLDEN"); v != "" {
		return v
	}
	return defaultGoldenPath
}

func headPath() string {
	if v := os.Getenv("HEAD_PARITY_MODEL"); v != "" {
		return v
	}
	return defaultHeadPath
}

// TestHeadParitySklearn 头求值器与 sklearn 概率一致性：200 golden 样本须 1e-9 内一致。
func TestHeadParitySklearn(t *testing.T) {
	if _, err := os.Stat(goldenPath()); err != nil {
		t.Skip("golden 数据不存在，跳过")
	}
	m, err := LoadHeadModel(headPath())
	if err != nil {
		t.Fatalf("加载头失败: %v", err)
	}
	b, err := os.ReadFile(goldenPath())
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		X  [][]float64 `json:"X"`
		P1 []float64   `json:"p1"`
	}
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatal(err)
	}
	for i, x := range golden.X {
		p := m.HeadProb(x)
		if math.Abs(p-golden.P1[i]) > 1e-9 {
			t.Fatalf("样本 %d 概率不一致: go=%.12f sklearn=%.12f", i, p, golden.P1[i])
		}
	}
}

// TestHeadFeaturesShape 维度与 has_det 语义。
func TestHeadFeaturesShape(t *testing.T) {
	full := HeadFeatures([]FrameFeatures{
		{Detected: true, VisRatio: 0.8, FaceFrac: 0.05, ExtH: 0.5, Aspect: 0.6},
		{Detected: true, VisRatio: 0.9, FaceFrac: 0.06, ExtH: 0.6, Aspect: 0.7},
	})
	if len(full) != 18 {
		t.Fatalf("应 18 维，得 %d", len(full))
	}
	if full[0] != 1.0 || full[17] != 1.0 {
		t.Fatalf("全检出窗 det_rate/has_det 应为 1: %v %v", full[0], full[17])
	}
	if math.Abs(full[1]-0.85) > 1e-12 {
		t.Fatalf("vis_mean 应 0.85，得 %v", full[1])
	}
	empty := HeadFeatures([]FrameFeatures{{Detected: false}, {Detected: false}})
	if empty[0] != 0.0 || empty[17] != 0.0 || empty[1] != 0 {
		t.Fatalf("无检出窗口径错: %v", empty)
	}
}

// TestHeadSegmentVote 段级投票：强舞段保留、强非舞段否决、过短段保留。
func TestHeadSegmentVote(t *testing.T) {
	// 手工单叉树：feature0 > 0.5 → 舞（logodds +10），否则非舞（-10）
	m := &HeadModel{
		LearningRate: 1.0,
		InitLogodds:  0,
		Trees: []headTree{{
			Feature:       []int{1, 1, 1},
			Threshold:     []float64{0.5, 0, 0},
			ChildrenLeft:  []int{1, -1, -1},
			ChildrenRight: []int{2, -1, -1},
			LeafValue:     []float64{0, -10, 10},
		}},
	}
	mkSecs := func(f0 float64, n int) []FrameFeatures {
		secs := make([]FrameFeatures, 0, n)
		for i := 0; i < n; i++ {
			secs = append(secs, FrameFeatures{Detected: true, VisRatio: f0, FaceFrac: 0.1, ExtH: 0.5, Aspect: 0.6})
		}
		return secs
	}
	// 16 秒全高特征 → 两窗全判舞 → frac 1.0 ≥ 0.5 → 保留
	ok, frac := headSegmentVote(mkSecs(0.9, 16), m, 0.5)
	if !ok || math.Abs(frac-1.0) > 1e-9 {
		t.Fatalf("强舞段应保留 frac=1.0，得 ok=%v frac=%v", ok, frac)
	}
	// 16 秒全低特征 → 两窗全非舞 → frac 0 → 否决
	ok, frac = headSegmentVote(mkSecs(0.1, 16), m, 0.5)
	if ok || frac != 0 {
		t.Fatalf("强非舞段应否决，得 ok=%v frac=%v", ok, frac)
	}
	// 3 秒过短段（<8 窗仍成窗）→ 高特征保留
	ok, _ = headSegmentVote(mkSecs(0.9, 3), m, 0.5)
	if !ok {
		t.Fatal("过短强舞段应保留")
	}
	// 空段 → 保留（不误杀）
	if ok, _ := headSegmentVote(nil, m, 0.5); !ok {
		t.Fatal("空段应保留")
	}
}

// TestHeadSecondsBucketing 5fps 帧按秒取首帧。
func TestHeadSecondsBucketing(t *testing.T) {
	ffs := make([]*FrameFeatures, 12) // 2.4 秒 @5fps
	for i := range ffs {
		ffs[i] = &FrameFeatures{Detected: true, VisRatio: float64(i)}
	}
	secs := headSeconds(ffs, 5)
	if len(secs) != 3 {
		t.Fatalf("12 帧 @5fps 应 3 秒桶，得 %d", len(secs))
	}
	// 每桶首帧：秒 0 → 帧 0（vis 0），秒 1 → 帧 5（vis 5），秒 2 → 帧 10（vis 10）
	for i, want := range []float64{0, 5, 10} {
		if secs[i].VisRatio != want {
			t.Fatalf("桶 %d 首帧 vis 应 %v，得 %v", i, want, secs[i].VisRatio)
		}
	}
}
