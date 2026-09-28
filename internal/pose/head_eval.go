package pose

// head_eval.go 学习型门头（GBDT 树集成）的纯 Go 求值器 + 窗特征构建。
//
// 用途：姿态门段级投票（开封 #7，见 _diag/train/UNSEAL7_20260927.md 与
// docs/highlight-spatial-de.md §30-§32）。头为 sklearn GradientBoostingClassifier
// 导出的树集成 JSON（_diag/train/gate_head/gate_head_v2_trees.json，v2 飞轮头：
// gold_review 15665 窗 / gesture 763 窗跨 8 主播训练）。
//
// 纯 Go 实现的理由：CGO 与非 CGO 两种构建行为必须一致（ONNX 头会使非 CGO 构建的
// 门行为分叉）；树求值 200 棵 × 深度 3，成本可忽略。
//
// ⚠️ 口径红线：HeadFeatures 的特征序与统计口径必须与训练端
// _diag/train/audio_probe/learned_gate_probe.py 的 window_feats 逐字一致，
// 有 parity 单测（golden 200 样本须与 sklearn 概率一致到 1e-9）锁死。

import (
	"encoding/json"
	"math"
	"os"
	"sort"
)

// HeadModel 学习型门头。
type HeadModel struct {
	Version      string     `json:"version"`
	Features     []string   `json:"features"`
	LearningRate float64    `json:"learning_rate"`
	InitLogodds  float64    `json:"init_logodds"`
	Trees        []headTree `json:"trees"`
	TrainSet     string     `json:"train_set"`
}

type headTree struct {
	Feature       []int     `json:"feature"`
	Threshold     []float64 `json:"threshold"`
	ChildrenLeft  []int     `json:"children_left"`
	ChildrenRight []int     `json:"children_right"`
	LeafValue     []float64 `json:"leaf_value"`
}

// LoadHeadModel 从 JSON 加载头模型。
func LoadHeadModel(path string) (*HeadModel, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m HeadModel
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if len(m.Trees) == 0 || len(m.Features) == 0 {
		return nil, os.ErrInvalid
	}
	return &m, nil
}

// HeadProb 输入 18 维窗特征（顺序 = 模型 features），返回 dance 概率。
func (m *HeadModel) HeadProb(x []float64) float64 {
	raw := m.InitLogodds
	for i := range m.Trees {
		raw += m.LearningRate * m.Trees[i].leafLogodds(x)
	}
	return 1 / (1 + math.Exp(-raw))
}

func (t *headTree) leafLogodds(x []float64) float64 {
	node := 0
	for t.ChildrenLeft[node] != -1 {
		if x[t.Feature[node]] <= t.Threshold[node] {
			node = t.ChildrenLeft[node]
		} else {
			node = t.ChildrenRight[node]
		}
	}
	return t.LeafValue[node]
}

// HeadFeatures 把一窗逐秒特征转成头的 18 维输入。
// 口径（与训练端 window_feats 一致）：
//   - det_rate = 检出秒 / 窗内秒数；
//   - vis/face/ext/aspect 的均值、总体标准差、min、max 只统计检出秒；
//   - 无检出秒：det_rate 照常、其余 16 维全 0、has_det = 0。
func HeadFeatures(feats []FrameFeatures) []float64 {
	out := make([]float64, 18)
	if len(feats) == 0 {
		return out
	}
	out[0] = 0
	det := 0
	var vs, fs, es, as_ []float64
	for _, f := range feats {
		if !f.Detected {
			continue
		}
		det++
		vs = append(vs, f.VisRatio)
		fs = append(fs, f.FaceFrac)
		es = append(es, f.ExtH)
		as_ = append(as_, f.Aspect)
	}
	out[0] = float64(det) / float64(len(feats))
	if det == 0 {
		return out // has_det = 0，其余 16 维全 0
	}
	mean := func(xs []float64) float64 {
		s := 0.0
		for _, v := range xs {
			s += v
		}
		return s / float64(len(xs))
	}
	std := func(xs []float64) float64 {
		m := mean(xs)
		v := 0.0
		for _, x := range xs {
			v += (x - m) * (x - m)
		}
		return math.Sqrt(v / float64(len(xs)))
	}
	minv := func(xs []float64) float64 {
		m := xs[0]
		for _, x := range xs {
			if x < m {
				m = x
			}
		}
		return m
	}
	maxv := func(xs []float64) float64 {
		m := xs[0]
		for _, x := range xs {
			if x > m {
				m = x
			}
		}
		return m
	}
	out[1], out[5], out[6], out[7] = mean(vs), std(vs), minv(vs), maxv(vs)
	out[2], out[8], out[9], out[10] = mean(fs), std(fs), minv(fs), maxv(fs)
	out[3], out[11], out[12], out[13] = mean(es), std(es), minv(es), maxv(es)
	out[4], out[14], out[15], out[16] = mean(as_), std(as_), minv(as_), maxv(as_)
	out[17] = 1
	return out
}

// headSeconds 5fps 帧序列 → 每秒首帧特征（与 1fps 训练口径对齐：秒 s 取
// 该秒内最早一帧的 FeaturesFromFrame 输出）。
func headSeconds(ffs []*FrameFeatures, fps int) []FrameFeatures {
	if fps <= 0 {
		fps = 5
	}
	buckets := map[int]*FrameFeatures{}
	order := []int{}
	for i, ff := range ffs {
		if ff == nil {
			continue
		}
		sec := i / fps
		if _, ok := buckets[sec]; !ok {
			cp := *ff
			buckets[sec] = &cp
			order = append(order, sec)
		}
	}
	sort.Ints(order)
	out := make([]FrameFeatures, 0, len(order))
	for _, sec := range order {
		out = append(out, *buckets[sec])
	}
	return out
}

// headSegmentVote 段级投票：滑 8s 窗（尾窗截短，与训练口径一致），统计头判舞
// （prob ≥ 0.5）窗占比，≥ frac 保留。窗数 0（段过短）→ 保留（不误杀）。
func headSegmentVote(secs []FrameFeatures, m *HeadModel, frac float64) (bool, float64) {
	if len(secs) == 0 || m == nil {
		return true, 0
	}
	dance, total := 0, 0
	for off := 0; off < len(secs); off += 8 {
		hi := off + 8
		if hi > len(secs) {
			hi = len(secs)
		}
		x := HeadFeatures(secs[off:hi])
		p := m.HeadProb(x)
		total++
		if p >= 0.5 {
			dance++
		}
	}
	if total == 0 {
		return true, 0
	}
	return float64(dance)/float64(total) >= frac, float64(dance) / float64(total)
}
