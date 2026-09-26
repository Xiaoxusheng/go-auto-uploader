package httpapi

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPoseClipStreamer(t *testing.T) {
	cases := map[string]string{
		"闲闲饭。_2026-09-25_20-38-15_004":         "闲闲饭。",
		"D.an（9月26生日）_2026-09-26_09-14-32_000": "D.an（9月26生日）",
		"颍颍呐🍒_2026-09-25_15-37-56_003":         "颍颍呐🍒",
		"名_含_下划线_2026-09-25_20-38-15_004":      "名_含_下划线",
		"无日期片名":                                "",
		"缺口段_2026-09-25_004":                   "",
	}
	for clip, want := range cases {
		if got := poseClipStreamer(clip); got != want {
			t.Errorf("poseClipStreamer(%q) = %q, want %q", clip, got, want)
		}
	}
}

func TestPoseCurrentClip(t *testing.T) {
	old := poseTrainRoot
	t.Cleanup(func() { poseTrainRoot = old })
	root := t.TempDir()
	poseTrainRoot = root

	if _, mt, n := poseCurrentClip(); !mt.IsZero() || n != 0 {
		t.Errorf("无 frames 目录应返回零值, got mt=%v n=%d", mt, n)
	}

	frames := filepath.Join(root, "_pose_pilot", "frames")
	a := filepath.Join(frames, "爱喝旺仔_2026-09-26_09-06-59_000")
	b := filepath.Join(frames, "爱喝旺仔_2026-09-26_09-08-50_000")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	if err := os.Chtimes(a, base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(b, base.Add(time.Minute), base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	name, mt, _ := poseCurrentClip()
	if name != filepath.Base(b) || mt.IsZero() {
		t.Errorf("应取 mtime 最新的片目录 %q, got %q mt=%v", filepath.Base(b), name, mt)
	}
	// 已抽帧数：往 b 里放两帧
	for _, f := range []string{"f_0001.jpg", "f_0002.jpg"} {
		if err := os.WriteFile(filepath.Join(b, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, n := poseCurrentClip(); n != 2 {
		t.Errorf("已抽帧数应为 2, got %d", n)
	}
}

func TestPoseQueueHead(t *testing.T) {
	dl := t.TempDir()
	mk := func(name string, age time.Duration) {
		p := filepath.Join(dl, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	mk("b主播_2026-09-25_10-00-00_000.ts", 2*time.Hour) // 次旧
	mk("a主播_2026-09-25_09-00-00_000.ts", 3*time.Hour) // 最旧 → 队头
	mk("高光_2026-09-25_11-00-00_000.ts", 1*time.Hour)  // 高光产物排除
	mk("已入池_2026-09-25_08-00-00_000.ts", 4*time.Hour) // configured 排除
	configured := map[string]bool{"已入池_2026-09-25_08-00-00_000": true}

	remaining, head := poseQueueHead(dl, configured, 6)
	if remaining != 2 {
		t.Errorf("剩余应为 2, got %d", remaining)
	}
	if len(head) != 2 || head[0]["clip"] != "a主播_2026-09-25_09-00-00_000" || head[1]["clip"] != "b主播_2026-09-25_10-00-00_000" {
		t.Errorf("队头应按最旧在前, got %v", head)
	}
	if head[0]["streamer"] != "a主播" {
		t.Errorf("队头应带主播名, got %v", head[0]["streamer"])
	}
	// n 截断
	if _, head2 := poseQueueHead(dl, configured, 1); len(head2) != 1 {
		t.Errorf("n=1 应截断为 1, got %d", len(head2))
	}
}
