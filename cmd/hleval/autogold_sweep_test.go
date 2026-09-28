package main

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"
)

func agTestFeats(n int, det []bool, vis, face float64) [][5]float64 {
	fs := make([][5]float64, n)
	for i := range fs {
		if det[i] {
			fs[i] = [5]float64{vis, face, 0.5, 1.2, 1}
		}
	}
	return fs
}

func TestAgBuildRows(t *testing.T) {
	gold := map[string]map[string]string{
		"a": {"0": "dance", "8": "chat", "16": "dance"}, // 16 超出特征长度，应跳过
		"b": {"0": "gesture"},
		"c": {"0": "dance"}, // c 无特征，整片跳过
	}
	poseFeats := map[string]agClipFeats{
		"a": {FPS: 1, Feats: agTestFeats(10,
			[]bool{true, true, true, true, true, true, true, true, false, false}, 0.8, 0.1)},
		"b": {FPS: 1, Feats: agTestFeats(8,
			[]bool{true, true, true, true, false, false, false, false}, 0.6, 0.2)},
	}
	rows := agBuildRows(gold, poseFeats)
	if len(rows) != 3 {
		t.Fatalf("期望 3 行（a 两窗 + b 一窗，窗 16 越界跳过、片 c 无特征跳过），得 %d", len(rows))
	}
	// a 窗 0：全检出 → det率 1.0，均值只算检出秒
	r := rows[0]
	if r.Label != "dance" || r.DetRate != 1.0 ||
		math.Abs(r.VisMean-0.8) > 1e-9 || math.Abs(r.FaceMean-0.1) > 1e-9 {
		t.Fatalf("a 窗0 统计不符: %+v", r)
	}
	// a 窗 8：尾窗只剩 2 秒且全未检出 → det率 0、均值 0
	r = rows[1]
	if r.Label != "chat" || r.DetRate != 0 || r.VisMean != 0 || r.FaceMean != 0 {
		t.Fatalf("a 窗8 统计不符: %+v", r)
	}
	// b 窗 0：半检出 → det率 0.5，均值只算检出的 4 秒
	r = rows[2]
	if r.Label != "gesture" || math.Abs(r.DetRate-0.5) > 1e-9 ||
		math.Abs(r.VisMean-0.6) > 1e-9 || math.Abs(r.FaceMean-0.2) > 1e-9 {
		t.Fatalf("b 窗0 统计不符: %+v", r)
	}
}

func TestAgEvalPointAndGrid(t *testing.T) {
	rows := []agRow{
		{DetRate: 1, VisMean: 0.8, FaceMean: 0.1, Label: "dance"},    // 预测舞 ✓
		{DetRate: 1, VisMean: 0.2, FaceMean: 0.1, Label: "chat"},     // vis 低 → 非舞 ✓
		{DetRate: 1, VisMean: 0.8, FaceMean: 0.5, Label: "closeup"},  // face 高 → 非舞 ✓
		{DetRate: 0.05, VisMean: 0.9, FaceMean: 0.1, Label: "dance"}, // det 低 → 漏
	}
	p := agEvalPoint(rows, 0.6, 0.3, 0.3)
	if p.TP != 1 || p.FP != 0 || p.FN != 1 {
		t.Fatalf("TP/FP/FN 不符: %+v", p)
	}
	if p.P != 1 || p.R != 0.5 || math.Abs(p.F1-2*0.5/1.5) > 1e-9 {
		t.Fatalf("P/R/F1 不符: %+v", p)
	}
	// 网格最优稳定排序：并列 F1 时保持 V 外层、F 中层、D 内层的插入序
	g := agGrid(rows)
	if g[0].F1 < g[len(g)-1].F1 {
		t.Fatal("网格未按 F1 降序")
	}
	for i := 1; i < len(g); i++ {
		if g[i-1].F1 == g[i].F1 && agGridLess(&g[i], &g[i-1]) {
			t.Fatalf("并列 F1 顺序漂移 @%d", i)
		}
	}
}

// agGridLess 检查并列时是否违反插入序（V,F,D 字典序）。
func agGridLess(a, b *agPoint) bool {
	if a.V != b.V {
		return a.V < b.V
	}
	if a.F != b.F {
		return a.F < b.F
	}
	return a.D < b.D
}

func TestAgGestureStats(t *testing.T) {
	rows := []agRow{
		{DetRate: 1, VisMean: 0.8, FaceMean: 0.1, Label: "gesture"},
		{DetRate: 1, VisMean: 0.8, FaceMean: 0.1, Label: "gesture"},
		{DetRate: 0, VisMean: 0, FaceMean: 0, Label: "gesture"},
		{DetRate: 1, VisMean: 0.8, FaceMean: 0.1, Label: "dance"},
	}
	best := agPoint{V: 0.6, F: 0.3, D: 0.3}
	g := agGestureStats(rows, best, nil, nil)
	if g == nil || g.Windows != 3 || g.PredDanceBest != 2 || g.PredDanceBestPct != 66.7 {
		t.Fatalf("gesture 统计不符: %+v", g)
	}
	if g.PredDanceLive != nil || g.PredDanceLivePct != nil {
		t.Fatal("无 live 配置时 live 字段应为 nil")
	}
	live := &agLiveGate{Enable: true, VisMin: 0.6, FaceMax: 0.3, DetMin: 0.3}
	g = agGestureStats(rows, best, live, &agPoint{})
	if g.PredDanceLive == nil || *g.PredDanceLive != 2 || *g.PredDanceLivePct != 66.7 {
		t.Fatalf("live 判舞率不符: %+v", g)
	}
	if agGestureStats([]agRow{{Label: "dance"}}, best, nil, nil) != nil {
		t.Fatal("无 gesture 窗应返回 nil")
	}
}

func TestAgRoundingMatchesJS(t *testing.T) {
	// JS +toFixed(3)：0.7896 → 0.79（尾随 0 消失）；0.5 进位
	if agRound3(0.7896) != 0.79 || agRound3(0.5) != 0.5 {
		t.Fatalf("agRound3 不符: %v %v", agRound3(0.7896), agRound3(0.5))
	}
	if agRound3(0.78949) != 0.789 {
		t.Fatalf("agRound3 边界不符: %v", agRound3(0.78949))
	}
	// JS +(x).toFixed(1)：79.04 → 79（整数序列化无小数位）
	if agRound1(79.04) != 79 || agRound1(79.06) != 79.1 {
		t.Fatalf("agRound1 不符: %v %v", agRound1(79.04), agRound1(79.06))
	}
}

func TestAgResultJSONShape(t *testing.T) {
	live := 55
	pct := 71.4
	out := agResultOut{
		GeneratedAt: "2026/9/26 15:30:00", Windows: 10, DanceWindows: 3, GoldClips: 2,
		Best:     agMetricOut{0.7, 0.12, 0.3, 0.693, 0.915, 0.789},
		Top:      []agMetricOut{{0.7, 0.12, 0.3, 0.693, 0.915, 0.789}},
		AgreePct: 79, Gesture: &agGestureOut{
			Windows: 77, PredDanceBest: 55, PredDanceBestPct: 71.4,
			PredDanceLive: &live, PredDanceLivePct: &pct,
		},
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if json.Unmarshal(b, &m) != nil {
		t.Fatal("结果非合法 JSON")
	}
	for _, k := range []string{"generated_at", "windows", "dance_windows", "gold_clips",
		"best", "live", "agree_pct", "gesture", "top"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("缺字段 %s", k)
		}
	}
	// agree_pct=79 序列化须为 79（JS +("79.0") === 79）
	if v, ok := m["agree_pct"].(float64); !ok || v != 79 {
		t.Fatalf("agree_pct 序列化不符: %v", m["agree_pct"])
	}
}

// GroupKFold：片数不足 2 时返回 nil（宁可不报，也不报假数字）。
func TestAgGroupKFoldNeedsTwoClips(t *testing.T) {
	rows := []agRow{
		{Clip: "only", DetRate: 1, VisMean: 0.8, FaceMean: 0.1, Label: "dance"},
		{Clip: "only", DetRate: 1, VisMean: 0.2, FaceMean: 0.1, Label: "chat"},
	}
	if agGroupKFold(rows, 5) != nil {
		t.Fatal("单一片时应返回 nil")
	}
	if agGroupKFold(nil, 5) != nil {
		t.Fatal("空输入应返回 nil")
	}
}

// GroupKFold 必须按「片」分折：每行恰好被评估一次（无重复无遗漏），
// 且折数受片数约束。构造「两类特征完全相同」的数据集——此时任何阈值都分不开，
// OOF F1 必须显著低于 1（若实现按窗随机切分造成泄漏，这里会虚高）。
func TestAgGroupKFoldSplitsByClip(t *testing.T) {
	var rows []agRow
	for i := 0; i < 10; i++ {
		clip := "clip" + strconv.Itoa(i)
		lab := "dance"
		if i%2 == 1 {
			lab = "chat" // 奇偶片标签相反，但特征值完全相同 → 不可分
		}
		for w := 0; w < 6; w++ {
			rows = append(rows, agRow{Clip: clip, DetRate: 1, VisMean: 0.8, FaceMean: 0.1, Label: lab})
		}
	}
	cv := agGroupKFold(rows, 5)
	if cv == nil {
		t.Fatal("10 片应能分 5 折")
	}
	if cv.Folds != 5 {
		t.Fatalf("折数应为 5，得 %d", cv.Folds)
	}
	if cv.Windows != len(rows) {
		t.Fatalf("OOF 窗数应为 %d，得 %d", len(rows), cv.Windows)
	}
	sum := 0
	for _, fd := range cv.PerFold {
		if fd.Clips != 2 {
			t.Fatalf("第 %d 折应含 2 片（10 片轮转分 5 折），得 %d", fd.Fold, fd.Clips)
		}
		sum += fd.Wins
	}
	if sum != len(rows) {
		t.Fatalf("各折窗数之和 %d 应等于总窗数 %d（重复或遗漏）", sum, len(rows))
	}
	if cv.F1 > 0.75 {
		t.Fatalf("特征不可分时 OOF F1 应低，得 %.3f（疑似训练/测试折泄漏）", cv.F1)
	}
}

// agMergeSegs：gap=0 只合并严格相邻窗；gap 越大允许的间隔越大。
// 注意语义：gap 是**允许的最大间隔秒数**（gap=8 = 中间空一个窗仍算同段）。
func TestAgMergeSegs(t *testing.T) {
	// 窗起始秒 0,8,16 连续（合并后占 0-24）+ 32（与 24 相隔 8 秒）
	in := []int{0, 8, 16, 32}
	got := agMergeSegs(in, 0)
	if len(got) != 2 || got[0] != [2]int{0, 24} || got[1] != [2]int{32, 40} {
		t.Fatalf("gap=0 应得 [0,24]+[32,40]，得 %v", got)
	}
	if got = agMergeSegs(in, 4); len(got) != 2 {
		t.Fatalf("gap=4 时间隔 8 秒不应合并，得 %v", got)
	}
	got = agMergeSegs(in, 8)
	if len(got) != 1 || got[0] != [2]int{0, 40} {
		t.Fatalf("gap=8 时 8 秒间隔应合并为 [0,40]，得 %v", got)
	}
	if len(agMergeSegs(nil, 0)) != 0 {
		t.Fatal("空输入应返回空段")
	}
}

// agSegEval：两种判据的差别 —— IoU@0.5 要求重叠过半，重叠判据只看绝对秒数。
func TestAgSegEval(t *testing.T) {
	// 4 个窗（0..32），金标只标了前 2 个（0,8）；门把 4 个都判舞
	rows := []agRow{
		{Clip: "c", Sec: 0, DetRate: 1, VisMean: 0.8, FaceMean: 0.1, Label: "dance"},
		{Clip: "c", Sec: 8, DetRate: 1, VisMean: 0.8, FaceMean: 0.1, Label: "dance"},
		{Clip: "c", Sec: 16, DetRate: 1, VisMean: 0.8, FaceMean: 0.1, Label: "chat"},
		{Clip: "c", Sec: 24, DetRate: 1, VisMean: 0.8, FaceMean: 0.1, Label: "chat"},
	}
	// 预测段 [0,32)、金标段 [0,16)：重叠 16、并集 32 → IoU 恰为 0.5 → 命中
	s := agSegEval(rows, 0.7, 0.12, 0.3, 0, 0)
	if s.PredSegs != 1 || s.GoldSegs != 1 || s.TP != 1 || s.FP != 0 || s.FN != 0 {
		t.Fatalf("IoU@0.5 边界应命中，得 %+v", s)
	}
	// 重叠判据要求 ≥24 秒，实际只重叠 16 秒 → 不命中
	s = agSegEval(rows, 0.7, 0.12, 0.3, 0, 24)
	if s.TP != 0 || s.FP != 1 || s.FN != 1 {
		t.Fatalf("min_ov=24 应不命中，得 %+v", s)
	}
	// gap 不改变本例（两侧本就各自连续）
	s = agSegEval(rows, 0.7, 0.12, 0.3, 8, 0)
	if s.PredSegs != 1 || s.GoldSegs != 1 {
		t.Fatalf("gap=8 本例段数不应变化，得 %+v", s)
	}
	// 空输入不 panic
	if s = agSegEval(nil, 0.7, 0.12, 0.3, 0, 0); s.PredSegs != 0 || s.GoldSegs != 0 {
		t.Fatalf("空输入应得零段，得 %+v", s)
	}
}
