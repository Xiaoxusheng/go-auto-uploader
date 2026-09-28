package main

// batch-probe：批量给目录下视频提特征到 cache（替代 batch_probe_downloads.py）。
// de-auc：在 grid.csv + D/E 金标上算空间/时序特征 AUC（与 internal/highlight 同语义）。

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"upload/internal/highlight"
)

func cmdBatchProbe(args []string) {
	fs := flag.NewFlagSet("batch-probe", flag.ExitOnError)
	dir := fs.String("dir", "", "视频根目录（递归 *.ts/*.mp4）")
	cacheDir := fs.String("cache-dir", "", "feat.json 输出目录")
	ffmpegBin := fs.String("ffmpeg", "ffmpeg", "ffmpeg 路径")
	threads := fs.Int("threads", 2, "解码线程")
	limit := fs.Int("limit", 0, "最多处理 N 个（0=全部）")
	perStreamer := fs.Int("per-streamer", 0, "每个一级子目录最多 N 个（0=不限）")
	force := fs.Bool("force", false, "已有缓存也重跑")
	_ = fs.Parse(args)
	if *dir == "" || *cacheDir == "" {
		fmt.Fprintln(os.Stderr, "batch-probe 需要 -dir 与 -cache-dir")
		os.Exit(2)
	}
	if err := os.MkdirAll(*cacheDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var files []string
	exts := map[string]bool{".ts": true, ".mp4": true}
	_ = filepath.Walk(*dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(p, "高光") {
			return nil
		}
		if exts[strings.ToLower(filepath.Ext(p))] {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)

	byStreamer := map[string]int{}
	ok, skip, fail := 0, 0, 0
	done := 0
	for _, src := range files {
		if *limit > 0 && done >= *limit {
			break
		}
		rel, _ := filepath.Rel(*dir, src)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		streamer := ""
		if len(parts) > 1 {
			streamer = parts[0]
		}
		if *perStreamer > 0 && byStreamer[streamer] >= *perStreamer {
			continue
		}
		cache := filepath.Join(*cacheDir, strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))+".feat.json")
		if _, err := os.Stat(cache); err == nil && !*force {
			skip++
			byStreamer[streamer]++
			done++
			continue
		}
		fmt.Printf("[%d] probe %s\n", done+1, filepath.Base(src))
		if err := runProbeOne(src, cache, *ffmpegBin, *threads, *force); err != nil {
			fail++
			fmt.Printf("    FAIL %v\n", err)
		} else {
			ok++
			fmt.Printf("    ok → %s\n", filepath.Base(cache))
		}
		byStreamer[streamer]++
		done++
	}
	fmt.Printf("done ok=%d skip=%d fail=%d\n", ok, skip, fail)
}

type deRow struct {
	clip  string
	start int
	end   int
	lab   int // 1=dance, 0=D/E
	kind  string
}

func cmdDeAUC(args []string) {
	fs := flag.NewFlagSet("de-auc", flag.ExitOnError)
	labels := fs.String("labels", "", "grid_de_labels.jsonl 或 labels.jsonl")
	gridDir := fs.String("grid-dir", "D:/upload/downloads", "*.grid.csv 所在根")
	_ = fs.Parse(args)
	if *labels == "" {
		fmt.Fprintln(os.Stderr, "de-auc 需要 -labels")
		os.Exit(2)
	}

	rows := loadDeLabels(*labels)
	gridIndex := indexGrids(*gridDir)
	type sample struct {
		row deRow
		f   map[string]float64
	}
	var ss []sample
	for _, r := range rows {
		g := lookupGrid(gridIndex, r.clip)
		if g == nil {
			continue
		}
		f, ok := windowStats(g, r.start, r.end)
		if !ok {
			continue
		}
		ss = append(ss, sample{r, f})
	}
	fmt.Printf("windows %d\n", len(ss))
	if len(ss) == 0 {
		return
	}

	reportAUC := func(title string, keep func(deRow) bool) {
		var scores []float64
		var y []int
		var names = []string{"ac1", "bstd", "bstd_p90", "center", "full_mean"}
		count := 0
		for _, s := range ss {
			if keep(s.row) {
				count++
			}
		}
		fmt.Printf("\n=== %s n=%d ===\n", title, count)
		for _, name := range names {
			scores, y = scores[:0], y[:0]
			for _, s := range ss {
				if !keep(s.row) {
					continue
				}
				scores = append(scores, s.f[name])
				y = append(y, s.row.lab)
			}
			a := highlight.MannWhitneyAUC(scores, y)
			fmt.Printf("  %-12s AUC %.3f\n", name, a)
		}
	}

	reportAUC("dance vs all D/E", func(r deRow) bool { return true })
	reportAUC("dance vs gift", func(r deRow) bool { return r.lab == 1 || r.kind == "gift" })
	reportAUC("dance vs closeup", func(r deRow) bool { return r.lab == 1 || r.kind == "closeup" })
}

type gridTable struct {
	secs   int
	full   []float64
	blocks highlight.Blocks
}

// indexGrids 一次扫盘建 stem→表；lookup 按 clip 模糊命中。
func indexGrids(root string) map[string]*gridTable {
	out := map[string]*gridTable{}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".grid.csv") {
			return nil
		}
		stem := strings.TrimSuffix(info.Name(), ".grid.csv")
		if g := readGrid(p); g != nil && g.secs > 0 {
			out[stem] = g
		}
		return nil
	})
	return out
}

func lookupGrid(idx map[string]*gridTable, clip string) *gridTable {
	if g, ok := idx[clip]; ok {
		return g
	}
	for stem, g := range idx {
		if strings.Contains(clip, stem) || strings.Contains(stem, clip) {
			return g
		}
		if len(stem) >= 20 && strings.Contains(clip, stem[len(stem)-20:]) {
			return g
		}
	}
	return nil
}

func findGrid(root, clip string) *gridTable {
	return lookupGrid(indexGrids(root), clip)
}

func readGrid(path string) *gridTable {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	rd := csv.NewReader(f)
	header, err := rd.Read()
	if err != nil {
		return nil
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.TrimSpace(h)] = i
	}
	get := func(rec []string, name string) (float64, bool) {
		i, ok := idx[name]
		if !ok || i >= len(rec) {
			return 0, false
		}
		var v float64
		if _, err := fmt.Sscanf(rec[i], "%g", &v); err != nil {
			return 0, false
		}
		return v, true
	}
	var g gridTable
	for {
		rec, err := rd.Read()
		if err != nil {
			break
		}
		full, _ := get(rec, "full")
		row := make([]float64, 9)
		ok := true
		for k := 0; k < 9; k++ {
			v, good := get(rec, fmt.Sprintf("b%d", k))
			if !good {
				ok = false
				break
			}
			row[k] = v
		}
		if !ok {
			continue
		}
		g.full = append(g.full, full)
		g.blocks = append(g.blocks, row)
	}
	g.secs = len(g.full)
	return &g
}

func windowStats(g *gridTable, s, e int) (map[string]float64, bool) {
	if g == nil || e > g.secs || e-s < 3 {
		return nil, false
	}
	motion := g.full[s:e]
	b := g.blocks[s:e]
	return map[string]float64{
		"ac1":      highlight.WindowAC1(g.full, s, e),
		"bstd":     highlight.WindowBStd(b, 0, len(b)),
		"bstd_p90": highlight.WindowBStdP90(b, 0, len(b)),
		"center":   highlight.WindowCenterRatio(b, 0, len(b)),
		"full_mean": func() float64 {
			sum := 0.0
			for _, v := range motion {
				sum += v
			}
			return sum / float64(len(motion))
		}(),
	}, true
}

func loadDeLabels(path string) []deRow {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var out []deRow
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var o struct {
			Clip  string `json:"clip"`
			Label string `json:"label"`
			Start int    `json:"start"`
			End   int    `json:"end"`
		}
		if err := json.Unmarshal([]byte(line), &o); err != nil {
			continue
		}
		lab := 0
		if o.Label == "dance" {
			lab = 1
		}
		if o.Label != "dance" && o.Label != "gift" && o.Label != "closeup" && o.Label != "scene" {
			continue
		}
		out = append(out, deRow{clip: o.Clip, start: o.Start, end: o.End, lab: lab, kind: o.Label})
	}
	return out
}
