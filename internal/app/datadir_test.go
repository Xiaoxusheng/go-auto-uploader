package app

import (
	"os"
	"path/filepath"
	"testing"

	"upload/internal/config"
)

// ApplyDataDir 绝不能重建 store 实例。
// internal/bots 在包 init 阶段就捕获了 SuccessStore 的引用，一旦启动时替换全局指针，
// bots 会拿到永不加载的僵尸 store（趋势/排行数据全空，还可能往错误路径写文件）。
func TestApplyDataDirKeepsStoreIdentity(t *testing.T) {
	dir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	prev := CfgStore.Get()
	CfgStore.Replace(config.Config{DataDir: filepath.Join(dir, "d")})
	defer CfgStore.Replace(prev)

	succBefore := SuccessStore
	dirStatusBefore := DirStatusStore
	hashBefore := HashDB

	got := ApplyDataDir()

	if SuccessStore != succBefore {
		t.Fatal("SuccessStore 实例被替换了：bots 等提前捕获的引用会失效")
	}
	if DirStatusStore != dirStatusBefore {
		t.Fatal("DirStatusStore 实例被替换了")
	}
	if HashDB != hashBefore {
		t.Fatal("HashDB 实例被替换了")
	}
	if got == "" {
		t.Fatal("dataDir 不应为空")
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("数据目录应已创建: %v", err)
	}
}

// dataDir 缺省时应回落到 ./data。
func TestApplyDataDirDefault(t *testing.T) {
	dir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	prev := CfgStore.Get()
	CfgStore.Replace(config.Config{})
	defer CfgStore.Replace(prev)

	if got := ApplyDataDir(); got != "./data" {
		t.Fatalf("缺省 dataDir 应为 ./data, got %q", got)
	}
}
