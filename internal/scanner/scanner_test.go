package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, path string, size int, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestScanFilters(t *testing.T) {
	dir := t.TempDir()
	// 可上传
	write(t, filepath.Join(dir, "ok.ts"), 100, 5*time.Minute)
	// 刚写入 → active
	write(t, filepath.Join(dir, "fresh.ts"), 100, 10*time.Second)
	// 0 字节 → 删除
	write(t, filepath.Join(dir, "zero.ts"), 0, 5*time.Minute)
	// 中间产物
	write(t, filepath.Join(dir, "half.mp4.part"), 50, 5*time.Minute)

	var zero, artifact, active int
	var files int
	res := Scan(context.Background(), Options{
		Dirs: []string{dir},
		IsQueued: func(string) bool { return false },
		OnFile:   func(_, _ string, _ int64) { files++ },
		OnZeroByte: func(string) { zero++ },
		OnArtifact: func(string) { artifact++ },
		OnActive:   func(string) { active++ },
	})

	if res.Active != 1 || active != 1 {
		t.Fatalf("active=%d want 1", res.Active)
	}
	if zero != 1 {
		t.Fatalf("zero=%d", zero)
	}
	if artifact != 1 {
		t.Fatalf("artifact=%d", artifact)
	}
	if files != 1 {
		t.Fatalf("onFile=%d", files)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].Size != 100 {
		t.Fatalf("candidates=%v", res.Candidates)
	}
	if _, err := os.Stat(filepath.Join(dir, "zero.ts")); !os.IsNotExist(err) {
		t.Fatal("zero byte file should be removed")
	}
}

func TestScanIsQueuedSkipsCandidate(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.ts"), 10, 5*time.Minute)
	res := Scan(context.Background(), Options{
		Dirs:     []string{dir},
		IsQueued: func(string) bool { return true },
		OnFile:   func(_, _ string, _ int64) {},
	})
	if len(res.Candidates) != 0 {
		t.Fatal("queued path should not be candidate")
	}
}
