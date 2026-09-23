package highlight

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEndToEndRealClip 用真实录像跑通 Probe → Score → Select → Cut 全链路。
//
// 需要设置环境变量才执行，避免在无素材/无 ffmpeg 的环境里失败：
//
//	HIGHLIGHT_E2E_FFMPEG=/path/to/ffmpeg
//	HIGHLIGHT_E2E_SRC=/path/to/clip.mp4
//
// 预期素材是含明显跳舞段的直播切片（实测 15 分钟切片约跑 3 分钟）。
func TestEndToEndRealClip(t *testing.T) {
	ffmpegBin := os.Getenv("HIGHLIGHT_E2E_FFMPEG")
	src := os.Getenv("HIGHLIGHT_E2E_SRC")
	if ffmpegBin == "" || src == "" {
		t.Skip("未设置 HIGHLIGHT_E2E_FFMPEG / HIGHLIGHT_E2E_SRC，跳过端到端测试")
	}
	if _, err := os.Stat(src); err != nil {
		t.Skipf("素材不存在，跳过: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	opts := DefaultOptions()
	start := time.Now()
	series, err := Probe(ctx, ffmpegBin, src, opts.Threads)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if series.Len() == 0 {
		t.Fatal("未取到任何采样点")
	}
	t.Logf("采样 %d 秒，耗时 %s（%.1fx 实时）",
		series.Len(), time.Since(start).Truncate(time.Second),
		float64(series.Len())/time.Since(start).Seconds())

	segs := Select(Score(series, opts), opts)
	if len(segs) == 0 {
		t.Fatalf("未检出高光段，但素材应含明显跳舞段")
	}
	total := 0
	for _, s := range segs {
		total += s.Duration()
		t.Logf("高光段 %ds-%ds（%ds，均分 %.2f）", s.Start, s.End, s.Duration(), s.Score)
	}
	t.Logf("共 %d 段，合计 %ds", len(segs), total)

	out := filepath.Join(t.TempDir(), "highlight_e2e.mp4")
	if err := Cut(ctx, ffmpegBin, src, out, segs); err != nil {
		t.Fatalf("Cut 失败: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() == 0 {
		t.Fatalf("裁切产物无效: %v", err)
	}
	t.Logf("裁切产物 %d 字节（约 %.1f MB）", info.Size(), float64(info.Size())/1024/1024)
}
