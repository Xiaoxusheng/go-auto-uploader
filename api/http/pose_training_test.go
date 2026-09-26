package httpapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"upload/internal/app"
	"upload/internal/config"
)

func TestPoseClipStreamer(t *testing.T) {
	cases := map[string]string{
		"闲闲饭。_2026-09-25_20-38-15_004":         "闲闲饭。",
		"D.an（9月26生日）_2026-09-26_09-14-32_000": "D.an（9月26生日）",
		"颍颍呐🍒_2026-09-25_15-37-56_003":         "颍颍呐🍒",
		"名_含_下划线_2026-09-25_20-38-15_004":      "名_含_下划线",
		"无日期片名":                                "",
		"缺口段_2026-09-25_004":                   "",
	}
	for clip, want := range cases {
		if got := poseClipStreamer(clip); got != want {
			t.Errorf("poseClipStreamer(%q) = %q, want %q", clip, got, want)
		}
	}
}

func TestPoseCurrentClip(t *testing.T) {
	old := poseTrainRoot
	t.Cleanup(func() { poseTrainRoot = old })
	root := t.TempDir()
	poseTrainRoot = root

	if _, mt, n := poseCurrentClip(); !mt.IsZero() || n != 0 {
		t.Errorf("无 frames 目录应返回零值, got mt=%v n=%d", mt, n)
	}

	frames := filepath.Join(root, "_pose_pilot", "frames")
	a := filepath.Join(frames, "爱喝旺仔_2026-09-26_09-06-59_000")
	b := filepath.Join(frames, "爱喝旺仔_2026-09-26_09-08-50_000")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	if err := os.Chtimes(a, base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(b, base.Add(time.Minute), base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	name, mt, _ := poseCurrentClip()
	if name != filepath.Base(b) || mt.IsZero() {
		t.Errorf("应取 mtime 最新的片目录 %q, got %q mt=%v", filepath.Base(b), name, mt)
	}
	// 已抽帧数：往 b 里放两帧
	for _, f := range []string{"f_0001.jpg", "f_0002.jpg"} {
		if err := os.WriteFile(filepath.Join(b, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, n := poseCurrentClip(); n != 2 {
		t.Errorf("已抽帧数应为 2, got %d", n)
	}
}

func TestPoseQueueHead(t *testing.T) {
	dl := t.TempDir()
	mk := func(name string, age time.Duration) {
		p := filepath.Join(dl, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	mk("b主播_2026-09-25_10-00-00_000.ts", 2*time.Hour) // 次旧
	mk("a主播_2026-09-25_09-00-00_000.ts", 3*time.Hour) // 最旧 → 队头
	mk("高光_2026-09-25_11-00-00_000.ts", 1*time.Hour)  // 高光产物排除
	mk("已入池_2026-09-25_08-00-00_000.ts", 4*time.Hour) // configured 排除
	configured := map[string]bool{"已入池_2026-09-25_08-00-00_000": true}

	remaining, head := poseQueueHead(dl, configured, 6)
	if remaining != 2 {
		t.Errorf("剩余应为 2, got %d", remaining)
	}
	if len(head) != 2 || head[0]["clip"] != "a主播_2026-09-25_09-00-00_000" || head[1]["clip"] != "b主播_2026-09-25_10-00-00_000" {
		t.Errorf("队头应按最旧在前, got %v", head)
	}
	if head[0]["streamer"] != "a主播" {
		t.Errorf("队头应带主播名, got %v", head[0]["streamer"])
	}
	// n 截断
	if _, head2 := poseQueueHead(dl, configured, 1); len(head2) != 1 {
		t.Errorf("n=1 应截断为 1, got %d", len(head2))
	}
}

func TestPoseRecentDone(t *testing.T) {
	old := poseTrainRoot
	t.Cleanup(func() { poseTrainRoot = old })
	root := t.TempDir()
	poseTrainRoot = root

	log := "  [1/3] 爱喝旺仔_2026-09-26_09-06-59_000: 481 帧 / 4 段预标\n" +
		"  [2/3] 帧不足，跳过 颍颍呐🍒_2026-09-25_15-37-36_000 (3)\n" +
		"  [2/3] 雪梨汁_2026-09-25_19-12-29_003: 481 帧 / 9 段预标\n" +
		"  [3/3] 爱喝旺仔_2026-09-26_09-06-59_000: 464 帧 / 4 段预标\n"
	if err := os.WriteFile(filepath.Join(root, "review_ingest_test.log"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	got := poseRecentDone(5)
	// 跳过行不算完成；同片重复入池去重取最新；顺序为最新在前
	if len(got) != 2 {
		t.Fatalf("应解析出 2 个已完成片, got %d: %v", len(got), got)
	}
	if got[0]["clip"] != "爱喝旺仔_2026-09-26_09-06-59_000" || got[1]["clip"] != "雪梨汁_2026-09-25_19-12-29_003" {
		t.Errorf("顺序/去重不符: %v", got)
	}
	if got[0]["streamer"] != "爱喝旺仔" || got[1]["streamer"] != "雪梨汁" {
		t.Errorf("主播名解析不符: %v", got)
	}
	if len(poseRecentDone(1)) != 1 {
		t.Errorf("n=1 应截断为 1")
	}
}

func TestPoseRecentSkipped(t *testing.T) {
	old := poseTrainRoot
	t.Cleanup(func() { poseTrainRoot = old })
	root := t.TempDir()
	poseTrainRoot = root

	log := "  [2/4] 测试主播B_2026-09-26_11-05-00_000: 481 帧 / 1 段预标\n" +
		"  [3/4] 帧不足，跳过 颍颍呐🍒_2026-09-25_15-37-36_000 (3)\n" +
		"  [4/4] 帧不足，跳过 Lumi静_2026-09-25_17-19-39_000 (13)\n"
	if err := os.WriteFile(filepath.Join(root, "review_ingest_test.log"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	got := poseRecentSkipped(5)
	if len(got) != 2 {
		t.Fatalf("应解析出 2 个跳过片, got %d: %v", len(got), got)
	}
	if got[0]["clip"] != "Lumi静_2026-09-25_17-19-39_000" || got[0]["frames"] != 13 {
		t.Errorf("最新跳过片在前: %v", got[0])
	}
	if got[1]["streamer"] != "颍颍呐🍒" {
		t.Errorf("主播名解析不符: %v", got[1])
	}
}

func TestPoseApplyGateThresholds(t *testing.T) {
	cfg := config.Config{Builtin: config.BuiltinSettings{
		HighlightPoseGate: &config.HighlightPoseGateConfig{Enable: true, VisMin: 0.6, FaceMax: 0.12, DetMin: 0.3, KeepRatio: 0.3, FPS: 5},
	}}
	g, ok := poseApplyGateThresholds(&cfg, 0.7, 0.12, 0.3)
	if !ok || g.VisMin != 0.7 || g.FaceMax != 0.12 || g.DetMin != 0.3 {
		t.Errorf("应写三阈值: ok=%v gate=%+v", ok, g)
	}
	if g.KeepRatio != 0.3 || g.FPS != 5 || !g.Enable {
		t.Errorf("其余字段应保持: %+v", g)
	}
	// 门未启用 → 拒绝
	cfg.Builtin.HighlightPoseGate.Enable = false
	if _, ok := poseApplyGateThresholds(&cfg, 0.7, 0.12, 0.3); ok {
		t.Errorf("门关闭时应返回 false")
	}
	// 指针传递契约：调用方 cfg 的门应被写为新值
	if cfg.Builtin.HighlightPoseGate.VisMin != 0.7 {
		t.Errorf("调用方 cfg 应写为新值: %+v", cfg.Builtin.HighlightPoseGate)
	}
}

func TestPoseMaybeAutoApplyStreak(t *testing.T) {
	old := poseTrainRoot
	t.Cleanup(func() { poseTrainRoot = old })
	root := t.TempDir()
	poseTrainRoot = root

	mk := func(gen string, bestF1, liveF1 float64) {
		res := map[string]any{
			"generated_at": gen,
			"best":         map[string]any{"vis": 0.72, "face": 0.12, "det": 0.3, "F1": bestF1},
			"live":         map[string]any{"vis": 0.6, "face": 0.12, "det": 0.3, "F1": liveF1},
		}
		b, _ := json.Marshal(res)
		if err := os.WriteFile(filepath.Join(root, "autogold_result.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("g1", 0.81, 0.79)
	st, _ := poseMaybeAutoApply()
	if st.Streak != 1 {
		t.Fatalf("第1轮 streak 应为 1, got %d (seen=%q)", st.Streak, st.LastSeen)
	}
	mk("g2", 0.81, 0.79)
	st, _ = poseMaybeAutoApply()
	if st.Streak != 2 {
		t.Fatalf("第2轮 streak 应为 2, got %d (seen=%q)", st.Streak, st.LastSeen)
	}
	// 第 3 轮前给配置存储注入启用的门（模拟生产），测试后还原
	origCfg := app.AppCfg()
	t.Cleanup(func() {
		app.CfgStore.Replace(origCfg)
		app.SaveConfigToFile()
		_ = os.Remove("config.json")
	})
	gateOn := origCfg
	gateOn.Builtin = config.BuiltinSettings{HighlightPoseGate: &config.HighlightPoseGateConfig{Enable: true, VisMin: 0.6, FaceMax: 0.12, DetMin: 0.3}}
	app.CfgStore.Replace(gateOn)
	app.SaveConfigToFile()

	mk("g3", 0.81, 0.79)
	st, applied := poseMaybeAutoApply()
	if applied == "" {
		t.Fatalf("第3轮应自动应用, streak=%d seen=%q", st.Streak, st.LastSeen)
	}
	if got := app.AppCfg().Builtin.HighlightPoseGate.VisMin; got != 0.72 {
		t.Errorf("应用后配置 vis_min 应为 0.72, got %v", got)
	}
	// 应用后生产参数等于 best，下一轮 streak 归零
	mk("g4", 0.81, 0.81)
	st, _ = poseMaybeAutoApply()
	if st.Streak != 0 {
		t.Fatalf("应用后不再更优，streak 应归零, got %d", st.Streak)
	}
}
