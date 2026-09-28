package recorder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// syncBuiltinAnchorToTxt 以进程 CWD 下的 builtin_urls.txt 为操作对象，
// 测试前切换到临时目录，避免碰真实名单。
func chdirToTempTxt(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取工作目录失败: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("切换临时目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	if content != "" {
		if err := os.WriteFile("builtin_urls.txt", []byte(content), 0644); err != nil {
			t.Fatalf("写入名单失败: %v", err)
		}
	}
}

func readTempTxt(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("builtin_urls.txt"))
	if err != nil {
		t.Fatalf("读取名单失败: %v", err)
	}
	return string(b)
}

// 回归：对名单里已存在（尤其暂停态）的主播执行 add 时，原行必须保留并转为运行态。
//
// 曾经的故障：syncBuiltinAnchorToTxt 的 add 分支对已存在的行没有任何写回动作，
// 命中 found 后原行被整体丢弃——在控制台里重复添加一个暂停中的主播，
// 会把 builtin_urls.txt 里的名单行删掉，进程重启后该主播凭空消失。
func TestAddExistingPausedAnchorKeepsLine(t *testing.T) {
	chdirToTempTxt(t, "#https://live.douyin.com/875770026751,主播:小妤,录屏:1,截屏:1\n"+
		"https://live.douyin.com/999,主播:别人\n")

	syncBuiltinAnchorToTxt("add", "Douyin", "875770026751", "https://live.douyin.com/875770026751")

	out := readTempTxt(t)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("应当保留 2 行，实际 %d 行: %q", len(lines), out)
	}
	if !strings.Contains(out, "875770026751") {
		t.Fatalf("已存在的主播行被丢弃了: %q", out)
	}
	if strings.Contains(out, "#https://live.douyin.com/875770026751") {
		t.Fatalf("add 语义应当解除暂停（与 API 已启动监控的内存态一致），仍是暂停行: %q", out)
	}
	if !strings.Contains(out, "主播:小妤") || !strings.Contains(out, "录屏:1") {
		t.Fatalf("自定义名称与开关后缀应当原样保留: %q", out)
	}
	if !strings.Contains(out, "https://live.douyin.com/999") {
		t.Fatalf("无关行不应受影响: %q", out)
	}
}

// 名单里不存在时，add 维持原语义：追加原始行。
func TestAddNewAnchorAppendsLine(t *testing.T) {
	chdirToTempTxt(t, "https://live.douyin.com/999,主播:别人\n")

	syncBuiltinAnchorToTxt("add", "Douyin", "123", "https://live.douyin.com/123,主播:新人")

	out := readTempTxt(t)
	if !strings.Contains(out, "https://live.douyin.com/123,主播:新人") {
		t.Fatalf("新主播行未被追加: %q", out)
	}
}
