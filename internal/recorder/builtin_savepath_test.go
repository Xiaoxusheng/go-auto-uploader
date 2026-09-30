package recorder

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const gbUnit int64 = 1 << 30

// 存储溢出护栏决策矩阵：未启用 / 主目录健康 / 切换剩余最大备选 / 全不足维持 / 恢复切回。
func TestResolveRecordRoot(t *testing.T) {
	origFn, origHook, origCfg := freeSpaceFn, AlertHook, *Config()
	defer func() {
		freeSpaceFn, AlertHook = origFn, origHook
		SetConfig(&origCfg)
	}()

	root := t.TempDir()
	primary := filepath.Join(root, "primary")
	fbA := filepath.Join(root, "fbA")
	fbB := filepath.Join(root, "fbB")
	fbC := filepath.Join(root, "fbC") // 不预建：验证切换时自动创建
	for _, d := range []string{primary, fbA, fbB} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	free := map[string]int64{}
	freeSpaceFn = func(p string) int64 { return free[filepath.Clean(p)] }

	storageAlertMu.Lock()
	storageAlertLast = map[string]time.Time{}
	storageAlertMu.Unlock()
	alerts := 0
	AlertHook = func(_, _, _ string) { alerts++ }

	newCfg := func(fbs []string) {
		SetConfig(&BuiltinConfig{SavePath: primary, SavePathFallbacks: fbs, MinFreeGB: 10})
	}

	// 1) 未配置备选目录 → 功能关闭，恒主目录（即使主目录已满）
	free[primary] = 0
	newCfg(nil)
	if got := ResolveRecordRoot(); got != primary {
		t.Fatalf("未配置备选时应返回主目录, got %s", got)
	}

	// 2) 主目录健康 → 主目录（主目录优先，天然无抖动）
	free[primary] = 20 * gbUnit
	newCfg([]string{fbA, fbB})
	if got := ResolveRecordRoot(); got != primary {
		t.Fatalf("主目录健康时应返回主目录, got %s", got)
	}

	// 3) 主目录不足 → 选剩余空间最大且达标的备选，并告警一次
	free[primary] = 1 * gbUnit
	free[fbA] = 15 * gbUnit
	free[fbB] = 50 * gbUnit
	if got := ResolveRecordRoot(); got != fbB {
		t.Fatalf("主目录不足时应选剩余最大的备选 fbB, got %s", got)
	}
	if alerts != 1 {
		t.Fatalf("首次切换应告警 1 次, got %d", alerts)
	}

	// 4) 备选全部不足阈值 → 维持主目录，不乱找
	free[fbA] = 1 * gbUnit
	free[fbB] = 2 * gbUnit
	if got := ResolveRecordRoot(); got != primary {
		t.Fatalf("备选均不足时应维持主目录, got %s", got)
	}
	if alerts != 2 {
		t.Fatalf("溢出告警应再发 1 次, got %d", alerts)
	}

	// 5) 主目录恢复 → 自动切回
	free[primary] = 20 * gbUnit
	if got := ResolveRecordRoot(); got != primary {
		t.Fatalf("主目录恢复后应切回主目录, got %s", got)
	}

	// 6) 切换到未创建的备选目录 → 自动建目录（含主播子目录的父级就绪）
	free[primary] = 1 * gbUnit
	free[fbC] = 80 * gbUnit
	newCfg([]string{fbC})
	if got := ResolveRecordRoot(); got != fbC {
		t.Fatalf("应切换到备选 fbC, got %s", got)
	}
	if st, err := os.Stat(fbC); err != nil || !st.IsDir() {
		t.Fatalf("切换时应自动创建备选目录 %s", fbC)
	}
}

// FallbackRoots 归一化：剔除与主目录重复项（避免主目录被当备选参与选择）。
func TestFallbackRootsNormalization(t *testing.T) {
	origCfg := *Config()
	defer SetConfig(&origCfg)

	primary := filepath.Join(t.TempDir(), "p")
	SetConfig(&BuiltinConfig{
		SavePath:          primary,
		SavePathFallbacks: []string{primary, "  ", filepath.Join(primary, "..", "p"), "D:\\fb"},
	})
	// filepath.Join(primary, "..", "p") Clean 后与 primary 相同 → 剔除
	got := FallbackRoots()
	if len(got) != 1 || got[0] != filepath.Clean("D:\\fb") {
		t.Fatalf("FallbackRoots = %v, want [D:\\fb]", got)
	}
	if roots := RecordRoots(); len(roots) != 2 || roots[0] != getBuiltinSavePath() {
		t.Fatalf("RecordRoots 应以主目录开头, got %v", roots)
	}
}
