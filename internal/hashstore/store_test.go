package hashstore

import (
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
