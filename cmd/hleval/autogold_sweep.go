package main

// autogold-sweep 金标重定标（原 _diag/train/autogold_sweep.js 的 Go 化）。
//
// 门口径与生产一致：8s 窗，det率<detmin → 非舞；否则检出秒 vis 均值≥visMin
// 且 face 均值≤faceMax → 舞。窗级统计直接调 pose.AggregateWindow（生产同一份
// 数学，detMin/visMin/faceMax 传 0 只取原始统计），避免第二套实现分叉。
//
// gesture=手势聊天/轻晃（开封#5 盲区片 小妤_2026-09-25_20-13-29_010，见
// _diag/train/UNSEAL5_20260926.md）：窗按非舞计入 P/R/F1，另单独盯门在该类
// 上的判舞率——阈值层解决不了该盲区时，这个数字就是特征方向工作的基线。
//
// 输出 autogold_result.json 与 JS 版逐字段同构（控制台「姿态训练」页与 8080
// 自动应用防抖按此文件消费），落盘 tmp+rename 原子写。
//
// 用法：
//
//	hleval autogold-sweep [-gold <json>] [-pose <json>] [-config <json>] [-out <json>]

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"time"

	"upload/internal/pose"
)

const agWinSec = 8

type agClipFeats struct {
	FPS   int          `json:"fps"`
	Feats [][5]float64 `json:"feats"`
}

// agRow 一窗的原始统计 + 金标（Label 取 gold_review 的类别串，dance 之外全按非舞）。
// Clip 供按片分组的交叉验证（agGroupKFold）与段级评估使用——同一片的窗高度自相关，
// 按窗随机切分会泄漏，必须按片分组。Sec 是该窗的起始**秒**（段级合并用）。
type agRow struct {
	Clip                       string
	Sec                        int
	DetRate, VisMean, FaceMean float64
	Label                      string
}

func (r agRow) goldDance() bool { return r.Label == "dance" }

// agPoint 一个阈值组合的评估结果（P/R/F1 落盘前按 JS 口径舍入到 3 位小数）。
type agPoint struct {
	V, F, D     float64
	P, R, F1    float64
	TP, FP, FN  int
	P3, R3, F13 float64 // JS +toFixed(3) 等价值
}

type agMetricOut struct {
	Vis  float64 `json:"vis"`
	Face float64 `json:"face"`
	Det  float64 `json:"det"`
	P    float64 `json:"P"`
	R    float64 `json:"R"`
	F1   float64 `json:"F1"`
}

type agGestureOut struct {
	Windows          int      `json:"windows"`
	PredDanceBest    int      `json:"pred_dance_best"`
	PredDanceBestPct float64  `json:"pred_dance_best_pct"`
	PredDanceLive    *int     `json:"pred_dance_live"`
	PredDanceLivePct *float64 `json:"pred_dance_live_pct"`
}

// agCVFold 一折的评估结果：该折在其余片上选中的阈值 + 在该折（未见过的片）上的 F1。
type agCVFold struct {
	Fold  int     `json:"fold"`
	Clips int     `json:"clips"`
	Wins  int     `json:"windows"`
	Vis   float64 `json:"vis"`
	Face  float64 `json:"face"`
	Det   float64 `json:"det"`
	F1    float64 `json:"F1"`
}

// agCVOut 按片分组交叉验证的 out-of-fold 汇总。
type agCVOut struct {
	Folds   int        `json:"folds"`
	Windows int        `json:"windows"`
	P       float64    `json:"P"`
	R       float64    `json:"R"`
	F1      float64    `json:"F1"`
	TP      int        `json:"TP"`
	FP      int        `json:"FP"`
	FN      int        `json:"FN"`
	PerFold []agCVFold `json:"per_fold"`
}

// agSegOut 段级评估结果：窗级预测/金标按 gapSec 合并成段后贪心配对。
// 口径与 _diag/train/_hyst_probe.py 逐字对齐（同一份数学，避免第二套实现分叉）。
type agSegOut struct {
	GapSec   int     `json:"gap_sec"`    // 段合并允许的间隔秒数（0 = 严格相邻，unseal6_eval 旧口径）
	MinOvSec int     `json:"min_ov_sec"` // 命中所需最小重叠秒数（0 = 用 IoU@0.5）
	PredSegs int     `json:"pred_segs"`
	GoldSegs int     `json:"gold_segs"`
	P        float64 `json:"P"`
	R        float64 `json:"R"`
	F1       float64 `json:"F1"`
	TP       int     `json:"TP"`
	FP       int     `json:"FP"`
	FN       int     `json:"FN"`
}

type agResultOut struct {
	GeneratedAt  string        `json:"generated_at"`
	Windows      int           `json:"windows"`
	DanceWindows int           `json:"dance_windows"`
	GoldClips    int           `json:"gold_clips"`
	Best         agMetricOut   `json:"best"`
	Live         *agMetricOut  `json:"live"`
	AgreePct     float64       `json:"agree_pct"`
	Gesture      *agGestureOut `json:"gesture"`
	CV           *agCVOut      `json:"cv,omitempty"`  // 按片分组外推（选阈值与评估分离）
	Seg          *agSegOut     `json:"seg,omitempty"` // 段级（旧口径 gap=0/IoU@0.5，与 unseal6_eval 可比）
	Top          []agMetricOut `json:"top"`
}

type agLiveGate struct {
	Enable  bool    `json:"enable"`
	VisMin  float64 `json:"vis_min"`
	FaceMax float64 `json:"face_max"`
	DetMin  float64 `json:"det_min"`
}

func cmdAutogoldSweep(args []string) {
	fs := flag.NewFlagSet("autogold-sweep", flag.ExitOnError)
	goldPath := fs.String("gold", "D:/upload/_diag/train/_pose_pilot/gold_review.json", "金标窗级标签 JSON")
	posePath := fs.String("pose", "D:/upload/_diag/train/pose_features_go.json", "每秒姿态特征 JSON")
	cfgPath := fs.String("config", "D:/upload/config.json", "生产 config.json（读 highlight_pose_gate 现值供防抖比对）")
	outPath := fs.String("out", "D:/upload/_diag/train/autogold_result.json", "机器可读结果输出路径")
	fs.Parse(args)

	gold := map[string]map[string]string{}
	if b, err := os.ReadFile(*goldPath); err != nil {
		fmt.Fprintf(os.Stderr, "读金标失败 %s: %v\n", *goldPath, err)
		os.Exit(1)
	} else if err := json.Unmarshal(b, &gold); err != nil {
		fmt.Fprintf(os.Stderr, "解析金标失败: %v\n", err)
		os.Exit(1)
	}
	poseFeats := map[string]agClipFeats{}
	if b, err := os.ReadFile(*posePath); err != nil {
		fmt.Fprintf(os.Stderr, "读姿态特征失败 %s: %v\n", *posePath, err)
		os.Exit(1)
	} else if err := json.Unmarshal(b, &poseFeats); err != nil {
		fmt.Fprintf(os.Stderr, "解析姿态特征失败: %v\n", err)
		os.Exit(1)
	}

	rows := agBuildRows(gold, poseFeats)
	dance := 0
	for _, r := range rows {
		if r.goldDance() {
			dance++
		}
	}
	fmt.Printf("可比窗数: %d (dance 窗: %d)\n", len(rows), dance)

	all := agGrid(rows)
	best := all[0]
	fmt.Printf("\nTOP 8（按 F1）:\n")
	fmt.Printf(" vis   face  detmin |   P     R     F1   | TP/FP/FN\n")
	for _, p := range all[:min8(len(all))] {
		fmt.Printf(" %.2f  %.2f  %.2f  | %.3f %.3f %.3f | %d/%d/%d\n",
			p.V, p.F, p.D, p.P3, p.R3, p.F13, p.TP, p.FP, p.FN)
	}

	// §22 定标点（网格内），live 现值单独评估
	refs := []struct {
		name    string
		V, F, D float64
		p       *agPoint
	}{{"§22 定标", 0.6, 0.14, 0.2, agFind(all, 0.6, 0.14, 0.2)}}
	var liveCfg *agLiveGate
	var livePt *agPoint
	if g := agLoadLiveGate(*cfgPath); g != nil {
		liveCfg = g
		p := agEvalPoint(rows, g.VisMin, g.FaceMax, g.DetMin)
		livePt = &p
		refs = append(refs, struct {
			name    string
			V, F, D float64
			p       *agPoint
		}{"live 现值(生产)", g.VisMin, g.FaceMax, g.DetMin, livePt})
	}
	for _, ref := range refs {
		if ref.p == nil {
			continue
		}
		// JS 版 live 行 FP/FN 打印 undefined（对象缺字段）；Go 版补真实值
		fmt.Printf(" %s: vis%v/face%v/det%v → P=%.3f R=%.3f F1=%.3f (FP=%d FN=%d)\n",
			ref.name, ref.V, ref.F, ref.D, ref.p.P3, ref.p.R3, ref.p.F13, ref.p.FP, ref.p.FN)
	}

	// 自动标签一致率（按网格最优阈值）
	agree := 0
	for _, r := range rows {
		if agPred(r, best.V, best.F, best.D) == r.goldDance() {
			agree++
		}
	}
	agreePct := 100 * float64(agree) / float64(len(rows))
	fmt.Printf("\n自动标签 dance/非舞 二分类一致率: %.1f%%（阈值 vis%v/face%v/det%v）\n",
		agreePct, best.V, best.F, best.D)

	// 按片分组交叉验证：选阈值与评估分离，去掉「在同批数据上选阈值」的虚高
	cv := agGroupKFold(rows, agCVFolds)
	if cv != nil {
		fmt.Printf("\n=== GroupKFold(%d) 按片分组外推（阈值只在训练折选，测试折未见）===\n", cv.Folds)
		fmt.Printf(" 折  片数   窗数   vis   face  det  |  折内 F1\n")
		for _, fd := range cv.PerFold {
			fmt.Printf(" %2d  %4d  %5d   %.2f  %.2f  %.2f |  %.3f\n",
				fd.Fold, fd.Clips, fd.Wins, fd.Vis, fd.Face, fd.Det, fd.F1)
		}
		fmt.Printf(" OOF 汇总: P=%.3f R=%.3f F1=%.3f (TP/FP/FN=%d/%d/%d，%d 窗)\n",
			cv.P, cv.R, cv.F1, cv.TP, cv.FP, cv.FN, cv.Windows)
		if d := best.F13 - cv.F1; d < 0.005 {
			fmt.Printf(" 对比 in-sample best F1=%.3f → 差 %.3f：阈值是各折一致选出的稳健点，\n", best.F13, d)
			fmt.Printf(" 不是靠全量过拟合。真实差距在段级聚合（冻结集段级 F1 0.211 vs 窗级 %.3f）。\n", cv.F1)
		} else {
			fmt.Printf(" 对比 in-sample best F1=%.3f → 差 %.3f 即「在同批数据上选阈值」的虚高量。\n",
				best.F13, d)
		}
	}

	// 段级评估：多档 gap × 两种判据，如实暴露口径敏感性（不做推荐，见 §27）
	segOld := agSegEval(rows, best.V, best.F, best.D, 0, 0)
	fmt.Printf("\n=== 段级评估（gap = 允许合并的间隔秒数；判据二选一）===\n")
	fmt.Printf(" gap(秒)  判据         预测段  GT段      P       R      F1\n")
	for _, g := range []int{0, 8, 24} {
		for _, ov := range []int{0, 8} {
			s := agSegEval(rows, best.V, best.F, best.D, g, ov)
			label := "IoU@0.5"
			if ov > 0 {
				label = fmt.Sprintf("重叠>=%d秒", ov)
			}
			mark := ""
			if g == 0 && ov == 0 {
				mark = "  <- 旧口径（unseal6_eval）"
			}
			fmt.Printf("  %3d    %-12s %5d  %5d   %.3f   %.3f   %.3f%s\n",
				g, label, s.PredSegs, s.GoldSegs, s.P, s.R, s.F1, mark)
		}
	}
	fmt.Printf(" ⚠️ 段级数字随 gap/判据大幅摆动且非单调，口径固定前不可用于参数决策（§27）\n")

	// gesture 类别：门判舞=误报（该类含 13 真舞窗，真舞漏杀计入 FN）
	gestOut := agGestureStats(rows, best, liveCfg, livePt)
	if g := gestOut; g != nil && g.Windows > 0 {
		line := fmt.Sprintf("gesture 窗: %d，判舞率 best(vis%v/face%v/det%v) %d=%.1f%%",
			g.Windows, best.V, best.F, best.D, g.PredDanceBest, g.PredDanceBestPct)
		if g.PredDanceLive != nil {
			line += fmt.Sprintf(" / live %d=%.1f%%", *g.PredDanceLive, *g.PredDanceLivePct)
		}
		fmt.Println(line)
	}

	top := make([]agMetricOut, 0, 10)
	for _, p := range all[:min10(len(all))] {
		top = append(top, agMetricOut{p.V, p.F, p.D, p.P3, p.R3, p.F13})
	}
	out := agResultOut{
		GeneratedAt:  time.Now().Format("2006/1/2 15:04:05"), // 24 小时制（曾误用 3=12h 致下午时间显示成上午）
		Windows:      len(rows),
		DanceWindows: dance,
		GoldClips:    len(gold),
		Best:         agMetricOut{best.V, best.F, best.D, best.P3, best.R3, best.F13},
		AgreePct:     agRound1(agreePct),
		Gesture:      gestOut,
		CV:           cv,
		Seg:          &segOld,
		Top:          top,
	}
	if liveCfg != nil && livePt != nil {
		out.Live = &agMetricOut{liveCfg.VisMin, liveCfg.FaceMax, liveCfg.DetMin, livePt.P3, livePt.R3, livePt.F13}
	}
	b, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "序列化结果失败: %v\n", err)
		os.Exit(1)
	}
	// 原子写：8080 /live 防抖周期读取本文件，避免读到截断 JSON
	tmp := *outPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写结果失败: %v\n", err)
		os.Exit(1)
	}
	if err := os.Rename(tmp, *outPath); err != nil {
		fmt.Fprintf(os.Stderr, "替换结果失败: %v\n", err)
		os.Exit(1)
	}
}

func min8(n int) int {
	if n > 8 {
		return 8
	}
	return n
}

func min10(n int) int {
	if n > 10 {
		return 10
	}
	return n
}

// agBuildRows 金标窗 × 姿态特征 → 窗统计行。窗口径与 JS 版一致：
// seg=feats[s:s+8]（尾窗可短），空窗跳过；整片无特征则整片跳过。
func agBuildRows(gold map[string]map[string]string, poseFeats map[string]agClipFeats) []agRow {
	clips := make([]string, 0, len(gold))
	for c := range gold {
		clips = append(clips, c)
	}
	sort.Strings(clips)
	rows := make([]agRow, 0, 16384)
	for _, clip := range clips {
		cf, ok := poseFeats[clip]
		if !ok || len(cf.Feats) == 0 {
			continue
		}
		wins := make([]int, 0, len(gold[clip]))
		for k := range gold[clip] {
			s, err := strconv.Atoi(k)
			if err != nil {
				continue
			}
			wins = append(wins, s)
		}
		sort.Ints(wins)
		for _, s := range wins {
			if s < 0 || s >= len(cf.Feats) {
				continue
			}
			e := s + agWinSec
			if e > len(cf.Feats) {
				e = len(cf.Feats)
			}
			win := make([]pose.FrameFeatures, e-s)
			for i, f := range cf.Feats[s:e] {
				win[i] = pose.FrameFeatures{
					Detected: f[4] == 1,
					VisRatio: f[0],
					FaceFrac: f[1],
					ExtH:     f[2],
					Aspect:   f[3],
				}
			}
			// 阈值传 0 只取原始统计（DetRate/VisMean/FaceMean），标签不用
			w := pose.AggregateWindow(win, 0, 0, 0)
			rows = append(rows, agRow{
				Clip:     clip,
				Sec:      s,
				DetRate:  w.DetRate,
				VisMean:  w.VisMean,
				FaceMean: w.FaceMean,
				Label:    gold[clip][strconv.Itoa(s)],
			})
		}
	}
	return rows
}

func agPred(r agRow, V, F, D float64) bool {
	return r.DetRate >= D && r.VisMean >= V && r.FaceMean <= F
}

func agEvalPoint(rows []agRow, V, F, D float64) agPoint {
	p := agPoint{V: V, F: F, D: D}
	for _, r := range rows {
		pred := agPred(r, V, F, D)
		switch {
		case pred && r.goldDance():
			p.TP++
		case pred && !r.goldDance():
			p.FP++
		case !pred && r.goldDance():
			p.FN++
		}
	}
	p.P, p.R = agPR(p.TP, p.FP, p.FN)
	p.F1 = agF1(p.P, p.R)
	p.P3, p.R3, p.F13 = agRound3(p.P), agRound3(p.R), agRound3(p.F1)
	return p
}

func agPR(tp, fp, fn int) (float64, float64) {
	var P, R float64
	if tp+fp > 0 {
		P = float64(tp) / float64(tp+fp)
	}
	if tp+fn > 0 {
		R = float64(tp) / float64(tp+fn)
	}
	return P, R
}

func agF1(P, R float64) float64 {
	if P+R == 0 {
		return 0
	}
	return 2 * P * R / (P + R)
}

// agGrid 网格搜（遍历顺序与 JS 版一致：V 外层、F 中层、D 内层，稳定排序保序）。
func agGrid(rows []agRow) []agPoint {
	Vs := []float64{0.5, 0.55, 0.6, 0.65, 0.7}
	Fs := []float64{0.10, 0.12, 0.14, 0.16, 0.18}
	Ds := []float64{0.1, 0.2, 0.3}
	all := make([]agPoint, 0, len(Vs)*len(Fs)*len(Ds))
	for _, V := range Vs {
		for _, F := range Fs {
			for _, D := range Ds {
				all = append(all, agEvalPoint(rows, V, F, D))
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].F1 > all[j].F1 })
	return all
}

// agCVFolds 分组交叉验证折数。310 片规模下每折约 62 片，测试集足够。
const agCVFolds = 5

// agGroupKFold 按「片」分组的 K 折交叉验证：选阈值与评估分离。
//
// 为什么需要：agGrid 在**全部** rows 上搜 75 组阈值再取 F1 最高，报的是 in-sample
// 上限——实测 best 与 live 逐字段相等（0.70/0.12/0.30，F1 0.784），正是被
// 「在同批数据上选阈值」污染的结果。本函数每折只在其余片上选阈值，在本折
// （模型没见过的片）上评估，汇总 out-of-fold 预测；该数字与冻结集口径可比。
//
// 片数不足 2 时返回 nil：宁可不报，也不报假数字。
func agGroupKFold(rows []agRow, k int) *agCVOut {
	byClip := map[string][]agRow{}
	for _, r := range rows {
		byClip[r.Clip] = append(byClip[r.Clip], r)
	}
	clips := make([]string, 0, len(byClip))
	for c := range byClip {
		clips = append(clips, c)
	}
	sort.Strings(clips) // 确定性：片名排序后轮转分折
	if k > len(clips) {
		k = len(clips)
	}
	if k < 2 {
		return nil
	}

	out := &agCVOut{Folds: k, PerFold: make([]agCVFold, 0, k)}
	var tp, fp, fn int
	for f := 0; f < k; f++ {
		var train, test []agRow
		nClip := 0
		for i, c := range clips {
			if i%k == f {
				test = append(test, byClip[c]...)
				nClip++
			} else {
				train = append(train, byClip[c]...)
			}
		}
		if len(train) == 0 || len(test) == 0 {
			continue
		}
		best := agGrid(train)[0] // 阈值只在训练折上选
		p := agEvalPoint(test, best.V, best.F, best.D)
		tp += p.TP
		fp += p.FP
		fn += p.FN
		out.Windows += len(test) // 评估窗数（TP+FP+FN 不含 TN，不能当窗数用）
		out.PerFold = append(out.PerFold, agCVFold{
			Fold: f + 1, Clips: nClip, Wins: len(test),
			Vis: best.V, Face: best.F, Det: best.D, F1: agRound3(p.F1),
		})
	}
	out.TP, out.FP, out.FN = tp, fp, fn
	P, R := agPR(tp, fp, fn)
	out.P, out.R, out.F1 = agRound3(P), agRound3(R), agRound3(agF1(P, R))
	return out
}

// agMergeSegs 把窗起始秒列表合并成段 [start,end)（秒），允许间隔 <= gapSec。
// gapSec=0 即严格相邻（unseal6_eval 旧口径）；gapSec=8 表示中间空一个窗仍算同一段。
func agMergeSegs(secs []int, gapSec int) [][2]int {
	sort.Ints(secs)
	segs := make([][2]int, 0, len(secs))
	for _, s0 := range secs {
		s, e := s0, s0+agWinSec
		if n := len(segs); n > 0 && s-segs[n-1][1] <= gapSec {
			if e > segs[n-1][1] {
				segs[n-1][1] = e
			}
			continue
		}
		segs = append(segs, [2]int{s, e})
	}
	return segs
}

// agSegEval 段级评估：把窗级预测/金标按 gapSec 合并成段，再贪心一对一配对。
//
// minOvSec > 0 → 判据为「重叠 ≥ minOvSec 秒即命中」（对段边界差异稳健）；
// minOvSec <= 0 → 判据为 IoU ≥ 0.5（unseal6_eval 旧口径）。
//
// ⚠️ 口径敏感性（见 docs/highlight-progress.md §27）：gap 与判据都会显著改变 F1 且非单调。
// **段级数字在口径固定前不可用于参数决策**；本函数只负责如实计算，不做任何推荐。
func agSegEval(rows []agRow, V, F, D float64, gapSec, minOvSec int) agSegOut {
	out := agSegOut{GapSec: gapSec, MinOvSec: minOvSec}
	byClip := map[string][]agRow{}
	for _, r := range rows {
		byClip[r.Clip] = append(byClip[r.Clip], r)
	}
	clips := make([]string, 0, len(byClip))
	for c := range byClip {
		clips = append(clips, c)
	}
	sort.Strings(clips)

	type agPair struct{ pi, gi, score int }
	for _, clip := range clips {
		var pw, gw []int
		for _, r := range byClip[clip] {
			if agPred(r, V, F, D) {
				pw = append(pw, r.Sec)
			}
			if r.goldDance() {
				gw = append(gw, r.Sec)
			}
		}
		pred := agMergeSegs(pw, gapSec)
		gold := agMergeSegs(gw, gapSec)
		out.PredSegs += len(pred)
		out.GoldSegs += len(gold)

		pairs := make([]agPair, 0, len(pred))
		for pi, p := range pred {
			for gi, g := range gold {
				ov := minInt(p[1], g[1]) - maxInt(p[0], g[0])
				if ov <= 0 {
					continue
				}
				if minOvSec > 0 {
					if ov >= minOvSec {
						pairs = append(pairs, agPair{pi, gi, ov})
					}
					continue
				}
				union := (p[1] - p[0]) + (g[1] - g[0]) - ov
				if ov*2 >= union { // ov/union >= 0.5
					pairs = append(pairs, agPair{pi, gi, ov})
				}
			}
		}
		sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].score > pairs[j].score })
		usedP, usedG := map[int]bool{}, map[int]bool{}
		for _, pr := range pairs {
			if usedP[pr.pi] || usedG[pr.gi] {
				continue
			}
			usedP[pr.pi], usedG[pr.gi] = true, true
		}
		out.TP += len(usedP)
		out.FP += len(pred) - len(usedP)
		out.FN += len(gold) - len(usedG)
	}
	P, R := agPR(out.TP, out.FP, out.FN)
	out.P, out.R, out.F1 = agRound3(P), agRound3(R), agRound3(agF1(P, R))
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func agFind(all []agPoint, V, F, D float64) *agPoint {
	for i := range all {
		if all[i].V == V && all[i].F == F && all[i].D == D {
			return &all[i]
		}
	}
	return nil
}

func agGestureStats(rows []agRow, best agPoint, liveCfg *agLiveGate, livePt *agPoint) *agGestureOut {
	gest := make([]agRow, 0, 128)
	for _, r := range rows {
		if r.Label == "gesture" {
			gest = append(gest, r)
		}
	}
	if len(gest) == 0 {
		return nil
	}
	countAt := func(V, F, D float64) int {
		n := 0
		for _, r := range gest {
			if agPred(r, V, F, D) {
				n++
			}
		}
		return n
	}
	nBest := countAt(best.V, best.F, best.D)
	out := &agGestureOut{
		Windows:          len(gest),
		PredDanceBest:    nBest,
		PredDanceBestPct: agRound1(100 * float64(nBest) / float64(len(gest))),
	}
	if liveCfg != nil && livePt != nil {
		n := countAt(liveCfg.VisMin, liveCfg.FaceMax, liveCfg.DetMin)
		pct := agRound1(100 * float64(n) / float64(len(gest)))
		out.PredDanceLive, out.PredDanceLivePct = &n, &pct
	}
	return out
}

func agLoadLiveGate(cfgPath string) *agLiveGate {
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil
	}
	var cfg struct {
		Builtin struct {
			HighlightPoseGate *agLiveGate `json:"highlight_pose_gate"`
		} `json:"builtin"`
	}
	if json.Unmarshal(b, &cfg) != nil || cfg.Builtin.HighlightPoseGate == nil {
		return nil
	}
	g := cfg.Builtin.HighlightPoseGate
	if !g.Enable {
		return nil
	}
	return g
}

// agRound3 / agRound1 对齐 JS +toFixed(3) / +toFixed(1) 的数值结果
// （toFixed 产生字符串再转数字，尾随 0 自动消失，如 0.790 → 0.79）。
func agRound3(x float64) float64 { return math.Round(x*1000) / 1000 }
func agRound1(x float64) float64 { return math.Round(x*10) / 10 }
