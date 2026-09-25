package main

// train 子命令：hleval 的 Go 训练入口 —— 用户 2026-09-24 拍板「之后训练一律用 Go」。
//
// 完整复刻 tools/train_highlight/train.py + ablate_norm.py 的语义：
//   1. 数据概览 + 每片归一化基线来源（主播历史 / 切片内冷启动）
//   2. baseline  切片内归一化 + 固定 0.8/0.2 th1.5（线上现状口径）
//   3. tuned     主播历史基线 z 分 + 全量随机搜索（train.py 的乐观口径）
//   4. model     逻辑回归 5 特征，留一切片训练（C=0.5, class_weight=balanced）
//   5. ablate    A/B 固定权重只换归一化；C/D 折内搜权重阈值（诚实口径）；E 折内逻辑回归
//   6. 阈值扫描  切片内归一化、仅运动量 z 分
//
// 与 Python 的两处已知差异（都是刻意的）：
//   - 随机搜索用 Go rand（种子固定 42/7），序列与 numpy 不同 → tuned/C/D 数字有 ±0.01 级抖动
//   - smooth 按 numpy.convolve(x, ones(w)/w, mode="same") 的零填充语义实现（与 train.py
//     一致，保证与历史 Python 日志直接可比）。线上 score.go:smooth 是边缘收缩窗口，
//     两者只差每片首尾 2 秒的边缘值，对聚合指标影响可忽略。
//
// 用法：
//
//	hleval train -csv <features.csv> [-search-n 600]

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
)

// modelFeats 与 train.py MODEL_FEATS 一致（逻辑回归特征）。
var modelFeats = []string{"v_YAVG", "v_YLOW", "v_YDIF", "v_VDIF", "a_RMS_level"}

// scoreFeats 打分只用到这两列。
var scoreFeats = []string{"v_YAVG", "a_RMS_level"}

type trClip struct {
	name     string
	streamer string
	scene    string
	y        []int
	col      map[string][]float64
}

func cmdTrain(args []string) {
	fs := flag.NewFlagSet("train", flag.ExitOnError)
	csvPath := fs.String("csv", "", "features CSV（build_features.py / hleval export 产出）")
	searchN := fs.Int("search-n", 600, "折内随机搜索次数")
	_ = fs.Parse(args)
	if *csvPath == "" {
		fmt.Fprintln(os.Stderr, "train: 需要 -csv")
		os.Exit(2)
	}

	clips, nRows, err := loadTrainCSV(*csvPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读 CSV:", err)
		os.Exit(1)
	}
	var clipNames []string
	for _, c := range clips {
		clipNames = append(clipNames, c.name)
	}

	// 预计算两套 z 分：主播历史基线（冷启动回退切片内）与切片内归一化
	zs := buildZScores(clips)
	zSlice := buildZSliceScore(clips)
	yAll := map[string][]int{}
	for _, c := range clips {
		yAll[c.name] = c.y
	}

	// ── 1. 数据概览 ──────────────────────────────────────────────────
	fmt.Printf("数据: %d 行 | %d 个切片 | 逻辑回归特征 %v\n", nRows, len(clips), modelFeats)
	fmt.Printf("%-38s %6s %7s %7s  %-10s 场景\n", "切片", "秒数", "正样本", "占比", "基线来源")
	for _, c := range clips {
		src := "切片内(冷启动)"
		if zs.src[c.name] == "hist" {
			src = "主播历史"
		}
		pos := 0
		for _, v := range c.y {
			pos += v
		}
		fmt.Printf("%-38s %6d %7d %6.1f%%  %-10s %s\n",
			trunc(c.name, 36), len(c.y), pos, float64(pos)/float64(len(c.y))*100, src, c.scene)
	}
	if len(clips) < 2 {
		fmt.Println("\n⚠️  只有 1 个切片，无法做留一切片交叉验证，以下数字会明显偏乐观。")
	}

	// ── 2. baseline：切片内归一化 + 固定权重（线上当前做法） ───────────
	{
		var ys, ps []int
		for _, c := range clips {
			zm := normSlicewisePy(c.col["v_YAVG"])
			za := normSlicewisePy(c.col["a_RMS_level"])
			for i := range zm {
				s := zm[i]*0.8 + za[i]*0.2
				ys = append(ys, c.y[i])
				if s > 1.5 {
					ps = append(ps, 1)
				} else {
					ps = append(ps, 0)
				}
			}
		}
		p, r, f1, _, _, _ := metricsPR(ys, ps)
		fmt.Printf("\nbaseline 切片内归一化 0.8/0.2 th1.5            P %.3f  R %.3f  F1 %.3f\n", p, r, f1)
	}

	// ── 3. tuned：主播历史基线 + 全量随机搜索（train.py 乐观口径） ────
	var modelP, modelR, modelF1 float64
	{
		rng := rand.New(rand.NewSource(42))
		zv, za, yy := concatScore(zs.z, yAll, clipNames, nil, nil)
		f1, mw, aw, th := randomSearch(zv, za, yy, rng, 400,
			[2]float64{0.2, 2.0}, [2]float64{-0.5, 0.5}, [2]float64{0.5, 4.0})
		p, r, _, _, _, _ := evalPoint(zv, za, yy, mw, aw, th)
		fmt.Printf("\ntuned 主播历史基线 %.2f/%+.2f th%.2f（全量搜索 400 次, Go rand seed 42）\n", mw, aw, th)
		fmt.Printf("  P %.3f  R %.3f  F1 %.3f\n", p, r, f1)
	}

	// ── 4. model：逻辑回归（留一切片训练） ────────────────────────────
	{
		var ys, ps []int
		var coefs [][]float64
		type pcRow struct {
			name     string
			p, r, f1 float64
		}
		var perClip []pcRow
	var skippedFolds []string
	for _, c := range clips {
		trainClips := without(clipNames, c.name)
		var Xtr [][]float64
		var ytr []int
		for _, x := range trainClips {
			Xtr = append(Xtr, featMatrix(zs.z[x], modelFeats)...)
			ytr = append(ytr, yAll[x]...)
		}
		if !twoClasses(ytr) {
			// 训练折单一类别 → 逻辑回归无法拟合，该折整体跳过。
			// 必须显式记录：被跳过的测试切片不进合计指标，静默会让聚合数字无声偏置。
			skippedFolds = append(skippedFolds, c.name)
			continue
		}
			w, b := fitLogreg(Xtr, ytr, 0.5, 100)
			coefs = append(coefs, w)
			Xte := featMatrix(zs.z[c.name], modelFeats)
			base := len(ys)
			for i := range Xte {
				z := b
				for j := range w {
					z += w[j] * Xte[i][j]
				}
				ys = append(ys, c.y[i])
				if sigmoid(z) > 0.5 {
					ps = append(ps, 1)
				} else {
					ps = append(ps, 0)
				}
			}
			p, r, f1, _, _, _ := metricsPR(c.y, ps[base:])
			perClip = append(perClip, pcRow{c.name, p, r, f1})
		}
		modelP, modelR, modelF1, _, _, _ = metricsPR(ys, ps)
		if len(coefs) > 0 {
			fmt.Println("\nmodel 逻辑回归 5 特征（留一训练, C=0.5, balanced）")
			fmt.Println("逻辑回归平均系数（主播级基线归一化，可直接读方向）:")
			for j, f := range modelFeats {
				var s float64
				for _, cw := range coefs {
					s += cw[j]
				}
				fmt.Printf("  %-14s %+.3f\n", f, s/float64(len(coefs)))
			}
			fmt.Println("逐切片（model）:")
			for _, x := range perClip {
				fmt.Printf("  %-38s P %.3f  R %.3f  F1 %.3f\n", trunc(x.name, 36), x.p, x.r, x.f1)
			}
			fmt.Printf("  合计 P %.3f  R %.3f  F1 %.3f\n", modelP, modelR, modelF1)
			if len(skippedFolds) > 0 {
				fmt.Printf("  ⚠️  %d 折因训练集单一类别被跳过（未计入合计，避免无声偏置）: %s\n",
					len(skippedFolds), strings.Join(skippedFolds, ", "))
			}
		}
	}

	// ── 5. ablate：归一化 2×2 + 折内搜索（诚实口径） ───────────────────
	fmt.Printf("\n━━━ ablate（固定 0.8/0.2 th1.5 只换归一化 + 折内搜 %d 次, Go rand seed 7）━━━\n", *searchN)
	{
		// A / B：固定权重只换归一化
		for _, tag := range []string{"A 切片内", "B 主播历史"} {
			zmap := zSlice
			if strings.HasPrefix(tag, "B") {
				zmap = pickScore(zs.z)
			}
			var ys, ps []int
			for _, c := range clips {
				zm := zmap[c.name]["v_YAVG"]
				za := zmap[c.name]["a_RMS_level"]
				for i := range zm {
					s := zm[i]*0.8 + za[i]*0.2
					ys = append(ys, c.y[i])
					if s > 1.5 {
						ps = append(ps, 1)
					} else {
						ps = append(ps, 0)
					}
				}
			}
			p, r, f1, _, _, _ := metricsPR(ys, ps)
			fmt.Printf("  %s 归一化 0.8/0.2 th1.5   P %.3f  R %.3f  F1 %.3f\n", tag, p, r, f1)
		}
		// C / D：折内搜权重阈值（诚实版）
		rng := rand.New(rand.NewSource(7))
		for _, tag := range []string{"C 切片内", "D 主播历史"} {
			zmap := zSlice
			if strings.HasPrefix(tag, "D") {
				zmap = pickScore(zs.z)
			}
			var ys, ps []int
			var mws, aws, ths []float64
			for _, c := range clips {
				trainClips := without(clipNames, c.name)
				zv, za, yy := concatScore(nil, yAll, nil, zmap, trainClips)
				_, mw, aw, th := randomSearch(zv, za, yy, rng, *searchN,
					[2]float64{0.2, 2.0}, [2]float64{-0.5, 0.5}, [2]float64{0.3, 4.0})
				mws = append(mws, mw)
				aws = append(aws, aw)
				ths = append(ths, th)
				zv2 := zmap[c.name]["v_YAVG"]
				za2 := zmap[c.name]["a_RMS_level"]
				for i := range zv2 {
					s := zv2[i]*mw + za2[i]*aw
					ys = append(ys, c.y[i])
					if s > th {
						ps = append(ps, 1)
					} else {
						ps = append(ps, 0)
					}
				}
			}
			p, r, f1, _, _, _ := metricsPR(ys, ps)
			fmt.Printf("  %s 归一化 折内搜         P %.3f  R %.3f  F1 %.3f\n", tag, p, r, f1)
			fmt.Printf("    折内选中超参均值: 运动 %.2f±%.2f  音频 %+.2f±%.2f  阈值 %.2f±%.2f\n",
				mean64(mws), std64(mws), mean64(aws), std64(aws), mean64(ths), std64(ths))
		}
		fmt.Printf("  E 逻辑回归（同 model 留一）     P %.3f  R %.3f  F1 %.3f\n", modelP, modelR, modelF1)
	}

	// ── 6. 阈值扫描（切片内归一化，仅运动量 z 分） ─────────────────────
	{
		var zv, yy []float64
		for _, c := range clips {
			z := normSlicewisePy(c.col["v_YAVG"])
			zv = append(zv, z...)
			for _, v := range c.y {
				yy = append(yy, float64(v))
			}
		}
		fmt.Println("\n阈值扫描（切片内归一化，仅运动量 z 分）—— 决定「要多全」还是「要多准」")
		fmt.Printf("  %-6s %7s %7s %7s %8s %8s\n", "阈值", "P", "R", "F1", "检出秒", "误检秒")
		for _, th := range []float64{0.8, 1.0, 1.2, 1.4, 1.5, 1.8, 2.0, 2.5, 3.0} {
			var tp, fp, fn int
			for i := range zv {
				pred := zv[i] > th
				switch {
				case yy[i] == 1 && pred:
					tp++
				case yy[i] == 0 && pred:
					fp++
				case yy[i] == 1 && !pred:
					fn++
				}
			}
			p := div(tp, tp+fp)
			r := div(tp, tp+fn)
			f1 := divF(2*p*r, p+r)
			fmt.Printf("  %-6.1f %7.3f %7.3f %7.3f %8d %8d\n", th, p, r, f1, tp, fp)
		}
	}
}

// ── 数据加载 ─────────────────────────────────────────────────────────

func loadTrainCSV(path string) ([]*trClip, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	rd := csv.NewReader(f)
	rd.LazyQuotes = true
	header, err := rd.Read()
	if err != nil {
		return nil, 0, err
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.TrimSpace(h)] = i
	}
	need := append([]string{"clip", "streamer", "scene", "sec", "label"}, modelFeats...)
	for _, n := range need {
		if _, ok := idx[n]; !ok {
			return nil, 0, fmt.Errorf("缺少列 %s", n)
		}
	}
	type rawRow struct {
		clip, streamer, scene string
		sec, label            int
		vals                  []float64
	}
	var rows []rawRow
	badRows := 0
	for {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// 解析失败的行静默丢弃会偏置聚合指标 —— 计数并在摘要里告警。
			badRows++
			if badRows <= 3 {
				fmt.Fprintln(os.Stderr, "⚠️  CSV 行解析失败（跳过）:", err)
			}
			continue
		}
		get := func(name string) string {
			i, ok := idx[name]
			if !ok || i >= len(rec) {
				return ""
			}
			return rec[i]
		}
		r := rawRow{clip: get("clip"), streamer: get("streamer"), scene: get("scene")}
		r.sec, _ = strconv.Atoi(get("sec"))
		r.label, _ = strconv.Atoi(get("label"))
		for _, f := range modelFeats {
			v, _ := strconv.ParseFloat(get(f), 64)
			r.vals = append(r.vals, v)
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].clip != rows[j].clip {
			return rows[i].clip < rows[j].clip
		}
		return rows[i].sec < rows[j].sec
	})
	var out []*trClip
	var cur *trClip
	for _, r := range rows {
		if cur == nil || cur.name != r.clip {
			cur = &trClip{name: r.clip, streamer: r.streamer, scene: r.scene, col: map[string][]float64{}}
			out = append(out, cur)
		}
		cur.y = append(cur.y, r.label)
		for j, f := range modelFeats {
			cur.col[f] = append(cur.col[f], r.vals[j])
		}
	}
	if badRows > 0 {
		fmt.Fprintf(os.Stderr, "⚠️  loadTrainCSV: %d 行解析失败被跳过（聚合指标不含这些行）\n", badRows)
	}
	return out, len(rows), nil
}

// medianF64 返回中位数（不修改入参），与 internal/highlight.median 同语义；
// 本地再实现一份是因为该函数未导出。
func medianF64(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	c := append([]float64(nil), x...)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

// stddevF64 总体标准差（MAD 塌陷时的兜底尺度），同 internal/highlight.stddev。
func stddevF64(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	m := mean64(x)
	var s float64
	for _, v := range x {
		d := v - m
		s += d * d
	}
	return math.Sqrt(s / float64(len(x)))
}

// divF 浮点安全除法（分母 0 返回 0）。
func divF(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// ── 归一化（numpy 语义） ─────────────────────────────────────────────

// smoothPy 与 numpy.convolve(x, ones(w)/w, mode="same") 同语义：
// 两端零填充、除以完整窗口宽（边缘值被衰减）。见文件头说明。
func smoothPy(x []float64, w int) []float64 {
	if w < 2 {
		return append([]float64(nil), x...)
	}
	half := w / 2
	out := make([]float64, len(x))
	for i := range x {
		lo, hi := i-half, i+half+1
		if lo < 0 {
			lo = 0
		}
		if hi > len(x) {
			hi = len(x)
		}
		var s float64
		for j := lo; j < hi; j++ {
			s += x[j]
		}
		out[i] = s / float64(w)
	}
	return out
}

// normSlicewisePy 切片内自适应归一化，与 train_common.norm_slicewise 同语义。
func normSlicewisePy(x []float64) []float64 {
	sm := smoothPy(x, 5)
	med := medianF64(sm)
	dev := make([]float64, len(sm))
	for i, v := range sm {
		dev[i] = math.Abs(v - med)
	}
	scale := 1.4826 * medianF64(dev)
	if scale < 1e-9 {
		scale = stddevF64(sm)
	}
	out := make([]float64, len(sm))
	if scale < 1e-9 {
		return out
	}
	for i, v := range sm {
		out[i] = (v - med) / scale
	}
	return out
}

// fitPriorPy 与 train_common.fit_prior 同语义：从参考序列拟合 (med, mad)。
func fitPriorPy(vals []float64) (float64, float64) {
	v := smoothPy(vals, 5)
	med := medianF64(v)
	dev := make([]float64, len(v))
	for i, x := range v {
		dev[i] = math.Abs(x - med)
	}
	mad := 1.4826 * medianF64(dev)
	if mad < 1e-9 {
		mad = stddevF64(v)
	}
	if mad < 1e-9 {
		mad = 1.0
	}
	return med, mad
}

func applyPriorPy(x []float64, med, mad float64) []float64 {
	sm := smoothPy(x, 5)
	out := make([]float64, len(sm))
	for i, v := range sm {
		out[i] = (v - med) / mad
	}
	return out
}

// ── z 分集合 ─────────────────────────────────────────────────────────

type zScores struct {
	z   map[string]map[string][]float64 // clip -> feat -> z
	y   map[string][]int
	src map[string]string // "hist" | "cold"
}

// buildZScores 主播历史基线：同主播其他切片拟合 (med,mad)；冷启动回退切片内。
// 与 train.py build_zscores 完全同语义（历史取自全量 df，含被留出的同主播切片）。
func buildZScores(clips []*trClip) *zScores {
	out := &zScores{z: map[string]map[string][]float64{}, y: map[string][]int{}, src: map[string]string{}}
	for _, c := range clips {
		out.y[c.name] = c.y
		var hist []*trClip
		for _, h := range clips {
			if h.name != c.name && h.streamer == c.streamer {
				hist = append(hist, h)
			}
		}
		m := map[string][]float64{}
		if len(hist) > 0 {
			out.src[c.name] = "hist"
			for _, f := range modelFeats {
				var ref []float64
				for _, h := range hist {
					ref = append(ref, h.col[f]...)
				}
				med, mad := fitPriorPy(ref)
				m[f] = applyPriorPy(c.col[f], med, mad)
			}
		} else {
			out.src[c.name] = "cold"
			for _, f := range modelFeats {
				m[f] = normSlicewisePy(c.col[f])
			}
		}
		out.z[c.name] = m
	}
	return out
}

// buildZSliceScore 每片切片内归一化的打分两列（ablate A/C 用）。
func buildZSliceScore(clips []*trClip) map[string]map[string][]float64 {
	out := map[string]map[string][]float64{}
	for _, c := range clips {
		m := map[string][]float64{}
		for _, f := range scoreFeats {
			m[f] = normSlicewisePy(c.col[f])
		}
		out[c.name] = m
	}
	return out
}

// pickScore 从 5 特征 z 集合里抽出打分两列（ablate B/D 用，避免改动原 map）。
func pickScore(z map[string]map[string][]float64) map[string]map[string][]float64 {
	out := map[string]map[string][]float64{}
	for c, m := range z {
		out[c] = map[string][]float64{"v_YAVG": m["v_YAVG"], "a_RMS_level": m["a_RMS_level"]}
	}
	return out
}

// concatScore 把若干切片的打分两列与标签拼成大数组。
// 全量：concatScore(zs.z, yAll, clipNames, nil, nil)；子集：concatScore(nil, yAll, nil, zmap, subset)。
func concatScore(z map[string]map[string][]float64, y map[string][]int, names []string, zmapSubset map[string]map[string][]float64, subset []string) (zv, za, yy []float64) {
	list := names
	zmap := z
	if zmapSubset != nil {
		zmap = zmapSubset
		list = subset
	}
	for _, n := range list {
		zv = append(zv, zmap[n]["v_YAVG"]...)
		za = append(za, zmap[n]["a_RMS_level"]...)
		for _, v := range y[n] {
			yy = append(yy, float64(v))
		}
	}
	return zv, za, yy
}

// ── 随机搜索与评估 ───────────────────────────────────────────────────

// randomSearch 在给定数组上随机搜 (mw, aw, th) 最大化秒级 F1。
func randomSearch(zv, za, yy []float64, rng *rand.Rand, n int, mwR, awR, thR [2]float64) (bestF1, mw, aw, th float64) {
	best := -1.0
	for i := 0; i < n; i++ {
		m := mwR[0] + (mwR[1]-mwR[0])*rng.Float64()
		a := awR[0] + (awR[1]-awR[0])*rng.Float64()
		t := thR[0] + (thR[1]-thR[0])*rng.Float64()
		_, _, f1, _, _, _ := evalPoint(zv, za, yy, m, a, t)
		if f1 > best {
			best = f1
			mw, aw, th = m, a, t
		}
	}
	return best, mw, aw, th
}

func evalPoint(zv, za, yy []float64, mw, aw, th float64) (p, r, f1 float64, tp, fp, fn int) {
	for i := range zv {
		s := zv[i]*mw + za[i]*aw
		pred := s > th
		switch {
		case yy[i] == 1 && pred:
			tp++
		case yy[i] == 0 && pred:
			fp++
		case yy[i] == 1 && !pred:
			fn++
		}
	}
	p = div(tp, tp+fp)
	r = div(tp, tp+fn)
	f1 = divF(2*p*r, p+r)
	return p, r, f1, tp, fp, fn
}

// metricsPR 与 train_common.metrics 同语义（int 版）。
func metricsPR(y, p []int) (pp, rr, f1 float64, tp, fp, fn int) {
	for i := range y {
		switch {
		case y[i] == 1 && p[i] == 1:
			tp++
		case y[i] == 0 && p[i] == 1:
			fp++
		case y[i] == 1 && p[i] == 0:
			fn++
		}
	}
	pp = div(tp, tp+fp)
	rr = div(tp, tp+fn)
	f1 = divF(2*pp*rr, pp+rr)
	return pp, rr, f1, tp, fp, fn
}

// ── 逻辑回归（Newton-Raphson / IRLS） ────────────────────────────────

// fitLogreg 拟合 L2 正则逻辑回归，目标与 sklearn.LogisticRegression
// (C=c, class_weight="balanced") 一致：0.5·||w||² + c·Σ cwᵢ·loglossᵢ，截距不正则。
// 与 sklearn 的 lbfgs 收敛到同一凸最优解（系数差 ~1e-4 求解器容差级）。
func fitLogreg(X [][]float64, y []int, c float64, maxIter int) (w []float64, b float64) {
	n := len(y)
	d := len(X[0])
	nPos, nNeg := 0, 0
	for _, v := range y {
		if v == 1 {
			nPos++
		} else {
			nNeg++
		}
	}
	cwPos := float64(n) / (2 * float64(nPos))
	cwNeg := float64(n) / (2 * float64(nNeg))

	theta := make([]float64, d+1) // [w..., b]
	for it := 0; it < maxIter; it++ {
		g := make([]float64, d+1)
		H := make([][]float64, d+1)
		for j := range H {
			H[j] = make([]float64, d+1)
		}
		for i := range y {
			z := theta[d]
			for j := 0; j < d; j++ {
				z += theta[j] * X[i][j]
			}
			p := sigmoid(z)
			cw := cwNeg
			if y[i] == 1 {
				cw = cwPos
			}
			cf := c * cw * (p - float64(y[i]))
			for j := 0; j < d; j++ {
				g[j] += cf * X[i][j]
			}
			g[d] += cf
			di := c * cw * p * (1 - p)
			for j := 0; j <= d; j++ {
				xj := 1.0
				if j < d {
					xj = X[i][j]
				}
				for k := j; k <= d; k++ {
					xk := 1.0
					if k < d {
						xk = X[i][k]
					}
					H[j][k] += di * xj * xk
				}
			}
		}
		for j := 0; j < d; j++ {
			g[j] += theta[j] // L2 梯度（截距不正则）
			H[j][j] += 1
		}
		for j := 0; j <= d; j++ {
			for k := j + 1; k <= d; k++ {
				H[k][j] = H[j][k]
			}
		}
		H[d][d] += 1e-10 // 截距行防奇异

		step := solveSym(H, g) // 解 H·step = g，theta -= step
		maxDelta := 0.0
		for j := range theta {
			theta[j] -= step[j]
			if math.Abs(step[j]) > maxDelta {
				maxDelta = math.Abs(step[j])
			}
		}
		if maxDelta < 1e-8 {
			break
		}
	}
	return theta[:d], theta[d]
}

// solveSym 高斯消元（部分主元）解 A·x = b。
func solveSym(a [][]float64, b []float64) []float64 {
	n := len(b)
	m := make([][]float64, n)
	for i := range m {
		m[i] = append(append([]float64(nil), a[i]...), b[i])
	}
	for col := 0; col < n; col++ {
		piv := col
		for r := col + 1; r < n; r++ {
			if math.Abs(m[r][col]) > math.Abs(m[piv][col]) {
				piv = r
			}
		}
		m[col], m[piv] = m[piv], m[col]
		pv := m[col][col]
		if math.Abs(pv) < 1e-12 {
			pv = 1e-12
			m[col][col] = pv
		}
		for r := col + 1; r < n; r++ {
			fc := m[r][col] / pv
			if fc == 0 {
				continue
			}
			for k := col; k <= n; k++ {
				m[r][k] -= fc * m[col][k]
			}
		}
	}
	x := make([]float64, n)
	for r := n - 1; r >= 0; r-- {
		s := m[r][n]
		for k := r + 1; k < n; k++ {
			s -= m[r][k] * x[k]
		}
		x[r] = s / m[r][r]
	}
	return x
}

func sigmoid(z float64) float64 { return 1 / (1 + math.Exp(-z)) }

// ── 小工具 ───────────────────────────────────────────────────────────

func featMatrix(z map[string][]float64, feats []string) [][]float64 {
	n := 0
	if len(feats) > 0 {
		n = len(z[feats[0]])
	}
	out := make([][]float64, n)
	for i := 0; i < n; i++ {
		row := make([]float64, len(feats))
		for j, f := range feats {
			row[j] = z[f][i]
		}
		out[i] = row
	}
	return out
}

func without(all []string, drop string) []string {
	var out []string
	for _, s := range all {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

func twoClasses(y []int) bool {
	var seen [2]bool
	for _, v := range y {
		seen[v] = true
	}
	return seen[0] && seen[1]
}

func div(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func mean64(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	var s float64
	for _, v := range x {
		s += v
	}
	return s / float64(len(x))
}

func std64(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	m := mean64(x)
	var s float64
	for _, v := range x {
		s += (v - m) * (v - m)
	}
	return math.Sqrt(s / float64(len(x)))
}
