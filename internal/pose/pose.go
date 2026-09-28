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

// 预标默认门槛（§22 定标值）。
//
// ⚠️ 这是**预标（生成候选金标）**口径，刻意比生产门（config highlight_pose_gate：
// det 0.3 / vis 0.6 / face 0.12）宽松——预标要尽量覆盖，生产门要尽量精确，两者不是一套数。
//
// 单一真相来源：cmd/hleval review-ingest 的 flag 默认值、控制台「姿态训练」页的
// 窗口回放都引用这里，改一处即同步。
const (
	PrelabelDetMin  = 0.2  // 窗内姿态检出率下限（低于=无人/特效场）
	PrelabelVisMin  = 0.6  // 检出帧 vis 均值下限
	PrelabelFaceMax = 0.14 // 检出帧 face 均值上限
)

// WindowStats 8s 窗聚合结果（预标判定与可视化共用同一份聚合）。
type WindowStats struct {
	DetRate  float64 // 检出帧占窗内帧数比（0~1）
	VisMean  float64 // 检出帧 vis 均值（无检出=0）
	FaceMean float64 // 检出帧 face 均值（无检出=0）
	ExtMean  float64 // 检出帧 ext 均值（无检出=0）
	Label    string  // none / dance / closeup / other
}

// AggregateWindow 8s 窗预标判定（det-aware；纯计算、无 cgo 依赖，两种构建都可用）。
//
// ⚠️ 这是「自动预标」口径的单一真相来源：cmd/hleval review-ingest 落盘 spans 与
// 控制台「姿态训练」页的窗口可视化都走本函数。历史上两处各写一套（页面不带 ext 带、
// 落盘带 ext 带），导致页面显示的标签与 clips_config.json 实际写入的 spans 不一致。
//
// ext 带已移除（§22 定标：无判别力且砍召回）；detMin/visMin/faceMax 语义与线上
// GateOptions 一致。feats 为窗内逐秒特征，Detected=false 的秒不计入均值。
func AggregateWindow(feats []FrameFeatures, detMin, visMin, faceMax float64) WindowStats {
	var w WindowStats
	if len(feats) == 0 {
		w.Label = "none"
		return w
	}
	det := 0
	sumV, sumF, sumE := 0.0, 0.0, 0.0
	for i := range feats {
		if !feats[i].Detected {
			continue
		}
		det++
		sumV += feats[i].VisRatio
		sumF += feats[i].FaceFrac
		sumE += feats[i].ExtH
	}
	w.DetRate = float64(det) / float64(len(feats))
	if det > 0 {
		w.VisMean = sumV / float64(det)
		w.FaceMean = sumF / float64(det)
		w.ExtMean = sumE / float64(det)
	}
	switch {
	case det == 0 || w.DetRate < detMin:
		w.Label = "none" // 整窗无人 / 检出率过低（无人·特效·特写聊天场）
	case w.VisMean >= visMin && w.FaceMean <= faceMax:
		w.Label = "dance"
	case w.FaceMean > faceMax:
		w.Label = "closeup"
	default:
		w.Label = "other"
	}
	return w
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
