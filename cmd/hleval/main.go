// Command hleval 是高光切片离线训练与评估的工具集。
//
// 刻意做成独立二进制，不参与主程序构建：
// 它要跑 ffmpeg 全量解码（分钟级），只适合离线批量使用，绝不能进线上链路。
//
// 分工：Go 侧负责「要进系统、会反复跑」的能力（解码 + 特征提取 + 导出 +
// 指标评估）。Python 只作探索期脚本；算法定型后迁入本包，避免与
// Select/robustZ 语义分叉。
//
// 用法：
//
//	hleval probe  -src <视频> [-cache <json>] [-ffmpeg <path>] [-force]
//	hleval export -labels <标注.jsonl> -cache-dir <目录> -out <csv>
//	hleval metrics -csv <features.csv> [-mw 1] [-aw 0] [-th 1.2] [-ml 8] [-gap 12]
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
	case "batch-probe":
		cmdBatchProbe(os.Args[2:])
	case "export":
		cmdExport(os.Args[2:])
	case "metrics":
		cmdMetrics(os.Args[2:])
	case "train":
		cmdTrain(os.Args[2:])
	case "pose-scan":
		cmdPoseScan(os.Args[2:])
	case "traj-scan":
		cmdTrajScan(os.Args[2:])
	case "review-ingest":
		cmdReviewIngest(os.Args[2:])
	case "autogold-sweep":
		cmdAutogoldSweep(os.Args[2:])
	case "pose-probe":
		cmdPoseProbe(os.Args[2:])
	case "pose2-probe":
		cmdPose2Probe(os.Args[2:])
	case "uncertainty-export":
		cmdUncertaintyExport(os.Args[2:])
	case "cleanup-sources":
		cmdCleanupSources(os.Args[2:])
	case "audio-probe":
		cmdAudioProbe(os.Args[2:])
	case "videomae-probe":
		cmdVideomaeProbe(os.Args[2:])
	case "de-auc":
		cmdDeAUC(os.Args[2:])
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
  probe       对视频跑全字段特征提取并写缓存
  batch-probe 批量 probe（-dir 递归视频，-per-streamer 限量）
  export      把标注 + 特征缓存导出成训练用 CSV
  metrics     在 features CSV 上评估打分+Select（秒级 + 段级 IoU + hn误检）
  train       在 features CSV 上跑训练对比（Go 版 train.py+ablate，留一切片）
  de-auc      grid.csv + D/E 金标上算 ac1/bstd/center AUC
  pose-scan   批量帧目录 → 每秒姿态特征 JSON（供 metrics -pose）
  review-ingest 新切片自动入池：抽帧+姿态推理+模型预标 → 复核页
  autogold-sweep 金标重定标：8s 窗网格搜 (vis/face/det) → autogold_result.json
  videomae-probe VideoMAE 视频分类头 Go ORT 推理验证（单窗嵌入+K400 top5）

probe 参数:
  -src <视频>        必填
  -cache <json>      缓存输出路径，默认 <src>.feat.json
  -ffmpeg <path>     ffmpeg 可执行文件，默认 ffmpeg
  -threads <n>       解码线程数，默认 2
  -force             已有缓存时强制重跑

batch-probe 参数:
  -dir <目录>        视频根目录
  -cache-dir <目录>  feat.json 输出
  -per-streamer N    每主播最多 N 个（0=不限）
  -limit N           总上限

export 参数:
  -labels <jsonl>    标注文件（一行一个切片）
  -cache-dir <目录>  特征缓存目录（probe 的输出）
  -out <csv>         输出 CSV
  -verified          剔除自证标签（3 条，同 build_features.py SELF_LABELED）

metrics 参数:
  -csv <features>    export 产出的 CSV
  -mw/-aw/-th        权重与阈值（默认 1.0/0.0/1.2）
  -ml/-gap/-pad      Select 后处理（默认 8/12/0）
  -smooth            滑动窗口，默认 5
  -pose <json>       每秒姿态特征（pose-scan 产出），启用姿态门 v2（det-aware）

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

	if err := runProbeOne(*src, out, *ffmpegBin, *threads, *force); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

// runProbeOne 提一次全字段特征并写缓存。
func runProbeOne(src, out, ffmpegBin string, threads int, force bool) error {
	if !force {
		if f, err := highlight.LoadFeatures(out); err == nil {
			fmt.Printf("缓存已存在，跳过: %s（%d 秒 / %d 列）\n", out, f.Seconds, len(f.Names))
			return nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	t0 := time.Now()
	f, err := highlight.ExtractFeatures(ctx, ffmpegBin, src, threads)
	if err != nil {
		return fmt.Errorf("特征提取失败 %s: %w", filepath.Base(src), err)
	}
	elapsed := time.Since(t0)

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("创建缓存目录失败: %w", err)
	}
	if err := highlight.SaveFeatures(out, f); err != nil {
		return fmt.Errorf("写入缓存失败: %w", err)
	}

	speed := 0.0
	if elapsed.Seconds() > 0 {
		speed = float64(f.Seconds) / elapsed.Seconds()
	}
	fmt.Printf("%-42s %4d 秒 / %2d 列 | %6s | %.2fx 实时 → %s\n",
		filepath.Base(src), f.Seconds, len(f.Names),
		elapsed.Truncate(time.Second), speed, out)
	return nil
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
	// NegativeHard 困难负样本 [start, end] 或 [start, end, note]：
	// 礼物特效 / 切近景 / 连麦 / 空镜等「高运动但不是跳舞」的区间。
	NegativeHard []HardSpan `json:"negative_hard"`
	Note         string     `json:"note"`
}

// HardSpan 是 labels.jsonl 里 negative_hard 的一条：JSON 数组 [start, end] 或 [start, end, note]。
type HardSpan struct {
	Start int
	End   int
	Note  string
}

func (h *HardSpan) UnmarshalJSON(data []byte) error {
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err == nil && len(arr) >= 2 {
		_ = json.Unmarshal(arr[0], &h.Start)
		_ = json.Unmarshal(arr[1], &h.End)
		if len(arr) >= 3 {
			_ = json.Unmarshal(arr[2], &h.Note)
		}
		return nil
	}
	type alias struct {
		Start int    `json:"start"`
		End   int    `json:"end"`
		Note  string `json:"note"`
	}
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	h.Start, h.End, h.Note = a.Start, a.End, a.Note
	return nil
}

func inHardNeg(spans []HardSpan, sec int) bool {
	for _, sp := range spans {
		if sec >= sp.Start && sec < sp.End {
			return true
		}
	}
	return false
}

func cmdExport(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	labelsPath := fs.String("labels", "", "标注 JSONL")
	cacheDirs := fs.String("cache-dir", "", "特征缓存目录，逗号可分多个")
	out := fs.String("out", "", "输出 CSV")
	verified := fs.Bool("verified", false, "剔除自证标签（同 build_features.py 的 SELF_LABELED 3 条）")
	fs.Parse(args)

	if *labelsPath == "" || *cacheDirs == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "缺少 -labels / -cache-dir / -out")
		os.Exit(2)
	}
	var dirs []string
	for _, d := range strings.Split(*cacheDirs, ",") {
		d = strings.TrimSpace(d)
		if d != "" {
			dirs = append(dirs, d)
		}
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
	if *verified {
		var kept []Label
		var excluded []string
		for _, lb := range labels {
			if selfLabeled[lb.Clip] {
				excluded = append(excluded, lb.Clip)
				continue
			}
			kept = append(kept, lb)
		}
		labels = kept
		if len(excluded) > 0 {
			fmt.Fprintf(os.Stderr, "剔除自证标签 %d 条: %s\n", len(excluded), strings.Join(excluded, ", "))
		}
	}

	var (
		rows      [][]string
		header    []string
		totalPos  int
		totalSec  int
		skippedNo []string
	)
	for _, lb := range labels {
		var f *highlight.Features
		loaded := false
		for _, d := range dirs {
			tmp, err := highlight.LoadFeatures(filepath.Join(d, lb.Clip+".feat.json"))
			if err == nil {
				f = tmp
				loaded = true
				break
			}
		}
		if !loaded {
			skippedNo = append(skippedNo, lb.Clip)
			continue
		}
		if header == nil {
			header = append([]string{"clip", "streamer", "scene", "sec", "label", "hard_neg"}, f.Names...)
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
			hard := 0
			if inHardNeg(lb.NegativeHard, sec) {
				hard = 1
			}
			row := make([]string, 0, len(header))
			row = append(row, lb.Clip, lb.Streamer, lb.Scene, strconv.Itoa(sec), strconv.Itoa(label), strconv.Itoa(hard))
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

// selfLabeled 自证标签（VERIFY_2026-09-23.md 判定：note=候选段标注、未逐帧人工复核）。
// 与 tools/train_highlight/build_features.py 的 SELF_LABELED 完全一致 ——
// 只剔这 3 条，不要按 note 关键字扩大剔除面（labels 里另有 10 条
// 「候选段 z>2 自动峰草稿 B批」是有意保留进训练集的）。
var selfLabeled = map[string]bool{
	"dance-064023": true,
	"dance-064057": true,
	"dance-064128": true,
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
