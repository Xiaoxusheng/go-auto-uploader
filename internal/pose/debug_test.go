//go:build cgo
package pose
import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)
func TestDebugVVya(t *testing.T) {
	d := newTestDetector(t)
	for _, clip := range []string{"VVya_2026-09-25_00-28-20_000", "卷卷卷上头_2026-09-24_12-53-36_001"} {
		for _, sec := range []int{96, 100, 104} {
			p := filepath.Join(testFrame, clip, fmt.Sprintf("f_%04d.jpg", sec+1))
			img := loadImage(t, p)
			b := img.Bounds()
			fp, err := d.DetectImage(img)
			if err != nil {
				t.Fatalf("%v", err)
			}
			t.Logf("%s f_%04d dims=%dx%d detected=%v conf=%.3f kpt0=%v",
				clip[:8], sec+1, b.Dx(), b.Dy(), fp.Detected, fp.Conf, fp.Kpts[0])
		}
	}
	_ = os.Stdout
}
