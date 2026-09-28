package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCmdCleanupSourcesRules 验证删除规则：池内+老+有帧 → 删；
// 未入池/新写/无帧/保护 → 保留。
func TestCmdCleanupSourcesRules(t *testing.T) {
	root := t.TempDir()
	dl := filepath.Join(root, "downloads")
	fr := filepath.Join(root, "frames")
	pd := filepath.Join(root, "protect")
	for _, d := range []string{dl, fr, pd} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(dir, name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// 帧：只有 clipA 有（clipB 无帧、clipC 未入池无帧）
	os.MkdirAll(filepath.Join(fr, "clipA"), 0o755)
	for i := 1; i <= ueMinFrames; i++ {
		mk(filepath.Join(fr, "clipA"), sprintf04d(i), time.Hour)
	}
	// 源片
	pA := mk(dl, "clipA.ts", 2*time.Hour) // 池内+老+有帧 → 删
	pB := mk(dl, "clipB.ts", 2*time.Hour) // 池内但无帧 → 留
	pC := mk(dl, "clipC.ts", 2*time.Hour) // 未入池 → 留
	pD := mk(dl, "clipD.ts", time.Minute) // 池内但新写 → 留
	mk(pd, "clipE.ts", 2*time.Hour)       // 保护副本
	pE := mk(dl, "clipE.ts", 2*time.Hour) // downloads 里的保护命中副本 → 留
	cfg := filepath.Join(root, "clips_config.json")
	os.WriteFile(cfg, []byte(`[
		{"clip":"clipA"},{"clip":"clipB"},{"clip":"clipD"},
		{"clip":"clipE"},{"clip":"clipC"}]`), 0o644)

	logP := filepath.Join(root, "audit.log")
	cmdCleanupSources([]string{
		"-downloads", dl, "-config", cfg, "-frames", fr,
		"-protect-dirs", pd, "-min-age-hours", "1", "-log", logP,
	})

	if _, err := os.Stat(pA); !os.IsNotExist(err) {
		t.Fatalf("clipA 应已删除")
	}
	for _, keep := range []string{pB, pC, pD, pE} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("%s 应保留", keep)
		}
	}
	if n, _ := filepath.Glob(filepath.Join(pd, "*.ts")); len(n) != 1 {
		t.Fatal("保护目录不应被触碰")
	}
	b, _ := os.ReadFile(logP)
	if !strings.Contains(string(b), "clipA.ts") {
		t.Fatal("审计日志应记录 clipA")
	}
}

func sprintf04d(n int) string { return "f_" + strings.Repeat("0", 4-len(itoa4(n))) + itoa4(n) + ".jpg" }

func itoa4(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
