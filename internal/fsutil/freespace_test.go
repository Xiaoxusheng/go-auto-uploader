package fsutil

import (
	"path/filepath"
	"testing"
)

// FreeSpace 正常路径返回正数；不存在的路径返回 0（护栏逻辑依赖 0 = 不可用语义）。
func TestFreeSpace(t *testing.T) {
	dir := t.TempDir()
	if got := FreeSpace(dir); got <= 0 {
		t.Fatalf("FreeSpace(临时目录) = %d, want > 0", got)
	}
	if got := FreeSpace(filepath.Join(dir, "不存在的子目录")); got != 0 {
		t.Fatalf("FreeSpace(不存在路径) = %d, want 0", got)
	}
	if got := FreeSpace(""); got <= 0 {
		t.Fatalf("FreeSpace(空串回落当前目录) = %d, want > 0", got)
	}
}
