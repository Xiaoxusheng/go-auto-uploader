package uploader

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPreparePath(t *testing.T) {
	p := &Pipeline{SafeBaseDir: "/home/_safe_uploads"}
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "主播"), 0755)
	file := filepath.Join(root, "主播", "a.ts")
	_ = os.WriteFile(file, []byte("x"), 0644)
	name, remote, ok := p.PreparePath(file, root)
	if !ok || name == "" || remote == "" {
		t.Fatalf("prepare %v %q %q", ok, name, remote)
	}
}

func TestHandleFileZeroByte(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "z.ts")
	_ = os.WriteFile(f, nil, 0644)
	p := &Pipeline{}
	p.HandleFile(context.Background(), f, []string{dir})
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatal("zero byte should be removed")
	}
}

func TestHandleFileSkipArtifact(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.mp4.part")
	_ = os.WriteFile(f, []byte("x"), 0644)
	p := &Pipeline{}
	p.HandleFile(context.Background(), f, []string{dir})
	if _, err := os.Stat(f); err != nil {
		t.Fatal("artifact should be left alone")
	}
}
