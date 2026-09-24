package main

// metrics：在特征 CSV 上评估固定打分 + Select 后处理（与线上 internal/highlight 同语义）。
// 这是 Python select_seg_search / eval_frozen 的 Go 权威实现；以后调参以本命令为准。

import (
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
	perClip := fs.Bool("per-clip", true, "打印逐切片指标")
	_ = fs.Parse(args)
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
		for i, r := range rows {
			motion[i] = r.motion
			audio[i] = r.audio
			y[i] = r.label
			hn[i] = r.hard
		}
		ser := &highlight.Series{Motion: motion, Audio: audio}
		scores := highlight.Score(ser, o)
		segs := highlight.Select(scores, o)
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
		out = append(out, clipRow{
			clip:   get("clip"),
			scene:  scene,
			sec:    sec,
			label:  label,
			hard:   hard,
			motion: motion,
			audio:  audio,
		})
	}
	return out, nil
}
