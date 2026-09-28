package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 高光参数的默认值回落逻辑有一个容易踩的边界：音频权重**显式设为 0** 是合法且
// 推荐的配置（真留一切片实证：0.8/0.2 th1.5 → F1 0.584；1.0/0.0 th1.2 → F1 0.842，
// 2026-09-24 起后者同时是代码默认值，见 RETRAIN_2026-09-24.md）。
// 如果哪天有人把回落条件改成 `||` 或按单个字段判空，音频权重 0 的配置会被
// 静默改掉 —— 准确率悄悄退化且不报错。这两条测试就是钉住这个契约。

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
	// 两个权重都没写 → 视为未设置，回落 1.0/0.0（2026-09-24 重训落地候选）
	c, err := Load(writeCfg(t, `{"builtin":{}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Builtin.HighlightMotionW != 1.0 || c.Builtin.HighlightAudioW != 0.0 {
		t.Errorf("未设置时 = %v/%v, 期望 1.0/0.0",
			c.Builtin.HighlightMotionW, c.Builtin.HighlightAudioW)
	}
	if c.Builtin.HighlightThreshold != 1.2 {
		t.Errorf("未设置时阈值 = %v, 期望 1.2", c.Builtin.HighlightThreshold)
	}

	// 只把运动量设为 0（音频也没写）→ 仍属「未设置」，回落
	c2, err := Load(writeCfg(t, `{"builtin":{"highlight_motion_weight":0}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c2.Builtin.HighlightMotionW != 1.0 {
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

// 迟滞退出比契约：0=关（默认/回落），仅 (0,1) 合法。
// 非法值必须回落 0，否则 Select 会走错分支或行为未定义。
// MinAC1: 0=off (default/clamp), only (0,1) legal. Grey recommend 0.15.
func TestHighlightMinAC1DefaultAndClamp(t *testing.T) {
	c, err := Load(writeCfg(t, `{"builtin":{}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Builtin.HighlightMinAC1; got != 0 {
		t.Errorf("default min_ac1 = %v, want 0", got)
	}

	c2, err := Load(writeCfg(t, `{"builtin":{"highlight_min_ac1":0.15}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c2.Builtin.HighlightMinAC1; got != 0.15 {
		t.Errorf("0.15 should pass, got %v", got)
	}

	for _, body := range []string{
		`{"builtin":{"highlight_min_ac1":-0.2}}`,
		`{"builtin":{"highlight_min_ac1":1.0}}`,
		`{"builtin":{"highlight_min_ac1":2}}`,
	} {
		c3, err := Load(writeCfg(t, body))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := c3.Builtin.HighlightMinAC1; got != 0 {
			t.Errorf("%s illegal should clamp 0, got %v", body, got)
		}
	}
}

func TestHighlightExitRatioDefaultAndClamp(t *testing.T) {
	c, err := Load(writeCfg(t, `{"builtin":{}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Builtin.HighlightExitRatio; got != 0 {
		t.Errorf("未配置 exit_ratio = %v, 期望 0（关闭迟滞）", got)
	}

	c2, err := Load(writeCfg(t, `{"builtin":{"highlight_exit_ratio":0.8}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c2.Builtin.HighlightExitRatio; got != 0.8 {
		t.Errorf("显式 0.8 应保留，实际 = %v", got)
	}

	for _, body := range []string{
		`{"builtin":{"highlight_exit_ratio":-0.5}}`,
		`{"builtin":{"highlight_exit_ratio":1.0}}`,
		`{"builtin":{"highlight_exit_ratio":2}}`,
	} {
		c3, err := Load(writeCfg(t, body))
		if err != nil {
			t.Fatalf("Load %s: %v", body, err)
		}
		if got := c3.Builtin.HighlightExitRatio; got != 0 {
			t.Errorf("%s → %v, 期望回落 0", body, got)
		}
	}
}

// 姿态门（§19）：enable=false 时整对象必须被回落为 nil（门关闭，行为与未配置一致）；
// enable=true 时非法参数回落到定标值，合法参数原样保留。
// 门槛方向与回落值见 docs/highlight-spatial-de.md §18c/§19。
func TestHighlightPoseGateDefaultsAndClamp(t *testing.T) {
	p := writeCfg(t, `{"builtin":{
		"highlight_min_ac1":0.15,
		"highlight_pose_gate":{"enable":false,"vis_min":9.9}}}`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Builtin.HighlightPoseGate != nil {
		t.Fatalf("enable=false 时姿态门应为 nil（关闭），得到 %+v", c.Builtin.HighlightPoseGate)
	}

	p2 := writeCfg(t, `{"builtin":{
		"highlight_pose_gate":{"enable":true,"vis_min":9.9,"face_max":0.14,
		"keep_ratio":0.5}}}`)
	c2, err := Load(p2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	g := c2.Builtin.HighlightPoseGate
	if g == nil || !g.Enable {
		t.Fatal("enable=true 时姿态门应保留")
	}
	if g.VisMin != 0.6 {
		t.Errorf("vis_min 非法值 9.9 应回落 0.6，得到 %v", g.VisMin)
	}
	if g.FaceMax != 0.14 || g.KeepRatio != 0.5 {
		t.Errorf("合法参数被改动: %+v", g)
	}
}
