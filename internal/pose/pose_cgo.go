//go:build cgo

// ONNX 姿态推理（仅 cgo 构建可用）。见 pose.go 文档。
package pose

import (
	"fmt"
	ort "github.com/yalue/onnxruntime_go"
	"image"
	_ "image/jpeg" // jpeg 解码注册
	_ "image/png"
	"math"
	"os"
)

// InSize：模型输入边长（yolov8n-pose 640）。
const InSize = 640

// Detector：ONNX 会话封装。
type Detector struct {
	session  *ort.DynamicSession[float32, float32]
	outShape ort.Shape
}

// NewDetector 初始化运行时并加载模型。dllPath 指向 onnxruntime.dll，
// modelPath 指向 yolov8n-pose.onnx。Initialize 幂等。
func NewDetector(dllPath, modelPath string) (*Detector, error) {
	ort.SetSharedLibraryPath(dllPath)
	if !ort.IsInitialized() {
		if err := ort.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("pose: 初始化 ORT 环境: %w", err)
		}
	}
	const inName = "images"
	const outName = "output0"
	s, err := ort.NewDynamicSession[float32, float32](modelPath, []string{inName}, []string{outName})
	if err != nil {
		return nil, fmt.Errorf("pose: 加载模型: %w", err)
	}
	// output0: [1, 56, 8400]（4 box + 1 conf + 17×3 kpt）
	return &Detector{session: s, outShape: ort.NewShape(1, 56, 8400)}, nil
}

// Close 释放会话。
func (d *Detector) Close() error { return d.session.Destroy() }

// DetectFile 对图片文件跑姿态推理。
func (d *Detector) DetectFile(path string) (*FramePose, error) {
	f, err := openImage(path)
	if err != nil {
		return nil, err
	}
	return d.DetectImage(f)
}
func openImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

// DetectImage 对单帧跑姿态推理（取置信度最高的人）。
func (d *Detector) DetectImage(img image.Image) (*FramePose, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	lb := letterbox(w, h, InSize)
	// CHW float32 RGB /255，letterbox 填充灰 114/255
	data := make([]float32, 3*InSize*InSize)
	const fill = 114.0 / 255.0
	for c := 0; c < 3; c++ {
		plane := data[c*InSize*InSize : (c+1)*InSize*InSize]
		for i := range plane {
			plane[i] = fill
		}
	}
	for y := 0; y < h; y++ {
		dy := int(float64(y)*lb.scale + lb.padY)
		for x := 0; x < w; x++ {
			dx := int(float64(x)*lb.scale + lb.padX)
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			data[dy*InSize+dx] = float32(r>>8) / 255
			data[InSize*InSize+dy*InSize+dx] = float32(g>>8) / 255
			data[2*InSize*InSize+dy*InSize+dx] = float32(bl>>8) / 255
		}
	}
	in, err := ort.NewTensor[float32](ort.NewShape(1, 3, InSize, InSize), data)
	if err != nil {
		return nil, err
	}
	defer in.Destroy()
	out, err := ort.NewTensor[float32](d.outShape, make([]float32, 56*8400))
	if err != nil {
		return nil, err
	}
	defer out.Destroy()
	if err := d.session.Run([]*ort.Tensor[float32]{in}, []*ort.Tensor[float32]{out}); err != nil {
		return nil, err
	}
	return decodeYOLOPose(out.GetData(), []int64(d.outShape), lb)
}

// decodeYOLOPose：output [1,56,8400] → 最高置信人 → 17 kpt（原图坐标）。
func decodeYOLOPose(out []float32, shape []int64, lb lbInfo) (*FramePose, error) {
	if len(shape) != 3 {
		return nil, fmt.Errorf("pose: 输出维度异常 %v", shape)
	}
	dim2 := int(shape[1])
	cells := int(shape[2])
	nk := (dim2 - 5) / 3
	if nk != 17 {
		return nil, fmt.Errorf("pose: 关键点数异常 %d", nk)
	}
	type cand struct {
		x1, y1, x2, y2, conf float64
		kpts                 [17]Landmark
	}
	var cands []cand
	get := func(row, cell int) float64 { return float64(out[row*cells+cell]) }
	for c := 0; c < cells; c++ {
		conf := get(4, c)
		if conf < 0.25 {
			continue
		}
		cx, cy := get(0, c), get(1, c)
		bw, bh := get(2, c), get(3, c)
		var cd cand
		cd.conf = conf
		cd.x1, cd.y1 = cx-bw/2, cy-bh/2
		cd.x2, cd.y2 = cx+bw/2, cy+bh/2
		for k := 0; k < nk; k++ {
			cd.kpts[k] = Landmark{
				X:    (get(5+3*k, c) - lb.padX) / lb.scale,
				Y:    (get(6+3*k, c) - lb.padY) / lb.scale,
				Conf: get(7+3*k, c),
			}
		}
		cands = append(cands, cd)
	}
	if len(cands) == 0 {
		return &FramePose{Detected: false}, nil
	}
	// 按置信度排序 + NMS
	for i := 0; i < len(cands); i++ {
		for j := i + 1; j < len(cands); j++ {
			if cands[j].conf > cands[i].conf {
				cands[i], cands[j] = cands[j], cands[i]
			}
		}
	}
	keep := cands[:0]
	for _, a := range cands {
		dup := false
		for _, b := range keep {
			if iou(a.x1, a.y1, a.x2, a.y2, b.x1, b.y1, b.x2, b.y2) > 0.45 {
				dup = true
				break
			}
		}
		if !dup {
			keep = append(keep, a)
			if len(keep) >= 8 {
				break
			}
		}
	}
	if len(keep) == 0 {
		return &FramePose{Detected: false}, nil
	}
	best := keep[0]
	return &FramePose{Detected: true, Conf: best.conf, Kpts: best.kpts}, nil
}
func iou(ax1, ay1, ax2, ay2, bx1, by1, bx2, by2 float64) float64 {
	ix1, iy1 := math.Max(ax1, bx1), math.Max(ay1, by1)
	ix2, iy2 := math.Min(ax2, bx2), math.Min(ay2, by2)
	iw, ih := math.Max(0, ix2-ix1), math.Max(0, iy2-iy1)
	inter := iw * ih
	if inter <= 0 {
		return 0
	}
	ua := (ax2-ax1)*(ay2-ay1) + (bx2-bx1)*(by2-by1) - inter
	return inter / ua
}

// letterbox 计算缩放与边距。
type lbInfo struct {
	scale      float64
	padX, padY float64
}

func letterbox(w, h int, size int) lbInfo {
	s := math.Min(float64(size)/float64(w), float64(size)/float64(h))
	return lbInfo{scale: s,
		padX: (float64(size) - float64(w)*s) / 2,
		padY: (float64(size) - float64(h)*s) / 2}
}
