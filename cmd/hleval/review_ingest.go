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
	"strconv"
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
	detMin := fs.Float64("detmin", 0.2, "窗口姿态检出率下限")
	visMin := fs.Float64("vismin", 0.6, "检出秒 vis 均值下限")
	faceMax := fs.Float64("facemax", 0.14, "检出秒 face 均值上限")
	extLo := fs.Float64("extlo", 0.25, "ext 下限")
	extHi := fs.Float64("exthi", 1.5, "ext 上限")
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
		news = append(news, p)
		return nil
	})
	if walkErr != nil {
		fmt.Fprintln(os.Stderr, "扫目录失败:", walkErr)
		os.Exit(1)
	}
	sort.Strings(news)
	fmt.Printf("待入池新切片: %d\n", len(news))
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
			spans, _ := riPrelabel(stored, *winSec, *detMin, *visMin, *faceMax, *extLo, *extHi)
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
				continue
			}
		}
		jpegs := riFrameList(fdir)
		if len(jpegs) < 20 {
			fmt.Printf("  [%d/%d] 帧不足，跳过 %s (%d)\n", i+1, len(news), stem, len(jpegs))
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
		spans, _ := riPrelabel(feats, *winSec, *detMin, *visMin, *faceMax, *extLo, *extHi)
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
		known[stem] = true
		added++
		fmt.Printf("  [%d/%d] %s: %d 帧 / %d 段预标\n", i+1, len(news), trunc(stem, 40), len(feats), len(spans))
	}
	fmt.Printf("review-ingest 完成: 新增 %d 片（config 总 %d 片）\n", added, len(cfg))
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

// riPrelabel：det-aware v2 窗口预标 → 连续段
func riPrelabel(feats []poseSec, winSec int, detMin, visMin, faceMax, extLo, extHi float64) (
	spans []map[string]interface{}, labels map[string]string) {
	labels = map[string]string{}
	var keys []int
	for s := 0; s < len(feats); s += winSec {
		e := s + winSec
		if e > len(feats) {
			e = len(feats)
		}
		det := 0
		var sv, sf, se float64
		for _, f := range feats[s:e] {
			if f[4] == 1 {
				det++
				sv += f[0]
				sf += f[1]
				se += f[2]
			}
		}
		if det == 0 {
			continue
		}
		dr := float64(det) / float64(e-s)
		mv, mf, me := sv/float64(det), sf/float64(det), se/float64(det)
		lb := "other"
		switch {
		case dr < detMin:
			lb = "none"
		case visMin <= mv && mf <= faceMax && extLo <= me && me <= extHi:
			lb = "dance"
		case mf > faceMax:
			lb = "closeup"
		}
		labels[strconv.Itoa(s)] = lb
		keys = append(keys, s)
	}
	sort.Ints(keys)
	for _, s := range keys {
		lb := labels[strconv.Itoa(s)]
		e := s + winSec
		if len(spans) > 0 && spans[len(spans)-1]["label"] == lb && s == spans[len(spans)-1]["end"].(int) {
			spans[len(spans)-1]["end"] = e
			continue
		}
		spans = append(spans, map[string]interface{}{"label": lb, "start": s, "end": e})
	}
	return spans, labels
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
