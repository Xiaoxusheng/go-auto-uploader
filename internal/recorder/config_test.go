package recorder

import (
	"context"
	"testing"
)

// 已发布的配置快照不能被后续修改污染（读方拿到的永远是完整、不可变的快照）。
func TestConfigSnapshotIsolation(t *testing.T) {
	SetConfig(&BuiltinConfig{Quality: "uhd", WatermarkText: "a"})
	s1 := Config()
	if s1.WatermarkText != "a" {
		t.Fatalf("s1=%+v", s1)
	}

	UpdateConfig(func(b *BuiltinConfig) { b.WatermarkText = "b" })

	if s1.WatermarkText != "a" {
		t.Fatal("旧快照被就地修改了，读方会看到半更新状态")
	}
	if Config().WatermarkText != "b" {
		t.Fatal("更新未生效")
	}
	if Config().Quality != "uhd" {
		t.Fatal("复制-修改流程丢失了未触碰的字段")
	}
}

// Config() 在任何情况下都不能返回 nil，否则读方会踩空指针。
func TestConfigNeverNil(t *testing.T) {
	if Config() == nil {
		t.Fatal("Config() 不应返回 nil")
	}
}

// UpdateConfig 应自动补足缺省值。
func TestUpdateConfigAppliesDefaults(t *testing.T) {
	SetConfig(&BuiltinConfig{})
	got := UpdateConfig(func(b *BuiltinConfig) { b.SegmentTime = 5 })
	if got.CheckInterval != 30 || got.Quality != "uhd" || got.SavePath != "./downloads" {
		t.Fatalf("缺省值未补齐: %+v", got)
	}
}

// 配置热重载：标记 + 取消会话，并可被监控循环消费一次。
func TestRestartActiveRecordingsMarksAndCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	builtinCancels.Store("Douyin_1", context.CancelFunc(cancel))
	defer builtinCancels.Delete("Douyin_1")
	defer clearConfigRestart("Douyin_1")

	RestartActiveRecordings()

	if !isConfigRestart("Douyin_1") {
		t.Fatal("会话应被标记为配置重载")
	}
	if ctx.Err() == nil {
		t.Fatal("会话应被取消")
	}
	if !clearConfigRestart("Douyin_1") {
		t.Fatal("首次 clear 应返回 true")
	}
	if isConfigRestart("Douyin_1") {
		t.Fatal("标记应已被清除")
	}
	if clearConfigRestart("Douyin_1") {
		t.Fatal("重复 clear 应返回 false")
	}
}
