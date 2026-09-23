// Command hleval 是高光切片离线训练与评估的工具集。
//
// 刻意做成独立二进制，不参与主程序构建：
// 它要跑 ffmpeg 全量解码（分钟级），只适合离线批量使用，绝不能进线上链路。
//
// 分工：Go 侧只做「重」的事（解码 + 特征提取 + 导出），
// 指标计算、超参搜索、模型训练都在 Python 侧（tools/train_highlight/），
// 因为那部分需要频繁改算法，用 Python 迭代成本远低于重新编译。
//
// 用法：
//
//	hleval probe  -src <视频> [-cache <json>] [-ffmpeg <path>] [-force]
//	hleval export -labels <标注.jsonl> -cache-dir <目录> -out <csv>
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"upload/internal/highlight"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "probe":
		cmdProbe(os.Args[2:])
	case "export":
		cmdExport(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", os.Args[1])
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `hleval — 高光切片离线训练工具

子命令:
  probe   对视频跑全字段特征提取并写缓存
  export  把标注 + 特征缓存导出成训练用 CSV

probe 参数:
  -src <视频>        必填
  -cache <json>      缓存输出路径，默认 <src>.feat.json
  -ffmpeg <path>     ffmpeg 可执行文件，默认 ffmpeg
  -threads <n>       解码线程数，默认 2
  -force             已有缓存时强制重跑

export 参数:
  -labels <jsonl>    标注文件（一行一个切片）
  -cache-dir <目录>  特征缓存目录（probe 的输出）
  -out <csv>         输出 CSV

标注 JSONL 格式（一行一个切片）:
  {"clip":"a.mp4","streamer":"某主播","scene":"dance","duration":899,
   "positive":[[450,600],[625,780]],"note":"450-600 跳舞"}
`)
	os.Exit(2)
}

// ── probe ──────────────────────────────────────────────────────────────

func cmdProbe(args []string) {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	src := fs.String("src", "", "视频文件")
	cache := fs.String("cache", "", "特征缓存输出路径")
	ffmpegBin := fs.String("ffmpeg", "ffmpeg", "ffmpeg 路径")
	threads := fs.Int("threads", 2, "解码线程数")
	force := fs.Bool("force", false, "强制重跑")
	_ = fs.Parse(args)

	if *src == "" {
		fmt.Fprintln(os.Stderr, "缺少 -src")
		os.Exit(2)
	}
	out := *cache
	if out == "" {
		out = *src + ".feat.json"
	}

	if !*force {
		if f, err := highlight.LoadFeatures(out); err == nil {
			fmt.Printf("缓存已存在，跳过: %s（%d 秒 / %d 列）\n", out, f.Seconds, len(f.Names))
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	t0 := time.Now()
	f, err := highlight.ExtractFeatures(ctx, *ffmpegBin, *src, *threads)
	if err != nil {
		fmt.Fprintf(os.Stderr, "特征提取失败 %s: %v\n", filepath.Base(*src), err)
		os.Exit(1)
	}
	elapsed := time.Since(t0)

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "创建缓存目录失败: %v\n", err)
		os.Exit(1)
	}
	if err := highlight.SaveFeatures(out, f); err != nil {
		fmt.Fprintf(os.Stderr, "写入缓存失败: %v\n", err)
		os.Exit(1)
	}

	speed := 0.0
	if elapsed.Seconds() > 0 {
		speed = float64(f.Seconds) / elapsed.Seconds()
	}
	fmt.Printf("%-42s %4d 秒 / %2d 列 | %6s | %.2fx 实时 → %s\n",
		filepath.Base(*src), f.Seconds, len(f.Names),
		elapsed.Truncate(time.Second), speed, out)
}

// ── export ─────────────────────────────────────────────────────────────

// Label 是一个切片的标注。
//
// Positive 用「秒级区间列表」而不是「段级标签」：区间标注的人工成本与段级几乎相同，
// 但能直接算出秒级 P/R/F1 与段级 IoU 两套指标，信息量更大。
type Label struct {
	Clip     string   `json:"clip"`     // 文件名，用于在缓存目录里找 <clip>.feat.json
	Streamer string   `json:"streamer"` // 主播名，交叉验证时按它分组（同一场的切片高度相关）
	Scene    string   `json:"scene"`    // dance / chat / game / sing / idle
	Duration int      `json:"duration"` // 秒，用于校验与缓存是否对得上
	Positive [][2]int `json:"positive"` // 高光区间 [start, end)
	Note     string   `json:"note"`
}

func cmdExport(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	labelsPath := fs.String("labels", "", "标注 JSONL")
	cacheDir := fs.String("cache-dir", "", "特征缓存目录")
	out := fs.String("out", "", "输出 CSV")
	fs.Parse(args)

	if *labelsPath == "" || *cacheDir == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "缺少 -labels / -cache-dir / -out")
		os.Exit(2)
	}

	labels, err := readLabels(*labelsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取标注失败: %v\n", err)
		os.Exit(1)
	}
	if len(labels) == 0 {
		fmt.Fprintln(os.Stderr, "标注为空")
		os.Exit(1)
	}

	var (
		rows      [][]string
		header    []string
		totalPos  int
		totalSec  int
		skippedNo []string
	)
	for _, lb := range labels {
		cachePath := filepath.Join(*cacheDir, lb.Clip+".feat.json")
		f, err := highlight.LoadFeatures(cachePath)
		if err != nil {
			skippedNo = append(skippedNo, lb.Clip)
			continue
		}
		if header == nil {
			header = append([]string{"clip", "streamer", "scene", "sec", "label"}, f.Names...)
		}
		// 标注的 duration 与缓存秒数不一致时以缓存为准，但记下来 —— 多半是标注时
		// 拿错了源文件，或者缓存是旧版本的（改了滤镜参数就该重跑）。
		n := f.Seconds
		if lb.Duration > 0 && lb.Duration != n {
			fmt.Fprintf(os.Stderr, "⚠️  %s 标注时长 %d 秒与缓存 %d 秒不一致，按缓存处理\n",
				lb.Clip, lb.Duration, n)
		}
		for sec := 0; sec < n; sec++ {
			label := 0
			if inPositive(lb.Positive, sec) {
				label = 1
				totalPos++
			}
			row := make([]string, 0, len(header))
			row = append(row, lb.Clip, lb.Streamer, lb.Scene, strconv.Itoa(sec), strconv.Itoa(label))
			for _, name := range f.Names {
				row = append(row, strconv.FormatFloat(f.Column(name)[sec], 'f', 6, 64))
			}
			rows = append(rows, row)
			totalSec++
		}
	}

	if len(skippedNo) > 0 {
		fmt.Fprintf(os.Stderr, "⚠️  %d 个切片没有特征缓存，已跳过（先跑 probe）: %s\n",
			len(skippedNo), strings.Join(skippedNo, ", "))
	}
	if header == nil {
		fmt.Fprintln(os.Stderr, "没有任何切片可用，导出中止")
		os.Exit(1)
	}

	if err := writeCSV(*out, header, rows); err != nil {
		fmt.Fprintf(os.Stderr, "写 CSV 失败: %v\n", err)
		os.Exit(1)
	}

	posRate := 0.0
	if totalSec > 0 {
		posRate = float64(totalPos) / float64(totalSec) * 100
	}
	fmt.Printf("导出 %d 行 × %d 列 → %s\n", len(rows), len(header), *out)
	fmt.Printf("正样本 %d 秒 / 共 %d 秒（%.1f%%）\n", totalPos, totalSec, posRate)
	if posRate > 60 {
		fmt.Fprintln(os.Stderr, "⚠️  正样本占比超过 60%，模型可能退化成「全选」，检查标注区间是否过宽")
	}
}

// inPositive 判断某一秒是否落在标注区间内（区间为 [start, end)）。
func inPositive(spans [][2]int, sec int) bool {
	for _, sp := range spans {
		if len(sp) != 2 {
			continue
		}
		if sec >= sp[0] && sec < sp[1] {
			return true
		}
	}
	return false
}

func readLabels(path string) ([]Label, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Label
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var lb Label
		if err := json.Unmarshal([]byte(line), &lb); err != nil {
			return nil, fmt.Errorf("第 %d 行解析失败: %w", i+1, err)
		}
		if lb.Clip == "" {
			return nil, fmt.Errorf("第 %d 行缺少 clip 字段", i+1)
		}
		out = append(out, lb)
	}
	return out, nil
}

func writeCSV(path string, header []string, rows [][]string) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fh.Close()

	w := csv.NewWriter(fh)
	if err := w.Write(header); err != nil {
		return err
	}
	if err := w.WriteAll(rows); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}
