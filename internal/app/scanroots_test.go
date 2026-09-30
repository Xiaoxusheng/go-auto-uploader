package app

import (
	"path/filepath"
	"testing"

	"upload/internal/config"
	"upload/internal/recorder"
)

// ScanRoots 必须把备选落盘目录并入扫描/归属根集合，且与 cfg.Dirs 去重——
// 否则备选目录里的录制文件会被 pipeline 按 NO_ROOT_MATCH 直接跳过。
func TestScanRootsMergesFallbacks(t *testing.T) {
	origStore, origRec := CfgStore, *recorder.Config()
	defer func() {
		CfgStore = origStore
		recorder.SetConfig(&origRec)
	}()

	CfgStore = config.NewStore("config.json")
	CfgStore.Update(func(c *config.Config) {
		c.Dirs = []string{"./downloads", "D:\\store", ""}
	})
	recorder.SetConfig(&recorder.BuiltinConfig{
		SavePath:          "D:\\primary",
		SavePathFallbacks: []string{"D:\\store", "D:\\fb1"},
	})

	got := ScanRoots()
	want := []string{"./downloads", "D:\\store", "D:\\fb1"}
	if len(got) != len(want) {
		t.Fatalf("ScanRoots = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ScanRoots[%d] = %q, want %q（全部=%v）", i, got[i], want[i], got)
		}
	}

	// 备选目录下的文件必须能被归属判定命中（原 NO_ROOT_MATCH 场景）
	p := filepath.Join("D:\\fb1", "主播", "2026-09-30", "a_001.ts")
	if got := DetectRoot(p); got != filepath.Clean("D:\\fb1") {
		t.Fatalf("DetectRoot(备选目录文件) = %q, want 备选根目录", got)
	}
}
