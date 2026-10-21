//go:build cgo

// videomae-probe VideoMAE 视频分类头 Go 侧推理验证（§47.5 可移植路线）。
//
// 目的：证明导出的 videomae_small_k400.onnx 能在纯 Go + onnxruntime_go 链路上
// 跑通（服务器 sysroot ORT 同构），并与 Python 探针缓存的嵌入做余弦比对——
// Go/Python 预处理重采样核不同（双线性 vs PIL 双三次），余弦 >0.99 即认定链路等价；
// 生产集成时训练/推理用同一 Go 预处理即可，无需与 PIL 位级一致。
//
// 预处理纯函数在 videomae_prep.go（无构建约束），本文件只负责 ORT 推理。
//
// 用法：
//
//	hleval videomae-probe -model <onnx> -dll <onnxruntime.dll> -frames <dir>
//	    -clip <片名> -sec <秒> [-labels <config.json>] [-py-emb <json>] [-out <json>]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	ort "github.com/yalue/onnxruntime_go"
)

func cmdVideomaeProbe(args []string) {
	fs := flag.NewFlagSet("videomae-probe", flag.ExitOnError)
	var (
		model  = fs.String("model", "D:/upload/_vendor/videomae/videomae_small_k400.onnx", "videomae_small_k400.onnx 路径")
		dll    = fs.String("dll", "D:/upload/_vendor/onnxruntime/onnxruntime-win-x64-1.30.0/lib/onnxruntime.dll", "onnxruntime.dll 路径")
		frames = fs.String("frames", "D:/upload/_diag/train/_pose_pilot/frames", "1fps 帧根目录（子目录=片）")
		clip   = fs.String("clip", "", "片名（帧目录名）")
		sec    = fs.Int("sec", 0, "窗口起始秒")
		labels = fs.String("labels", "D:/upload/_vendor/videomae/small/config.json", "K400 config.json（id2label）")
		pyEmb  = fs.String("py-emb", "", "Python 缓存导出的同窗嵌入 JSON（可选，用于比对）")
		out    = fs.String("out", "", "结果 JSON 输出路径（可选）")
		dump   = fs.String("dump", "", "调试：导出预处理后张量 [16,3,224,224] 为 JSON")
	)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *model == "" || *dll == "" || *frames == "" || *clip == "" {
		fmt.Fprintln(os.Stderr, "必填: -model -dll -frames -clip")
		os.Exit(1)
	}
	if err := vmRun(*model, *dll, *frames, *clip, *sec, *labels, *pyEmb, *out, *dump); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func vmRun(model, dll, frames, clip string, sec int, labels, pyEmb, out, dump string) error {
	ort.SetSharedLibraryPath(dll)
	if !ort.IsInitialized() {
		if err := ort.InitializeEnvironment(); err != nil {
			return fmt.Errorf("videomae: 初始化 ORT: %w", err)
		}
	}
	sess, err := ort.NewDynamicSession[float32, float32](model,
		[]string{"pixel_values"}, []string{"embedding", "logits"})
	if err != nil {
		return fmt.Errorf("videomae: 加载模型: %w", err)
	}
	defer sess.Destroy()

	data, err := vmWindowTensor(frames, clip, sec)
	if err != nil {
		return err
	}
	if dump != "" {
		dj, _ := json.Marshal(data)
		if err := os.WriteFile(dump, dj, 0o644); err != nil {
			return fmt.Errorf("videomae: 写 dump: %w", err)
		}
		fmt.Fprintln(os.Stderr, "dump →", dump)
	}
	in, err := ort.NewTensor[float32](ort.NewShape(1, vmFrames, 3, vmSize, vmSize), data)
	if err != nil {
		return fmt.Errorf("videomae: 输入张量: %w", err)
	}
	defer in.Destroy()
	embT, err := ort.NewTensor[float32](ort.NewShape(1, 384), make([]float32, 384))
	if err != nil {
		return fmt.Errorf("videomae: 嵌入输出张量: %w", err)
	}
	defer embT.Destroy()
	logitT, err := ort.NewTensor[float32](ort.NewShape(1, 400), make([]float32, 400))
	if err != nil {
		return fmt.Errorf("videomae: logits 输出张量: %w", err)
	}
	defer logitT.Destroy()
	if err := sess.Run([]*ort.Tensor[float32]{in}, []*ort.Tensor[float32]{embT, logitT}); err != nil {
		return fmt.Errorf("videomae: 推理: %w", err)
	}
	emb := embT.GetData()
	logits := logitT.GetData()

	res := map[string]any{
		"clip": clip, "sec": sec,
		"emb_dim": len(emb), "emb_norm": fmt.Sprintf("%.6f", vmNorm(emb)),
		"emb_head4":  fmt.Sprintf("%.4f %.4f %.4f %.4f", emb[0], emb[1], emb[2], emb[3]),
		"logit_mean": fmt.Sprintf("%.4f", vmMean(logits)),
	}

	if pyEmb != "" {
		var py struct {
			Emb []float32 `json:"emb"`
		}
		b, err := os.ReadFile(pyEmb)
		if err != nil {
			return fmt.Errorf("videomae: 读 -py-emb: %w", err)
		}
		if err := json.Unmarshal(b, &py); err != nil {
			return fmt.Errorf("videomae: 解析 -py-emb: %w", err)
		}
		if len(py.Emb) != len(emb) {
			return fmt.Errorf("videomae: 维度不一致 go=%d py=%d", len(emb), len(py.Emb))
		}
		res["cos_vs_py"] = fmt.Sprintf("%.6f", vmCosine(emb, py.Emb))
		res["max_abs_diff"] = fmt.Sprintf("%.6f", vmMaxAbsDiff(emb, py.Emb))
	}
	if labels != "" {
		top, err := vmTop5(labels, logits)
		if err != nil {
			return err
		}
		res["top5"] = top
	}
	b, _ := json.MarshalIndent(res, "", " ")
	fmt.Println(string(b))
	if out != "" {
		return os.WriteFile(out, b, 0o644)
	}
	return nil
}
