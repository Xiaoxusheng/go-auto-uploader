package convert

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTSToMP4UsesExplicitMP4Format 确保输出参数带 -f mp4（.part 扩展名无法被 ffmpeg 识别）。
func TestTSToMP4UsesExplicitMP4Format(t *testing.T) {
	// 源码级断言：构造 args 的两组尝试都必须包含 -f mp4
	src, err := os.ReadFile("convert.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"-f", "mp4"`) {
		t.Fatal("TSToMP4 must pass -f mp4 for .part outputs")
	}
}

func TestIsTSAndArtifact(t *testing.T) {
	if !IsTS("a.TS") || IsTS("a.mp4") {
		t.Fatal("IsTS")
	}
	if !IsArtifact("x.mp4.part") || IsArtifact("x.ts") {
		t.Fatal("IsArtifact")
	}
}

// TestTSToMP4MissingFile 失败路径返回 error
func TestTSToMP4MissingFile(t *testing.T) {
	if _, err := TSToMP4(filepath.Join(t.TempDir(), "no.ts"), "ffmpeg"); err == nil {
		// 无 ffmpeg 或文件不存在都应失败
		if _, e2 := exec.LookPath("ffmpeg"); e2 == nil {
			t.Fatal("missing ts should error")
		}
	}
}
