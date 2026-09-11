package hashstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreSaveExists(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "h.db")
	s := New(p)
	s.Load()
	if s.Exists("abc") {
		t.Fatal("empty store should not contain abc")
	}
	s.Save("abc")
	if !s.Exists("abc") {
		t.Fatal("after Save, Exists should be true")
	}
	// 重新加载
	s2 := New(p)
	s2.Load()
	if !s2.Exists("abc") {
		t.Fatal("reload from disk should contain abc")
	}
}

func TestFileHashEmpty(t *testing.T) {
	if FileHash(filepath.Join(t.TempDir(), "nope")) != "" {
		t.Fatal("missing file should return empty hash")
	}
}

// Repath 让同一实例改写到新路径（启动时重定向到 config.dataDir），且不重建实例。
func TestStoreRepath(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.db")
	newPath := filepath.Join(dir, "data", "new.db")

	s := New(oldPath)
	s.Repath(newPath)
	s.Save("deadbeef")

	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("应写入新路径: %v", err)
	}
	if _, err := os.Stat(oldPath); err == nil {
		t.Fatal("不应再写旧路径")
	}
}

// 父目录不存在时应自动补建，而不是静默丢记录。
func TestStoreSaveCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "deep", "h.db")
	s := New(p)
	s.Save("cafebabe")

	if _, err := os.Stat(p); err != nil {
		t.Fatalf("应自动创建父目录并落盘: %v", err)
	}
}
