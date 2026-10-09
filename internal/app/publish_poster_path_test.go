package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPublishPosterForRejectsNonHighlightTarget 回归：PublishPosterFor 的 id 直接来自
// URL 查询串（?id=<产物绝对路径>），未命中投稿队列时不得原样交给 ffmpeg 抽帧——
// 否则任意登录用户可用 ?id=/任意路径/x.mp4 读取服务器上任意 mp4/ts（路径穿越 / 任意文件读取）。
func TestPublishPosterForRejectsNonHighlightTarget(t *testing.T) {
	resetPublishForTest(t)

	// 真实存在、扩展名合法的 mp4，但它不是本系统的高光产物
	outside := filepath.Join(t.TempDir(), "outside.mp4")
	if err := os.WriteFile(outside, []byte("not a highlight"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isHighlightOutputPath(outside) {
		t.Fatalf("非高光产物不应通过产物校验: %s", outside)
	}
	if _, err := PublishPosterFor(outside, 1); err == nil || !strings.Contains(err.Error(), "预览目标不存在") {
		t.Fatalf("非高光产物应被校验层拒绝（防路径穿越），实际错误: %v", err)
	}

	// 各类穿越输入同样必须在校验层被拦下，绝不能落到 ffmpeg
	for _, id := range []string{
		"../../etc/passwd.mp4",
		"/etc/shadow.ts",
		`C:\Windows\win.ini`,
		"",
	} {
		if _, err := PublishPosterFor(id, 1); err == nil || !strings.Contains(err.Error(), "预览目标不存在") {
			t.Fatalf("穿越输入 %q 应被拒绝，实际: %v", id, err)
		}
	}
}

// TestIsHighlightOutputPathMatchesProducedClip 正向：真实高光产物必须被识别，
// 否则候选列表的封面会全部 404。
func TestIsHighlightOutputPathMatchesProducedClip(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "主播A", "2026-10-09")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "主播A_2026-10-09_10-00-00_000.ts")
	if err := os.WriteFile(src, []byte("src"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := makeHighlightFixture(t, root, "主播A", "2026-10-09", "10-00-00")
	highlightStateSet(src, highlightEntry{Output: filepath.Base(out)})

	if !isHighlightOutputPath(out) {
		t.Fatalf("已产出的高光产物应被识别: %s", out)
	}
	missing := filepath.Join(dir, "高光", "不存在_highlight.mp4")
	if isHighlightOutputPath(missing) {
		t.Fatalf("不存在的产物不应被识别: %s", missing)
	}
}
