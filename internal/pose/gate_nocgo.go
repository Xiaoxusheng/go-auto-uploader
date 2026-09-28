//go:build !cgo

// 纯 Go 构建下姿态门不可用：段照常保留（门自动失效，行为与关闭一致）。
package pose

// GateOptions 段级过滤参数（保留类型，便于调用点两种构建同构）。
type GateOptions struct {
	DetMin float64
	// FPS 段内抽帧率（纯 Go 构建不抽帧，字段仅为调用点同构保留）。默认 5。
	FPS       int
	VisMin    float64
	FaceMax   float64
	KeepRatio float64
	// 学习型门头段级投票（仅 cgo 构建生效；非 cgo 门为直通，字段仅保编译一致）。
	HeadEnable    bool
	HeadModelPath string
	HeadFrac      float64
}

// GatePassWith 纯 Go 构建恒放行。
func GatePassWith(f *FrameFeatures, o GateOptions) bool { return true }

// FilterSegments 纯 Go 构建恒放行全部段。
func FilterSegments(ffmpegBin, src string, segs [][2]int, o GateOptions,
	dllPath, modelPath string) (kept [][2]int, dropped int, err error) {
	return segs, 0, nil
}
