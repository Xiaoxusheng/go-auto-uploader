// Package pose：高光段姿态语义特征。
//
// 用途：作为高光段后置过滤器（docs/highlight-spatial-de.md §19）——对已选出的
// 高光段抽帧跑人体姿态，聚合 vis_ratio / face_frac / extent_h / aspect 四个
// 语义特征，判定「全身舞蹈 vs 近景聊天 vs 无人的特效」。
//
// 构建隔离：ONNX 推理依赖 cgo（onnxruntime 动态库），仅 CGO_ENABLED=1 时编译
// （pose_cgo.go）；纯 Go 构建下 Detector 为不可用桩、姿态门自动关闭，主程序
// uploader 保持零 cgo。语义特征与门槛函数在本文件，两种构建共用。
//
// 与 MediaPipe 版语义对齐（§18）：
//   - vis_ratio  躯干+四肢关键点平均置信度（全身入镜才是舞）
//   - face_frac  双眼间距/帧宽（脸越大越不是舞）
//   - extent_h   可见关键点纵向跨度占帧高（蹲坐矮、站立舞高）
//   - aspect     包络宽高比（蹲爬宽扁）
//
// 门槛（定标见 §22 全量用户金标 1402 窗）：vis≥0.6 且 face≤0.14（ext 带无判别力
// 已移除），段级 det-aware（det_rate≥0.2 拒无人场）+ 时序平滑——上线前必须过
// 冻结开封复验，默认配置关闭。
package pose

import (
	"image"
	"math"
)

// COCO 17 关键点索引（YOLOv8-pose 输出顺序）
const (
	kLEye = 1
	kREye = 2
	kLSho = 5
	kRSho = 6
	kLWri = 9
	kRWri = 10
	kLHip = 11
	kRHip = 12
	kLAnk = 15
	kRAnk = 16
)

// CoreKpts：躯干+四肢核心点（对应 MediaPipe 版 CORE：肩×2 髋×2 腕×2 踝×2）。
var CoreKpts = []int{kLSho, kRSho, kLHip, kRHip, kLWri, kRWri, kLAnk, kRAnk}

// Landmark 单个关键点（原图像素坐标）。
type Landmark struct {
	X, Y, Conf float64
}

// FramePose 单帧姿态结果。
type FramePose struct {
	Detected bool
	Conf     float64
	Kpts     [17]Landmark
}

// FrameFeatures 单帧语义特征（语义与 MediaPipe 版对齐）。
type FrameFeatures struct {
	Detected bool
	VisRatio float64
	FaceFrac float64
	ExtH     float64
	Aspect   float64
}

// FeaturesFromFrame 把一帧关键点转成语义特征。
// frameW/frameH 用于 face_frac / extent_h 归一化。
func FeaturesFromFrame(fp *FramePose, frameW, frameH int) *FrameFeatures {
	if fp == nil || !fp.Detected {
		return &FrameFeatures{Detected: false}
	}
	var vs []float64
	xs, ys := []float64{}, []float64{}
	for _, i := range CoreKpts {
		k := fp.Kpts[i]
		if k.Conf < 0.3 {
			continue
		}
		vs = append(vs, k.Conf)
		xs = append(xs, k.X)
		ys = append(ys, k.Y)
	}
	le, re := fp.Kpts[kLEye], fp.Kpts[kREye]
	face := math.Hypot(le.X-re.X, le.Y-re.Y)
	out := &FrameFeatures{Detected: len(vs) >= 4}
	if !out.Detected {
		return out
	}
	out.VisRatio = mean(vs)
	out.FaceFrac = face / float64(frameW)
	minX, maxX := minOf(xs), maxOf(xs)
	minY, maxY := minOf(ys), maxOf(ys)
	out.ExtH = (maxY - minY) / float64(frameH)
	out.Aspect = (maxX - minX) / (maxY - minY + 1e-6)
	return out
}

// GatePass 单秒姿态门判定（门槛与 §22 定标一致：vis 0.6 / face 0.14，ext 带已移除）。
func GatePass(f *FrameFeatures) bool {
	if f == nil || !f.Detected {
		return true // 无姿态数据的秒不误杀
	}
	return f.VisRatio >= 0.6 && f.FaceFrac <= 0.14
}

// SegmentGatePass 段级判定：≥50% 已知秒通过才保留（无姿态数据的秒不误杀）。
func SegmentGatePass(feats []*FrameFeatures) bool {
	known := 0
	ok := 0
	for _, f := range feats {
		if f == nil || !f.Detected {
			continue
		}
		known++
		if GatePass(f) {
			ok++
		}
	}
	if known == 0 {
		return true
	}
	return float64(ok)/float64(known) >= 0.5
}

func mean(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	s := 0.0
	for _, v := range x {
		s += v
	}
	return s / float64(len(x))
}

func minOf(x []float64) float64 {
	m := math.Inf(1)
	for _, v := range x {
		if v < m {
			m = v
		}
	}
	return m
}

func maxOf(x []float64) float64 {
	m := math.Inf(-1)
	for _, v := range x {
		if v > m {
			m = v
		}
	}
	return m
}

var _ = image.Rect // 保持 image 导入（关键点坐标语义按帧像素）
