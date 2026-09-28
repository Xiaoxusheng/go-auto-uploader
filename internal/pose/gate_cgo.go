//go:build cgo

// 姿态门段级过滤（cgo 实现）：对候选段抽帧跑姿态，按段保留/丢弃。
package pose

import (
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// GateOptions 段级过滤参数（与 highlight.PoseGateParams 字段对应）。
type GateOptions struct {
	DetMin float64
	// FPS 段内抽帧率。默认 5：奈奎斯特 2.5Hz，覆盖舞曲节拍 1.7~2.3Hz，
	// 为将来节拍耦合特征免重抽（1fps 的奈奎斯特只有 0.5Hz，看不到节拍）。
	// vis/face 聚合语义与抽帧率无关，但门槛定标基于 1fps 抽帧，
	// 5fps 下分布可能略移——灰度前需用 pose-scan 重定标一次。
	FPS       int
	VisMin    float64
	FaceMax   float64
	KeepRatio float64

	// 学习型门头段级投票（开封 #7 灰度）：enable 时段内头判舞窗占比 ≥ HeadFrac
	// 才保留段。只会删段不会加段；头加载失败自动放行（不误杀）。
	HeadEnable    bool
	HeadModelPath string
	HeadFrac      float64
}

// GatePassWith 参数化单秒判定（§22 定标：vis+face 两条件最优，ext 带无判别力已移除）。
func GatePassWith(f *FrameFeatures, o GateOptions) bool {
	if f == nil || !f.Detected {
		return true // 无姿态数据的秒不误杀
	}
	return f.VisRatio >= o.VisMin && f.FaceFrac <= o.FaceMax
}

var (
	lazyMu     sync.Mutex
	lazyDetect *Detector
	lazyDLL    string
	lazyModel  string

	headMu      sync.Mutex
	headLoaded  *HeadModel
	headLoadedP string
)

// DefaultHeadModelPath 灰度头模型内置路径（配置 head_model 空时使用）。
const DefaultHeadModelPath = "D:/upload/_diag/train/gate_head/gate_head_v2_trees.json"

// lazyHead 懒加载学习型门头（按路径缓存；失败返回错误由调用方放行）。
func lazyHead(path string) (*HeadModel, error) {
	if path == "" {
		path = DefaultHeadModelPath
	}
	headMu.Lock()
	defer headMu.Unlock()
	if headLoaded != nil && headLoadedP == path {
		return headLoaded, nil
	}
	m, err := LoadHeadModel(path)
	if err != nil {
		return nil, err
	}
	headLoaded, headLoadedP = m, path
	return m, nil
}

func lazyDetector(dllPath, modelPath string) (*Detector, error) {
	lazyMu.Lock()
	defer lazyMu.Unlock()
	if lazyDetect != nil && lazyDLL == dllPath && lazyModel == modelPath {
		return lazyDetect, nil
	}
	d, err := NewDetector(dllPath, modelPath)
	if err != nil {
		return nil, err
	}
	lazyDetect, lazyDLL, lazyModel = d, dllPath, modelPath
	return d, nil
}

// FilterSegments 对候选段逐段抽帧（默认 5fps）跑姿态门：
// 段内「已知秒」通过占比 ≥ KeepRatio 才保留。无姿态数据的段放行（不误杀）。
// 抽帧/推理异常的段视为不可判定 → 放行（宁可多留，不误杀）。
func FilterSegments(ffmpegBin, src string, segs [][2]int, o GateOptions,
	dllPath, modelPath string) (kept [][2]int, dropped int, err error) {
	if dllPath == "" {
		dllPath = "onnxruntime.dll"
	}
	if modelPath == "" {
		modelPath = "yolov8n-pose.onnx"
	}
	if o.FPS <= 0 {
		o.FPS = 5
	}
	det, err := lazyDetector(dllPath, modelPath)
	if err != nil {
		return segs, 0, fmt.Errorf("pose: 姿态门初始化失败（放行全部段）: %w", err)
	}
	tmp, err := os.MkdirTemp("", "posegate")
	if err != nil {
		return segs, 0, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	kept = [][2]int{}
	for _, sg := range segs {
		st, en := sg[0], sg[1]
		dur := en - st
		if dur <= 0 {
			continue
		}
		fdir := filepath.Join(tmp, fmt.Sprintf("%d_%d", st, en))
		if mkerr := os.MkdirAll(fdir, 0o755); mkerr != nil {
			return segs, 0, mkerr
		}
		// -ss 在 -i 前：快速定位；按 o.FPS 抽帧（默认 5fps：奈奎斯特 2.5Hz，
		// 覆盖舞曲节拍 1.7~2.3Hz，为将来节拍耦合特征免重抽；vis/face/ext 聚合语义不变）
		cmd := exec.Command(ffmpegBin, "-y", "-ss", fmt.Sprint(st), "-t", fmt.Sprint(dur),
			"-i", src, "-vf", "fps="+fmt.Sprint(o.FPS)+",scale=480:-2", "-q:v", "5",
			filepath.Join(fdir, "f_%04d.jpg"))
		if runerr := cmd.Run(); runerr != nil {
			kept = append(kept, sg) // 抽帧失败 → 不可判定 → 放行
			continue
		}
		jpegs, _ := filepath.Glob(filepath.Join(fdir, "f_*.jpg"))
		if len(jpegs) == 0 {
			kept = append(kept, sg)
			continue
		}
		var ffs []*FrameFeatures
		for _, jp := range jpegs {
			img, derr := decodeJPEG(jp)
			if derr != nil {
				continue
			}
			fp, derr := det.DetectImage(img)
			if derr != nil {
				continue
			}
			ffs = append(ffs, FeaturesFromFrame(fp, img.Bounds().Dx(), img.Bounds().Dy()))
		}
		detCnt := 0
		for _, ff := range ffs {
			if ff.Detected {
				detCnt++
			}
		}
		// 无可用帧（解码/推理全失败）→ 不可判定 → 放行（宁可多留，不误杀）。
		// ⚠️ 必须放在 detmin 判定之前：「整段判不了」与「整段确实无人」是两回事，
		// 前者放行、后者才该拒。若顺序颠倒，DetMin>0 时 0 帧会被算成 det率 0 而误拒。
		if len(ffs) == 0 {
			kept = append(kept, sg)
			continue
		}
		// detmin 分母必须是帧数 len(ffs) 而不是秒数 dur：抽帧是 o.FPS fps，
		// detCnt 数的是帧——用 dur 当分母会把检出率放大 FPS 倍（5fps 下
		// detmin 0.2 实际只挡 4% 检出秒）。帧占比与抽帧率无关，
		// 等价于 §22 定标时 1fps 的「检出秒占比」语义。
		if float64(detCnt)/float64(len(ffs)) < o.DetMin {
			// 现场砍留归因必需：门只记数量时无法区分「内容确实无人」与「运行时异常」
			log.Printf("[POSE-GATE] %s 段[%d-%ds] det关拒: 帧%d 检出%d det率=%.3f (<%.2f)",
				filepath.Base(src), st, en, len(ffs), detCnt, float64(detCnt)/float64(len(ffs)), o.DetMin)
			dropped++ // 姿态层几乎无证据 → 无人/特效/特写聊天场 → 拒
			continue
		}
		// 时序平滑（窗 3，仅在检出帧上取均值）
		smV, smF := make([]float64, len(ffs)), make([]float64, len(ffs))
		for i := range ffs {
			lo, hi := i-1, i+2
			if lo < 0 {
				lo = 0
			}
			if hi > len(ffs) {
				hi = len(ffs)
			}
			sv, sf, cnt := 0.0, 0.0, 0
			for k := lo; k < hi; k++ {
				if ffs[k].Detected {
					sv += ffs[k].VisRatio
					sf += ffs[k].FaceFrac
					cnt++
				}
			}
			if cnt == 0 {
				smV[i], smF[i] = ffs[i].VisRatio, ffs[i].FaceFrac
			} else {
				smV[i], smF[i] = sv/float64(cnt), sf/float64(cnt)
			}
		}
		known, okCnt := 0, 0
		sumV, sumF := 0.0, 0.0
		for i := range ffs {
			if !ffs[i].Detected {
				continue
			}
			known++
			sumV += smV[i]
			sumF += smF[i]
			if GatePassWith(&FrameFeatures{Detected: true, VisRatio: smV[i], FaceFrac: smF[i]}, o) {
				okCnt++
			}
		}
		if known == 0 {
			kept = append(kept, sg)
			continue
		}
		if float64(okCnt)/float64(known) >= o.KeepRatio {
			// 学习型门头段级投票（灰度）：段内滑 8s 窗（与训练口径一致，尾窗截短），
			// 头判舞窗占比 ≥ HeadFrac 才保留。头加载失败 → 放行（不误杀）。
			if o.HeadEnable {
				hm, herr := lazyHead(o.HeadModelPath)
				if herr != nil {
					log.Printf("[POSE-GATE] ⚠️ %s 头加载失败（放行）: %v", filepath.Base(src), herr)
					kept = append(kept, sg)
					continue
				}
				secs := headSeconds(ffs, o.FPS)
				ok, frac := headSegmentVote(secs, hm, o.HeadFrac)
				if !ok {
					log.Printf("[POSE-GATE] %s 段[%d-%ds] 头投票拒: 段内%d窗 frac_dance=%.3f (<%.2f)",
						filepath.Base(src), st, en, len(secs), frac, o.HeadFrac)
					dropped++
					continue
				}
			}
			kept = append(kept, sg)
		} else {
			log.Printf("[POSE-GATE] %s 段[%d-%ds] keep关拒: 检出帧%d 过%d keep率=%.3f (<%.2f) 平滑后均值 vis=%.3f face=%.3f",
				filepath.Base(src), st, en, known, okCnt, float64(okCnt)/float64(known), o.KeepRatio, sumV/float64(known), sumF/float64(known))
			dropped++
		}
	}
	return kept, dropped, nil
}

func decodeJPEG(p string) (image.Image, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return jpeg.Decode(f)
}
