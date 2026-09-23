package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 高光参数的默认值回落逻辑有一个容易踩的边界：音频权重**显式设为 0** 是合法且
// 推荐的配置（真留一切片实证：0.8/0.2 th1.5 → F1 0.584；1.0/0.0 th1.2 → F1 0.717，
// 音频特征在所有 AUC 排名里垫底且方向相反）。如果哪天有人把回落条件改成
// `||` 或按单个字段判空，这个配置会被静默改回 0.8/0.2 —— 准确率悄悄退化且不报错。
// 这两条测试就是钉住这个契约。

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHighlightAudioWeightExplicitZeroIsKept(t *testing.T) {
	p := writeCfg(t, `{"builtin":{
		"highlight_motion_weight":1.0,
		"highlight_audio_weight":0.0,
		"highlight_threshold":1.2}}`)

	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Builtin.HighlightMotionW; got != 1.0 {
		t.Errorf("运动量权重 = %v, 期望 1.0", got)
	}
	if got := c.Builtin.HighlightAudioW; got != 0.0 {
		t.Errorf("音频权重 = %v, 期望 0.0（显式 0 不得被默认值覆盖）", got)
	}
	if got := c.Builtin.HighlightThreshold; got != 1.2 {
		t.Errorf("阈值 = %v, 期望 1.2", got)
	}
}

func TestHighlightWeightsFallbackOnlyWhenBothZero(t *testing.T) {
	// 两个权重都没写 → 视为未设置，回落 0.8/0.2
	c, err := Load(writeCfg(t, `{"builtin":{}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Builtin.HighlightMotionW != 0.8 || c.Builtin.HighlightAudioW != 0.2 {
		t.Errorf("未设置时 = %v/%v, 期望 0.8/0.2",
			c.Builtin.HighlightMotionW, c.Builtin.HighlightAudioW)
	}
	if c.Builtin.HighlightThreshold != 1.5 {
		t.Errorf("未设置时阈值 = %v, 期望 1.5", c.Builtin.HighlightThreshold)
	}

	// 只把运动量设为 0（音频也没写）→ 仍属「未设置」，回落
	c2, err := Load(writeCfg(t, `{"builtin":{"highlight_motion_weight":0}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c2.Builtin.HighlightMotionW != 0.8 {
		t.Errorf("仅运动量为 0 时应回落，实际 = %v", c2.Builtin.HighlightMotionW)
	}
}

// 平滑窗口的契约：**未配置时必须等于 5**。
// 5 是改动前 score.go 里的硬编码常量，所以「未配置 == 行为完全不变」是这次
// 配置化的全部意义 —— 一旦默认值被改成别的数，存量部署会在无人察觉的情况下改变
// 高光判定灵敏度（而且阈值没跟着调，等于悄悄换了一档）。
// 另外它必须能与 highlight_threshold 一起改：调大窗口会缩小 MAD、放大 z 分，
// 实测 5→15 时阈值要 1.2→1.8 才是同一档灵敏度（docs/highlight-progress.md §13）。
func TestHighlightSmoothWindowDefaultsToFive(t *testing.T) {
	c, err := Load(writeCfg(t, `{"builtin":{}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Builtin.HighlightSmoothWindow; got != 5 {
		t.Errorf("未配置时平滑窗口 = %v, 期望 5（保持与改动前一致）", got)
	}

	c2, err := Load(writeCfg(t, `{"builtin":{"highlight_smooth_window":15,"highlight_threshold":1.8}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c2.Builtin.HighlightSmoothWindow; got != 15 {
		t.Errorf("显式配置 15 时应保留，实际 = %v", got)
	}
	if got := c2.Builtin.HighlightThreshold; got != 1.8 {
		t.Errorf("阈值 = %v, 期望 1.8", got)
	}

	// 显式 0 / 负数视为未设置 → 回落 5，不得让 smooth() 收到非法窗口
	c3, err := Load(writeCfg(t, `{"builtin":{"highlight_smooth_window":0}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c3.Builtin.HighlightSmoothWindow; got != 5 {
		t.Errorf("显式 0 应回落 5，实际 = %v", got)
	}
}
