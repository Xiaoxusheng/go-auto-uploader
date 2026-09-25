package main

// metrics：在特征 CSV 上评估固定打分 + Select 后处理（与线上 internal/highlight 同语义）。
// 这是 Python select_seg_search / eval_frozen 的 Go 权威实现；以后调参以本命令为准。

import (
	"encoding/json"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"upload/internal/highlight"
)

type clipRow struct {
	clip   string
	scene  string
	sec    int
	label  int
	hard   int
	motion float64
	audio  float64
	blocks [9]float64
	hasB   bool
}

func cmdMetrics(args []string) {
	fs := flag.NewFlagSet("metrics", flag.ExitOnError)
	csvPath := fs.String("csv", "", "features CSV（hleval export 产出）")
	mw := fs.Float64("mw", 1.0, "运动权重")
	aw := fs.Float64("aw", 0.0, "音频权重")
	th := fs.Float64("th", 1.2, "阈值")
	hyst := fs.Float64("hyst", 0, "迟滞退出比 ExitRatio（0=不用；推荐 0.8）")
	ml := fs.Int("ml", 8, "MinDuration 秒")
	gap := fs.Int("gap", 12, "MergeGap 秒（严格小于才合并）")
	pad := fs.Int("pad", 0, "Pad 秒")
	smooth := fs.Int("smooth", 5, "滑动平均窗口")
	hnLambda := fs.Float64("hn-lambda", 0, "hard_neg penalty weight")
	minBStd := fs.Float64("minbstd", 0, "段级 bstd 空间门槛（0=关；压礼物/近景伪运动）")
	minAC1 := fs.Float64("minac1", 0, "段级运动量 lag-1 自相关门槛（0=关；压礼物爆发）")
	minClose := fs.Float64("minclose", 0, "段级中心集中度门槛（0=关；需 b0..b8）")
	poseFile := fs.String("pose", "", "pose-scan 产出的每秒姿态特征 JSON（提供则启用姿态门）")
	poseVis := fs.Float64("pose-vis", 0.6, "姿态门：关键点置信度均值下限")
	poseFace := fs.Float64("pose-face", 0.14, "姿态门：双眼间距/帧宽上限")
	poseExtLo := fs.Float64("pose-extlo", 0.5, "姿态门：纵向跨度下限（占帧高）")
	poseExtHi := fs.Float64("pose-exthi", 1.0, "姿态门：纵向跨度上限（占帧高）")
	poseKeep := fs.Float64("pose-keep", 0.5, "姿态门：段内通过秒占比下限")
	poseDetMin := fs.Float64("pose-detmin", 0.15, "姿态门：段内姿态检出率下限（低于=无人/特效场，拒）")
	poseSmooth := fs.Int("pose-smooth", 3, "姿态特征时序平滑窗口（帧）")
	poseMode := fs.String("pose-mode", "seg", "门判定粒度：seg=段级均值聚合（与模拟同构）；sec=逐秒投票")
	search := fs.Bool("search", false, "grid search th/ml/gap")
	perClip := fs.Bool("per-clip", true, "打印逐切片指标")
	_ = fs.Parse(args)
	if *search && *csvPath != "" {
		rows, err := loadFeaturesCSV(*csvPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read csv:", err)
			os.Exit(1)
		}
		searchWorkPoints(rows, *hnLambda)
		return
	}
	if *csvPath == "" {
		fmt.Fprintln(os.Stderr, "metrics: 需要 -csv")
		os.Exit(2)
	}

	clips, err := loadFeaturesCSV(*csvPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读 CSV:", err)
		os.Exit(1)
	}

	by := map[string][]clipRow{}
	var names []string
	for _, r := range clips {
		if _, ok := by[r.clip]; !ok {
			names = append(names, r.clip)
		}
		by[r.clip] = append(by[r.clip], r)
	}
	sort.Strings(names)

	o := highlight.Options{
		MotionWeight: *mw,
		AudioWeight:  *aw,
		Threshold:    *th,
		ExitRatio:    *hyst,
		MinDuration:  *ml,
		MaxDuration:  0,
		MaxPerClip:   0,
		MergeGap:     *gap,
		SmoothWindow: *smooth,
		Pad:          *pad,
		MinBStd:      *minBStd,
		MinAC1:       *minAC1,
		MinClose:     *minClose,
	}

	var poseData map[string]struct {
		FPS   int          `json:"fps"`
		Feats [][5]float64 `json:"feats"`
	}
	if *poseFile != "" {
		b, perr := os.ReadFile(*poseFile)
		if perr != nil {
			fmt.Fprintln(os.Stderr, "读姿态特征失败:", perr)
			os.Exit(1)
		}
		if jerr := json.Unmarshal(b, &poseData); jerr != nil {
			fmt.Fprintln(os.Stderr, "解析姿态特征失败:", jerr)
			os.Exit(1)
		}
		fmt.Printf("姿态门: 已加载 %d 片每秒特征（vis≥%.2f face≤%.2f ext %.2f-%.2f keep≥%.2f）\n",
			len(poseData), *poseVis, *poseFace, *poseExtLo, *poseExtHi, *poseKeep)
	}

	var yAll, pAll []int
	var segGT, segPred, segHit int
	var hnN, hnFP int
	sceneY := map[string][]int{}
	sceneP := map[string][]int{}

	fmt.Printf("配置 mw=%.2f aw=%.2f th=%.2f hyst=%.2f ml=%d gap=%d pad=%d smooth=%d | %d 切片\n",
		*mw, *aw, *th, *hyst, *ml, *gap, *pad, *smooth, len(names))

	for _, name := range names {
		rows := by[name]
		sort.Slice(rows, func(i, j int) bool { return rows[i].sec < rows[j].sec })
		motion := make([]float64, len(rows))
		audio := make([]float64, len(rows))
		y := make([]int, len(rows))
		hn := make([]int, len(rows))
		var blocks highlight.Blocks
		hasB := len(rows) > 0
		for _, r := range rows {
			if !r.hasB {
				hasB = false
				break
			}
		}
		for i, r := range rows {
			motion[i] = r.motion
			audio[i] = r.audio
			y[i] = r.label
			hn[i] = r.hard
		}
		if hasB {
			blocks = make(highlight.Blocks, len(rows))
			for i, r := range rows {
				row := make([]float64, 9)
				copy(row, r.blocks[:])
				blocks[i] = row
			}
		}
		ser := &highlight.Series{Motion: motion, Audio: audio}
		scores := highlight.Score(ser, o)
		segs := highlight.SelectWithBlocks(scores, motion, blocks, o)
		// 姿态语义门 v3（§21/§22）：
		//   seg 模式（默认，与 Python 模拟同构）：段内检出秒的平滑特征取均值，
		//     一次门槛判定整段（det_rate < DetMin → 拒）；
		//   sec 模式（旧 v2）：逐秒判定 + 通过占比投票。
		if pf, ok := poseData[name]; ok {
			feats := pf.Feats
			nf := len(feats)
			sm := func(ch int) []float64 {
				out := make([]float64, nf)
				half := *poseSmooth / 2
				for t := 0; t < nf; t++ {
					lo, hi := t-half, t+half+1
					if lo < 0 {
						lo = 0
					}
					if hi > nf {
						hi = nf
					}
					sum, cnt := 0.0, 0
					for k := lo; k < hi; k++ {
						if feats[k][4] == 1 {
							sum += feats[k][ch]
							cnt++
						}
					}
					if cnt == 0 {
						out[t] = feats[t][ch]
						continue
					}
					out[t] = sum / float64(cnt)
				}
				return out
			}
			smVis, smFace, smExt := sm(0), sm(1), sm(2)
			kept := segs[:0]
			for _, sg := range segs {
				dur := sg.End - sg.Start
				det, sumV, sumF, sumE := 0, 0.0, 0.0, 0.0
				for t := sg.Start; t < sg.End && t < nf; t++ {
					if feats[t][4] != 1 {
						continue
					}
					det++
					sumV += smVis[t]
					sumF += smFace[t]
					sumE += smExt[t]
				}
				detRate := float64(det) / float64(dur)
				if detRate < *poseDetMin {
					continue // 无人/特效场 → 拒
				}
				keep := true
				if *poseMode == "seg" {
					mV, mF, mE := sumV/float64(det), sumF/float64(det), sumE/float64(det)
					keep = mV >= *poseVis && mF <= *poseFace && mE >= *poseExtLo && mE <= *poseExtHi
				} else {
					pass := 0
					for t := sg.Start; t < sg.End && t < nf; t++ {
						if feats[t][4] != 1 {
							continue
						}
						if smVis[t] >= *poseVis && smFace[t] <= *poseFace &&
							smExt[t] >= *poseExtLo && smExt[t] <= *poseExtHi {
							pass++
						}
					}
					keep = det > 0 && float64(pass)/float64(det) >= *poseKeep
				}
				if keep {
					kept = append(kept, sg)
				}
			}
			dropped := len(segs) - len(kept)
			segs = kept
			if dropped > 0 {
				fmt.Printf("  [姿态门-%s] %s: 砍 %d 段\n", *poseMode, trunc(name, 40), dropped)
			}
		}
		pred := highlight.SegmentsToMask(segs, len(rows))

		sec := highlight.SecondPRF(y, pred)
		seg := highlight.SegmentPRF(y, pred, 0.5, 1)
		segGT += seg.GT
		segPred += seg.Pred
		segHit += seg.Hit
		for i := range y {
			if hn[i] == 1 {
				hnN++
				if pred[i] == 1 {
					hnFP++
				}
			}
		}
		yAll = append(yAll, y...)
		pAll = append(pAll, pred...)
		sc := "other"
		if len(rows) > 0 {
			sc = rows[0].scene
		}
		sceneY[sc] = append(sceneY[sc], y...)
		sceneP[sc] = append(sceneP[sc], pred...)

		if *perClip {
			fmt.Printf("  %-48s secP %.3f R %.3f F1 %.3f  segF1 %.3f (hit %d/%d pred %d)\n",
				trunc(name, 48), sec.P, sec.R, sec.F1, seg.F1, seg.Hit, seg.GT, seg.Pred)
		}
	}

	sec := highlight.SecondPRF(yAll, pAll)
	var seg highlight.SegPR
	seg.GT, seg.Pred, seg.Hit = segGT, segPred, segHit
	if segPred > 0 {
		seg.P = float64(segHit) / float64(segPred)
	}
	if segGT > 0 {
		seg.R = float64(segHit) / float64(segGT)
	}
	if seg.P+seg.R > 0 {
		seg.F1 = 2 * seg.P * seg.R / (seg.P + seg.R)
	}

	fmt.Printf("\n合计 秒级 P %.3f R %.3f F1 %.3f  (TP %d FP %d FN %d)\n",
		sec.P, sec.R, sec.F1, sec.TP, sec.FP, sec.FN)
	fmt.Printf("    段级 IoU@0.5 P %.3f R %.3f F1 %.3f  (hit %d / gt %d / pred %d)\n",
		seg.P, seg.R, seg.F1, segHit, segGT, segPred)
	if hnN > 0 {
		fmt.Printf("    hard_neg 误检 %d/%d (%.1f%%)\n", hnFP, hnN, 100*float64(hnFP)/float64(hnN))
	}
	for _, sc := range sortedKeys(sceneY) {
		m := highlight.SecondPRF(sceneY[sc], sceneP[sc])
		fmt.Printf("    scene[%s] F1 %.3f (P %.3f R %.3f)\n", sc, m.F1, m.P, m.R)
	}
}

func sortedKeys(m map[string][]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func loadFeaturesCSV(path string) ([]clipRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rd := csv.NewReader(f)
	rd.LazyQuotes = true
	header, err := rd.Read()
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.TrimSpace(h)] = i
	}
	for _, need := range []string{"clip", "label", "v_YAVG", "a_RMS_level"} {
		if _, ok := idx[need]; !ok {
			return nil, fmt.Errorf("缺少列 %s", need)
		}
	}
	var out []clipRow
	for {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		get := func(name string) string {
			i, ok := idx[name]
			if !ok || i >= len(rec) {
				return ""
			}
			return rec[i]
		}
		motion, _ := strconv.ParseFloat(get("v_YAVG"), 64)
		audio, _ := strconv.ParseFloat(get("a_RMS_level"), 64)
		label, _ := strconv.Atoi(get("label"))
		hard, _ := strconv.Atoi(get("hard_neg"))
		sec, _ := strconv.Atoi(get("sec"))
		scene := get("scene")
		if scene == "" {
			scene = "unknown"
		}
		var blocks [9]float64
		hasB := true
		for k := 0; k < 9; k++ {
			raw := get(fmt.Sprintf("b%d", k))
			if raw == "" {
				hasB = false
				break
			}
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				hasB = false
				break
			}
			blocks[k] = v
		}
		out = append(out, clipRow{
			clip:   get("clip"),
			scene:  scene,
			sec:    sec,
			label:  label,
			hard:   hard,
			motion: motion,
			audio:  audio,
			blocks: blocks,
			hasB:   hasB,
		})
	}
	return out, nil
}


func searchWorkPoints(rows []clipRow, hnLambda float64) {
	by := map[string][]clipRow{}
	var names []string
	for _, r := range rows {
		if _, ok := by[r.clip]; !ok {
			names = append(names, r.clip)
		}
		by[r.clip] = append(by[r.clip], r)
	}
	sort.Strings(names)
	type scoreT struct {
		obj, f1, hn, th, ml, gap, hyst float64
	}
	var best []scoreT
	for _, th := range []float64{1.0, 1.2, 1.4, 1.6, 1.8} {
		for _, ml := range []int{8, 12, 15, 20} {
			for _, gap := range []int{5, 10, 12, 20} {
				for _, hy := range []float64{0, 0.8} {
					o := highlight.Options{MotionWeight: 1, AudioWeight: 0, Threshold: th,
						ExitRatio: hy, MinDuration: ml, MaxDuration: 0, MaxPerClip: 0,
						MergeGap: gap, SmoothWindow: 5, Pad: 0}
					var yAll, pAll []int
					var hnN, hnFP int
					for _, name := range names {
						rr := by[name]
						sort.Slice(rr, func(i, j int) bool { return rr[i].sec < rr[j].sec })
						motion := make([]float64, len(rr))
						audio := make([]float64, len(rr))
						y := make([]int, len(rr))
						hn := make([]int, len(rr))
						var blocks highlight.Blocks
						hasB := len(rr) > 0
						for _, r := range rr {
							if !r.hasB {
								hasB = false
								break
							}
						}
						for i, r := range rr {
							motion[i], audio[i], y[i], hn[i] = r.motion, r.audio, r.label, r.hard
						}
						if hasB {
							blocks = make(highlight.Blocks, len(rr))
							for i, r := range rr {
								row := make([]float64, 9)
								copy(row, r.blocks[:])
								blocks[i] = row
							}
						}
						scores := highlight.Score(&highlight.Series{Motion: motion, Audio: audio}, o)
						pred := highlight.SegmentsToMask(highlight.SelectWithBlocks(scores, motion, blocks, o), len(rr))
						yAll = append(yAll, y...)
						pAll = append(pAll, pred...)
						for i := range y {
							if hn[i] == 1 {
								hnN++
								if pred[i] == 1 {
									hnFP++
								}
							}
						}
					}
					var tp, fp, fn int
					for i := range yAll {
						t, p := yAll[i] != 0, pAll[i] != 0
						if t && p {
							tp++
						} else if !t && p {
							fp++
						} else if t && !p {
							fn++
						}
					}
					f1 := 0.0
					if tp+fp > 0 && tp+fn > 0 {
						pr := float64(tp) / float64(tp+fp)
						rc := float64(tp) / float64(tp+fn)
						if pr+rc > 0 {
							f1 = 2 * pr * rc / (pr + rc)
						}
					}
					hnRate := 0.0
					if hnN > 0 {
						hnRate = float64(hnFP) / float64(hnN)
					}
					best = append(best, scoreT{obj: f1 - hnLambda*hnRate, f1: f1, hn: hnRate, th: th, ml: float64(ml), gap: float64(gap), hyst: hy})
				}
			}
		}
	}
	sort.Slice(best, func(i, j int) bool { return best[i].obj > best[j].obj })
	fmt.Println("=== search F1 - lambda*hnFP top 8 ===")
	for i, b := range best {
		if i >= 8 {
			break
		}
		fmt.Printf("  th=%.1f ml=%.0f gap=%.0f hyst=%.1f  obj=%.3f F1=%.3f hn=%.1f%%\n",
			b.th, b.ml, b.gap, b.hyst, b.obj, b.f1, b.hn*100)
	}
}
