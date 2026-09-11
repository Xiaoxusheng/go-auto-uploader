package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Repath 必须让「同一个实例」改写到新路径。
// 这条约定很关键：internal/bots 在包 init 阶段就捕获了 SuccessStore 的引用，
// 如果启动时改为重建实例，bots 会拿到一个永不加载、路径也错的僵尸 store。
func TestSuccessStoreRepath(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.json")
	newPath := filepath.Join(dir, "data", "new.json")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatal(err)
	}

	s := NewSuccessStore(oldPath, 100)
	identity := s
	s.Repath(newPath)

	if identity != s {
		t.Fatal("Repath 不应重建实例")
	}

	s.Add(UploadRecord{Time: time.Now(), Streamer: "主播", Name: "f.ts", Size: 1024})
	s.Flush()

	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("应写入新路径: %v", err)
	}
	if _, err := os.Stat(oldPath); err == nil {
		t.Fatal("不应再写旧路径")
	}

	s2 := NewSuccessStore(newPath, 100)
	s2.Load()
	if s2.Len() != 1 {
		t.Fatalf("新路径应有 1 条记录, got %d", s2.Len())
	}
}

func TestDirStatusStoreRepath(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.json")
	newPath := filepath.Join(dir, "data", "ds.json")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatal(err)
	}

	s := NewDirStatusStore(oldPath)
	s.Repath(newPath)
	s.Put("/root", &DirStatus{Path: "/root", TotalFiles: 3})
	s.Flush()

	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("应写入新路径: %v", err)
	}
	if _, err := os.Stat(oldPath); err == nil {
		t.Fatal("不应再写旧路径")
	}
}
