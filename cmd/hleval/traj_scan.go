// traj-scan：对 5fps 帧目录跑姿态推理，输出四肢关键点轨迹 JSON（时序动力学特征用）。
//
// 背景：§24 证伪了 1fps 节律/频谱特征族（奈奎斯特 0.5Hz 看不到 1.7~2.3Hz 舞曲节拍），
// 本命令在 5fps 帧上提取手腕/脚踝轨迹，供节拍耦合特征评估（HANDOFF 接下来做 #3）。
//
// 用法：
//
//	hleval traj-scan -frames <5fps帧根目录> -out traj.json [-dll ...] [-model ...]
//
// frames 根目录下每个子目录 = 一个评估窗（f_%04d.jpg 按 5fps 抽取）。
// 输出：{"<窗名>": {"fps":5,"w":W,"h":H,"frames":[[idx, lwx,lwy,lwc, rwx,rwy,rwc,
//
//	lax,lay,lac, rax,ray,rac, msx,msy, mhx,mhy, conf], ...缺检帧跳过]}}
//
// 关键点序号（COCO）：9=左腕 10=右腕 15=左踝 16=右踝；ms/mh = 肩中点/髋中点（躯干归一化用）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"upload/internal/pose"
)

func cmdTrajScan(args []string) {
	fs := flag.NewFlagSet("traj-scan", flag.ExitOnError)
	framesRoot := fs.String("frames", "", "5fps 帧根目录（子目录=评估窗）")
	dllPath := fs.String("dll", "onnxruntime.dll", "onnxruntime.dll 路径")
	modelPath := fs.String("model", "yolov8n-pose.onnx", "姿态 ONNX 模型路径")
	out := fs.String("out", "traj.json", "输出 JSON")
	_ = fs.Parse(args)
	if *framesRoot == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "traj-scan: 需要 -frames 与 -out")
		os.Exit(2)
	}

	det, err := pose.NewDetector(*dllPath, *modelPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "traj-scan:", err)
		os.Exit(1)
	}
	defer func() { _ = det.Close() }()

	result := map[string]json.RawMessage{}
	if b, rerr := os.ReadFile(*out); rerr == nil {
		_ = json.Unmarshal(b, &result) // 增量：保留已有窗
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
			continue
		}
		files, _ := filepath.Glob(filepath.Join(*framesRoot, name, "f_*.jpg"))
		if len(files) < 20 {
			continue
		}
		sort.Strings(files)
		frames := make([][]float64, 0, len(files))
		w, h := 0, 0
		for _, f := range files {
			img, derr := loadFrameImage(f)
			if derr != nil {
				continue
			}
			if w == 0 {
				w, h = img.Bounds().Dx(), img.Bounds().Dy()
			}
			fp, derr := det.DetectImage(img)
			if derr != nil || !fp.Detected {
				continue
			}
			k := fp.Kpts
			mid := func(a, b pose.Landmark) (float64, float64) { return (a.X + b.X) / 2, (a.Y + b.Y) / 2 }
			msx, msy := mid(k[5], k[6])
			mhx, mhy := mid(k[11], k[12])
			frames = append(frames, []float64{
				float64(len(frames)), // 输出序号（非帧号；缺检帧被跳过，分析端按 5fps 折算）
				k[9].X, k[9].Y, k[9].Conf,
				k[10].X, k[10].Y, k[10].Conf,
				k[15].X, k[15].Y, k[15].Conf,
				k[16].X, k[16].Y, k[16].Conf,
				msx, msy, mhx, mhy,
				fp.Conf,
			})
		}
		doc := struct {
			FPS    int         `json:"fps"`
			W      int         `json:"w"`
			H      int         `json:"h"`
			Frames [][]float64 `json:"frames"`
		}{FPS: 5, W: w, H: h, Frames: frames}
		b, _ := json.Marshal(doc)
		result[name] = b
		fmt.Printf("%s: %d 检出帧 / %d 帧\n", name, len(frames), len(files))
	}

	b, _ := json.MarshalIndent(result, "", " ")
	if werr := os.WriteFile(*out, b, 0o644); werr != nil {
		fmt.Fprintln(os.Stderr, "写文件失败:", werr)
		os.Exit(1)
	}
	fmt.Printf("traj-scan 完成 → %s（%d 窗）\n", *out, len(result))
}
