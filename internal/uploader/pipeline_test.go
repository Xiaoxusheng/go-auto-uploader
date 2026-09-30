package uploader

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"upload/internal/hashstore"
	"upload/internal/storage"
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

// newDirStatus 建一个带 1 个 Pending 的目录状态，返回 store 便于断言。
func newDirStatus(t *testing.T, root string) *storage.DirStatusStore {
	t.Helper()
	store := storage.NewDirStatusStore(filepath.Join(t.TempDir(), "dir_status.json"))
	store.Put(root, &storage.DirStatus{Path: root, PendingFiles: 1})
	return store
}

// 秒传 = 远端已有同哈希内容：不得重复累计 UploadedFiles/UploadedSize，
// 但 Pending 名额要回收，否则目录卡的「待处理」永远清不掉。
func TestHandleFileInstantUploadDoesNotRecount(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026-09-29")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(day, "a_001.ts")
	const payload = 3 << 20
	if err := os.WriteFile(src, make([]byte, payload), 0o644); err != nil {
		t.Fatal(err)
	}

	hashDB := hashstore.New(filepath.Join(t.TempDir(), "hash.db"))
	hashDB.Save(hashstore.FileHash(src))

	store := newDirStatus(t, root)
	p := &Pipeline{SafeBaseDir: "base", HashDB: hashDB, DirStatus: store}
	p.HandleFile(context.Background(), src, []string{root})

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("秒传完成后源文件应被删除: %v", err)
	}
	ds, ok := store.Get(root)
	if !ok {
		t.Fatal("目录状态应已存在")
	}
	ds.Mu.RLock()
	defer ds.Mu.RUnlock()
	if ds.UploadedFiles != 0 || ds.UploadedSize != 0 {
		t.Errorf("秒传不应重复累计: files=%d size=%d", ds.UploadedFiles, ds.UploadedSize)
	}
	if ds.PendingFiles != 0 {
		t.Errorf("Pending 名额应被回收，实际 %d", ds.PendingFiles)
	}
}

// 真实上传完成：正常累计 UploadedFiles/UploadedSize。
func TestHandleFileRealUploadCounts(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026-09-29")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(day, "b_001.ts")
	if err := os.WriteFile(src, make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	store := newDirStatus(t, root)
	p := &Pipeline{
		SafeBaseDir: "base",
		DirStatus:   store,
		OnUpload:    func(context.Context, string, string, int64) bool { return true },
	}
	p.HandleFile(context.Background(), src, []string{root})

	ds, ok := store.Get(root)
	if !ok {
		t.Fatal("目录状态应已存在")
	}
	ds.Mu.RLock()
	defer ds.Mu.RUnlock()
	if ds.UploadedFiles != 1 {
		t.Errorf("真实上传应累计文件数，实际 %d", ds.UploadedFiles)
	}
	if ds.UploadedSize != 2<<20 {
		t.Errorf("真实上传应累计体积，实际 %d", ds.UploadedSize)
	}
	if ds.PendingFiles != 0 {
		t.Errorf("Pending 名额应被回收，实际 %d", ds.PendingFiles)
	}
}
