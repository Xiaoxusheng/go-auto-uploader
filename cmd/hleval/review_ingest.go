// review-ingest：复核池自动入池（Go 全流程）。
//
// 流程：扫 downloads 找新稳定切片（不在 clips_config、3 分钟未写入）→
// ffmpeg 抽帧 1fps → in-process 姿态推理（internal/pose）→ 每秒特征并入
// pose 特征 JSON → det-aware v2 模型预标 → 追加 clips_config.json。
// 每处理完一片立即落盘 pose 特征与 clips_config.json（复核页可边跑边看新片）。
// 幂等：已在 config 的片跳过；pose 特征已有的片直接复用做预标，不重复推理，
// 可随时中断续跑。
//
// 用法：
//
//	hleval review-ingest -downloads <dir> -frames <dir> -config <clips_config.json> \
//	  -pose-out <pose_features.json> -dll <onnxruntime.dll> -model <yolov8n-pose.onnx>
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"upload/internal/pose"
)

type clipConfigEntry struct {
	Clip  string                   `json:"clip"`
	Prior string                   `json:"prior"`
	Spans []map[string]interface{} `json:"spans"`
	Model bool                     `json:"model,omitempty"`
}

type poseSec [5]float64 // vis, face, ext, aspect, detected

// riSkipMark 碎片切片的跳过标记：抽帧失败或帧数不足，重试也不会成功。
// 记下源文件字节数——文件被重录（大小变化）时标记自动失效，重新尝试。
// 历史问题：这类片不进 clips_config，每轮 walk 都被当「新片」重扫重抽；
// 实测 11 片待入池里 10 片是 3~18 秒碎片，每 3 分钟白跑一遍。
type riSkipMark struct {
	Reason string `json:"reason"`
	Frames int    `json:"frames"`
	Size   int64  `json:"size"`
	At     string `json:"at"`
}

func cmdReviewIngest(args []string) {
	fs := flag.NewFlagSet("review-ingest", flag.ExitOnError)
	downloads := fs.String("downloads", "D:/upload/downloads", "录制根目录")
	framesRoot := fs.String("frames", "D:/upload/_diag/train/_pose_pilot/frames", "帧输出根目录")
	cfgPath := fs.String("config", "D:/upload/_diag/train/_pose_pilot/clips_config.json", "clips_config.json 路径")
	poseOut := fs.String("pose-out", "D:/upload/_diag/train/pose_features_go.json", "每秒姿态特征 JSON（增量合并）")
	dllPath := fs.String("dll", "onnxruntime.dll", "onnxruntime.dll 路径")
	modelPath := fs.String("model", "yolov8n-pose.onnx", "姿态 ONNX 模型路径")
	ffmpegBin := fs.String("ffmpeg", "ffmpeg", "ffmpeg 路径")
	stableAge := fs.Int("stable-age", 180, "切片 mtime 超过该秒数才算稳定")
	winSec := fs.Int("win", 8, "预标窗口秒数")
	detMin := fs.Float64("detmin", pose.PrelabelDetMin, "窗口姿态检出率下限")
	visMin := fs.Float64("vismin", pose.PrelabelVisMin, "检出秒 vis 均值下限")
	faceMax := fs.Float64("facemax", pose.PrelabelFaceMax, "检出秒 face 均值上限")
	_ = fs.Parse(args)

	cfgRaw, rerr := os.ReadFile(*cfgPath)
	if rerr != nil {
		fmt.Fprintln(os.Stderr, "读 clips_config 失败:", rerr)
		os.Exit(1)
	}
	var cfg []clipConfigEntry
	if jerr := json.Unmarshal(cfgRaw, &cfg); jerr != nil {
		fmt.Fprintln(os.Stderr, "解析 clips_config 失败:", jerr)
		os.Exit(1)
	}
	known := map[string]bool{}
	for _, c := range cfg {
		known[c.Clip] = true
	}
	poseLoaded := riLoadPoseFeats(*poseOut)

	now := time.Now()
	var news []string
	skipped := 0
	walkErr := filepath.Walk(*downloads, func(p string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(p), ".ts") || strings.Contains(p, "高光") {
			return nil
		}
		stem := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		if known[stem] || now.Sub(info.ModTime()) < time.Duration(*stableAge)*time.Second {
			return nil
		}
		// 已判死的碎片：源文件字节数未变则不再重抽（历史：每轮重扫重抽同一批）
		if riSkipFresh(*framesRoot, stem, info.Size()) {
			skipped++
			return nil
		}
		news = append(news, p)
		return nil
	})
	if walkErr != nil {
		fmt.Fprintln(os.Stderr, "扫目录失败:", walkErr)
		os.Exit(1)
	}
	sort.Strings(news)
	fmt.Printf("待入池新切片: %d（已跳过碎片 %d）\n", len(news), skipped)
	if len(news) == 0 {
		fmt.Println("无新增，完成。")
		return
	}

	det, err := pose.NewDetector(*dllPath, *modelPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "姿态初始化失败:", err)
		os.Exit(1)
	}
	defer func() { _ = det.Close() }()

	added := 0
	for i, src := range news {
		stem := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		// 续跑快路径：pose 特征已存但 config 未落盘的片（上次中断），
		// 直接用存量特征预标入池，不重抽帧不重推理。
		if stored, ok := poseLoaded[stem]; ok && len(stored) > 0 {
			spans := riPrelabel(stored, *winSec, *detMin, *visMin, *faceMax)
			cfg = append(cfg, clipConfigEntry{
				Clip:  stem,
				Prior: "模型预标（自动入池）",
				Spans: spans,
				Model: true,
			})
			known[stem] = true
			added++
			if werr := riWriteConfig(*cfgPath, cfg); werr != nil {
				fmt.Fprintln(os.Stderr, "写 config 失败:", werr)
				os.Exit(1)
			}
			fmt.Printf("  [%d/%d] %s: 续跑复用 %d 秒特征 / %d 段预标\n",
				i+1, len(news), trunc(stem, 40), len(stored), len(spans))
			continue
		}
		fdir := filepath.Join(*framesRoot, stem)
		if riCountFrames(fdir) < 20 {
			if exerr := riExtractFrames(*ffmpegBin, src, fdir); exerr != nil {
				fmt.Printf("  [%d/%d] 抽帧失败 %s: %v\n", i+1, len(news), stem, exerr)
				riWriteSkip(*framesRoot, stem, "extract-failed", 0, riFileSize(src))
				continue
			}
		}
		jpegs := riFrameList(fdir)
		if len(jpegs) < 20 {
			fmt.Printf("  [%d/%d] 帧不足，跳过 %s (%d)\n", i+1, len(news), stem, len(jpegs))
			riWriteSkip(*framesRoot, stem, "too-few-frames", len(jpegs), riFileSize(src))
			continue
		}
		feats := make([]poseSec, 0, len(jpegs))
		for _, jp := range jpegs {
			img, derr := riDecodeImage(jp)
			if derr != nil {
				feats = append(feats, poseSec{})
				continue
			}
			fp, derr := det.DetectImage(img)
			if derr != nil {
				feats = append(feats, poseSec{})
				continue
			}
			ff := pose.FeaturesFromFrame(fp, img.Bounds().Dx(), img.Bounds().Dy())
			if !ff.Detected {
				feats = append(feats, poseSec{})
				continue
			}
			feats = append(feats, poseSec{ff.VisRatio, ff.FaceFrac, ff.ExtH, ff.Aspect, 1})
		}
		spans := riPrelabel(feats, *winSec, *detMin, *visMin, *faceMax)
		cfg = append(cfg, clipConfigEntry{
			Clip:  stem,
			Prior: "模型预标（自动入池）",
			Spans: spans,
			Model: true,
		})
		riMergePoseFeats(*poseOut, stem, feats)
		if werr := riWriteConfig(*cfgPath, cfg); werr != nil {
			fmt.Fprintln(os.Stderr, "写 config 失败:", werr)
			os.Exit(1)
		}
		// 曾判死但这次成功（源文件变过），清掉旧标记
		_ = os.Remove(riSkipPath(*framesRoot, stem))
		known[stem] = true
		added++
		fmt.Printf("  [%d/%d] %s: %d 帧 / %d 段预标\n", i+1, len(news), trunc(stem, 40), len(feats), len(spans))
	}
	fmt.Printf("review-ingest 完成: 新增 %d 片（config 总 %d 片）\n", added, len(cfg))
}

// riSkipPath 碎片跳过标记路径（放在该片的帧目录下，与帧同生共死）。
func riSkipPath(framesRoot, stem string) string {
	return filepath.Join(framesRoot, stem, ".skipped")
}

// riSkipFresh 标记存在且源文件字节数未变 → 本片可跳过（重试也不会成功）。
// size<=0（stat 失败）一律不跳过，宁可多跑一次也不漏片。
func riSkipFresh(framesRoot, stem string, size int64) bool {
	if size <= 0 {
		return false
	}
	b, err := os.ReadFile(riSkipPath(framesRoot, stem))
	if err != nil {
		return false
	}
	var m riSkipMark
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	return m.Size == size
}

// riWriteSkip 落跳过标记；best-effort，失败不影响主流程。
func riWriteSkip(framesRoot, stem, reason string, frames int, size int64) {
	dir := filepath.Join(framesRoot, stem)
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	m := riSkipMark{Reason: reason, Frames: frames, Size: size, At: time.Now().Format(time.RFC3339)}
	if b, err := json.MarshalIndent(m, "", " "); err == nil {
		_ = os.WriteFile(riSkipPath(framesRoot, stem), b, 0o644)
	}
}

// riFileSize 源文件字节数；失败返回 0（此时 riSkipFresh 不会跳过）。
func riFileSize(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

// riWriteConfig 原子落盘 clips_config（临时文件 + 改名，复核页随时在读）。
func riWriteConfig(cfgPath string, cfg []clipConfigEntry) error {
	b, err := json.MarshalIndent(cfg, "", " ")
	if err != nil {
		return err
	}
	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, cfgPath)
}

// riLoadPoseFeats 读存量每秒姿态特征（中断续跑用）。
func riLoadPoseFeats(poseOut string) map[string][]poseSec {
	m := map[string][]poseSec{}
	b, err := os.ReadFile(poseOut)
	if err != nil {
		return m
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return m
	}
	for clip, rb := range raw {
		var e struct {
			Feats [][5]float64 `json:"feats"`
		}
		if json.Unmarshal(rb, &e) != nil {
			continue
		}
		fs := make([]poseSec, len(e.Feats))
		for i, f := range e.Feats {
			fs[i] = poseSec(f)
		}
		m[clip] = fs
	}
	return m
}

func riMergePoseFeats(poseOut, clip string, feats []poseSec) {
	m := map[string]json.RawMessage{}
	if b, err := os.ReadFile(poseOut); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	entry := struct {
		FPS   int          `json:"fps"`
		Feats [][5]float64 `json:"feats"`
	}{FPS: 1, Feats: make([][5]float64, len(feats))}
	for i, f := range feats {
		entry.Feats[i] = [5]float64(f)
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	m[clip] = b
	if wb, werr := json.MarshalIndent(m, "", " "); werr == nil {
		_ = os.WriteFile(poseOut, wb, 0o644)
	}
}

// riPrelabel 8s 窗预标 → 连续段。
// 判定走 pose.AggregateWindow —— 与线上姿态门、控制台可视化**同一口径**（ext 带已去）。
// 返回**空切片**而非 nil：nil 会被 JSON 序列化成 `"spans": null`，历史上让复核页
// （c.spans.find）与控制台片池详情直接抛错（§22 记录的 4 片 null）。
func riPrelabel(feats []poseSec, winSec int, detMin, visMin, faceMax float64) []map[string]interface{} {
	spans := make([]map[string]interface{}, 0)
	prevLabel, prevEnd := "", -1
	for s := 0; s < len(feats); s += winSec {
		e := s + winSec
		if e > len(feats) {
			e = len(feats)
		}
		win := make([]pose.FrameFeatures, e-s)
		for i, f := range feats[s:e] {
			win[i] = pose.FrameFeatures{
				Detected: f[4] == 1,
				VisRatio: f[0],
				FaceFrac: f[1],
				ExtH:     f[2],
				Aspect:   f[3],
			}
		}
		lb := pose.AggregateWindow(win, detMin, visMin, faceMax).Label
		if len(spans) > 0 && lb == prevLabel && s == prevEnd {
			spans[len(spans)-1]["end"] = e
			prevEnd = e
			continue
		}
		spans = append(spans, map[string]interface{}{"label": lb, "start": s, "end": e})
		prevLabel, prevEnd = lb, e
	}
	return spans
}

func riCountFrames(dir string) int {
	n, _ := filepath.Glob(filepath.Join(dir, "f_*.jpg"))
	return len(n)
}

func riFrameList(dir string) []string {
	f, _ := filepath.Glob(filepath.Join(dir, "f_*.jpg"))
	sort.Strings(f)
	return f
}

func riExtractFrames(ffmpegBin, src, fdir string) error {
	if mkerr := os.MkdirAll(fdir, 0o755); mkerr != nil {
		return mkerr
	}
	cmd := exec.Command(ffmpegBin, "-y", "-i", src, "-vf", "fps=1,scale=480:-2",
		"-q:v", "5", filepath.Join(fdir, "f_%04d.jpg"))
	return cmd.Run()
}

func riDecodeImage(p string) (image.Image, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}
