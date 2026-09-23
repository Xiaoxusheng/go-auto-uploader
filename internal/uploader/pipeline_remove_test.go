package uploader

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// mayRemove 必须把删除决定权交给 BeforeRemove；未注入时保持历史行为（放行）。
func TestMayRemoveHonoursHook(t *testing.T) {
	var asked []string
	p := &Pipeline{
		BeforeRemove: func(path string) bool {
			asked = append(asked, path)
			return path != "/keep.ts"
		},
	}
	if !p.mayRemove("/drop.ts") {
		t.Error("钩子返回 true 时应放行删除")
	}
	if p.mayRemove("/keep.ts") {
		t.Error("钩子返回 false 时不应删除")
	}
	if len(asked) != 2 {
		t.Fatalf("钩子应被调用 2 次，实际 %d 次", len(asked))
	}

	if !(&Pipeline{}).mayRemove("/anything.ts") {
		t.Error("未注入钩子时应保持历史行为：放行删除")
	}
}

// 集成：上传成功后，被 BeforeRemove 拦下的源文件必须留在盘上。
//
// 这是高光能读到原片的前提 —— 之前没有这道闸，上传完即删源，
// 高光 3 分钟后才来读，于是每个切片都以「No such file or directory」收场、产出恒为 0。
func TestHandleFileKeepsSourceWhenClaimed(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026-09-22")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(day, "a_001.mp4")
	if err := os.WriteFile(src, make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	uploaded := false
	p := &Pipeline{
		SafeBaseDir: "base",
		OnUpload: func(context.Context, string, string, int64) bool {
			uploaded = true
			return true
		},
		BeforeRemove: func(string) bool { return false }, // 高光认领
	}
	p.HandleFile(context.Background(), src, []string{root})

	if !uploaded {
		t.Fatal("文件应当被上传")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("被认领的源文件不应被删除: %v", err)
	}

	// 未认领时应当照旧删除，否则磁盘只增不减
	src2 := filepath.Join(day, "a_002.mp4")
	if err := os.WriteFile(src2, make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	p2 := &Pipeline{
		SafeBaseDir: "base",
		OnUpload:    func(context.Context, string, string, int64) bool { return true },
	}
	p2.HandleFile(context.Background(), src2, []string{root})
	if _, err := os.Stat(src2); !os.IsNotExist(err) {
		t.Fatalf("未认领的文件上传后应被删除，stat err = %v", err)
	}
}
