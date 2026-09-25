//go:build !cgo

// 纯 Go 构建下的姿态桩：ONNX 推理不可用，姿态门自动关闭。
package pose

import "errors"

// Detector 桩：NewDetector 恒返回错误。
type Detector struct{}

// NewDetector 纯 Go 构建不支持 ONNX 推理。
func NewDetector(dllPath, modelPath string) (*Detector, error) {
	return nil, errors.New("pose: 当前构建未启用 cgo，姿态门不可用（需 CGO_ENABLED=1 重新构建）")
}

// Close 空实现。
func (d *Detector) Close() error { return nil }

// DetectFile 空实现。
func (d *Detector) DetectFile(path string) (*FramePose, error) {
	return nil, errors.New("pose: 当前构建未启用 cgo")
}

// DetectImage 空实现。
func (d *Detector) DetectImage(img interface{}) (*FramePose, error) {
	return nil, errors.New("pose: 当前构建未启用 cgo")
}
