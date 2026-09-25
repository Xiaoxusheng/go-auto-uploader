//go:build cgo
package pose
import (
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)
const (
	testDLL   = `D:/upload/_vendor/onnxruntime/onnxruntime-win-x64-1.30.0/lib/onnxruntime.dll`
	testModel = `D:/upload/_vendor/yolov8n-pose.onnx`
	testFrame = `D:/upload/_diag/train/_pose_pilot/frames`
)
func newTestDetector(t *testing.T) *Detector {
	t.Helper()
	for _, p := range []string{testDLL, testModel} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("依赖缺失 %s（onnxruntime/模型未就绪）", p)
		}
	}
	d, err := NewDetector(testDLL, testModel)
	if err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}
func loadImage(t *testing.T, p string) image.Image {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("打开帧失败 %s: %v", p, err)
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	return img
}
// 冒烟 + 语义方向验证（多帧投票，避免单帧硬帧干扰）：
// 舞蹈片（卷卷，全身）多数帧应检出且 vis_ratio 高、face 小；
// 近景聊天片（VVya，脸占满）多数帧 face_frac 更大 / vis 更低。
func TestDetectSmokeAndSemantics(t *testing.T) {
	d := newTestDetector(t)
	type sample struct {
		clip string
		secs []int
	}
	run := func(s sample) (nDet, n int, vis, face []float64) {
		for _, sec := range s.secs {
			p := filepath.Join(testFrame, s.clip, fmt.Sprintf("f_%04d.jpg", sec+1))
			if _, err := os.Stat(p); err != nil {
				continue
			}
			img := loadImage(t, p)
			fp, err := d.DetectImage(img)
			if err != nil {
				t.Fatalf("%s@%ds: %v", s.clip, sec, err)
			}
			ff := FeaturesFromFrame(fp, img.Bounds().Dx(), img.Bounds().Dy())
			n++
			if ff.Detected {
				nDet++
				vis = append(vis, ff.VisRatio)
				face = append(face, ff.FaceFrac)
			}
		}
		return
	}
	meanOf := func(x []float64) float64 {
		if len(x) == 0 {
			return 0
		}
		s := 0.0
		for _, v := range x {
			s += v
		}
		return s / float64(len(x))
	}
	secs := []int{8, 24, 40, 56, 72, 88, 104, 120, 136, 152, 168, 184}
	nDet, n, vis, face := run(sample{"卷卷卷上头_2026-09-24_12-53-36_001", secs})
	t.Logf("舞蹈片: 检出 %d/%d vis=%.3f face=%.3f", nDet, n, meanOf(vis), meanOf(face))
	if nDet*2 <= n {
		t.Fatalf("舞蹈片检出率过低: %d/%d", nDet, n)
	}
	if meanOf(vis) < 0.3 || meanOf(face) > 0.1 {
		t.Fatalf("舞蹈片语义异常 vis=%.3f face=%.3f", meanOf(vis), meanOf(face))
	}
	nDet2, n2, vis2, face2 := run(sample{"VVya_2026-09-25_00-28-20_000", secs})
	t.Logf("近景聊天片: 检出 %d/%d vis=%.3f face=%.3f", nDet2, n2, meanOf(vis2), meanOf(face2))
	if meanOf(face2) <= meanOf(face) {
		t.Fatalf("近景片 face 应大于舞蹈片: %.3f vs %.3f", meanOf(face2), meanOf(face))
	}
}
// 批量对齐：对一批帧输出 Go 特征 JSON，供与 MediaPipe 版对比（M3）。
func TestBatchAlignDump(t *testing.T) {
	d := newTestDetector(t)
	clips := []struct {
		clip string
		secs []int
	}{
		{"卷卷卷上头_2026-09-24_12-53-36_001", []int{0, 40, 80, 120, 160, 200, 240, 280, 320, 360, 400, 440}},
		{"VVya_2026-09-25_00-28-20_000", []int{0, 40, 80, 120, 160, 200, 240, 280, 320, 360, 400, 440}},
		{"清清荷子🪷_2026-09-24_17-06-02_000", []int{0, 40, 80, 120, 160, 200, 240, 280, 320, 360, 400, 440}},
	}
	out := map[string][]*FrameFeatures{}
	for _, c := range clips {
		for _, sec := range c.secs {
			p := filepath.Join(testFrame, c.clip, fmt.Sprintf("f_%04d.jpg", sec+1))
			if _, err := os.Stat(p); err != nil {
				continue
			}
			img := loadImage(t, p)
			fp, err := d.DetectImage(img)
			if err != nil {
				t.Fatalf("%s@%ds: %v", c.clip, sec, err)
			}
			out[fmt.Sprintf("%s@%d", c.clip, sec)] = append(out[fmt.Sprintf("%s@%d", c.clip, sec)],
				FeaturesFromFrame(fp, img.Bounds().Dx(), img.Bounds().Dy()))
		}
	}
	b, _ := json.MarshalIndent(out, "", " ")
	t.Logf("对齐导出 %d 项", len(out))
	_ = b
}
