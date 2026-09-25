// pose-scan：批量对 frames/<clip>/f_*.jpg 跑姿态推理，输出每秒语义特征 JSON，
// 供 metrics -pose 在评估中应用姿态门（Go 全链路，替代 Python 模拟）。
//
// 用法：
//
//	hleval pose-scan -frames <frames根目录> -out pose_features.json [-dll ...] [-model ...]
//
// frames 根目录下每个子目录 = 一个 clip（目录名即 clip 名），f_%04d.jpg = 1fps 帧。
// 输出格式：{"<clip>": {"fps":1, "feats": [[vis,face,ext,aspect,det] 每秒一行]}}
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"

	"upload/internal/pose"
)

func cmdPoseScan(args []string) {
	fs := flag.NewFlagSet("pose-scan", flag.ExitOnError)
	framesRoot := fs.String("frames", "", "帧根目录（子目录=clip，f_%04d.jpg=1fps 帧）")
	dllPath := fs.String("dll", "onnxruntime.dll", "onnxruntime.dll 路径")
	modelPath := fs.String("model", "yolov8n-pose.onnx", "姿态 ONNX 模型路径")
	out := fs.String("out", "pose_features.json", "输出 JSON")
	_ = fs.Parse(args)
	if *framesRoot == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "pose-scan: 需要 -frames 与 -out")
		os.Exit(2)
	}

	det, err := pose.NewDetector(*dllPath, *modelPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pose-scan:", err)
		os.Exit(1)
	}
	defer func() { _ = det.Close() }()

	type clipFeats struct {
		FPS   int         `json:"fps"`
		Feats [][5]float64 `json:"feats"`
	}
	result := map[string]clipFeats{}
	// 增量模式：输出文件已存在时保留已有 clip 结果（只推理新增 clip）
	if b, rerr := os.ReadFile(*out); rerr == nil {
		var prev map[string]clipFeats
		if jerr := json.Unmarshal(b, &prev); jerr == nil {
			for k, v := range prev {
				result[k] = v
			}
		}
	}

	entries, err := os.ReadDir(*framesRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读目录失败:", err)
		os.Exit(1)
	}
	names := []string{}
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if _, done := result[name]; done {
			continue // 增量：已推理过的 clip 跳过
		}
		files, _ := filepath.Glob(filepath.Join(*framesRoot, name, "f_*.jpg"))
		if len(files) == 0 {
			continue
		}
		sort.Strings(files)
		cf := clipFeats{FPS: 1}
		for _, f := range files {
			img, derr := loadFrameImage(f)
			if derr != nil {
				cf.Feats = append(cf.Feats, [5]float64{0, 0, 0, 0, 0})
				continue
			}
			fp, derr := det.DetectImage(img)
			if derr != nil {
				cf.Feats = append(cf.Feats, [5]float64{0, 0, 0, 0, 0})
				continue
			}
			ff := pose.FeaturesFromFrame(fp, img.Bounds().Dx(), img.Bounds().Dy())
			if !ff.Detected {
				cf.Feats = append(cf.Feats, [5]float64{0, 0, 0, 0, 0})
				continue
			}
			cf.Feats = append(cf.Feats, [5]float64{ff.VisRatio, ff.FaceFrac, ff.ExtH, ff.Aspect, 1})
		}
		result[name] = cf
		fmt.Printf("%s: %d 秒\n", name, len(cf.Feats))
	}

	b, _ := json.MarshalIndent(result, "", " ")
	if werr := os.WriteFile(*out, b, 0o644); werr != nil {
		fmt.Fprintln(os.Stderr, "写文件失败:", werr)
		os.Exit(1)
	}
	fmt.Printf("pose-scan 完成 → %s（%d 片）\n", *out, len(result))
}

// loadFrameImage 解码图片文件（Windows 中文路径安全）。
func loadFrameImage(p string) (image.Image, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return jpeg.Decode(f)
}
