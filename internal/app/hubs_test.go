package app

import "testing"

// 实时推送（开播/下播/异常）刻意不接邮件通道，邮件仅用于定时统计报告
// （app/report.go 的 sendQQMail）。高频推送走邮件会刷爆邮箱。
// 若此测试失败，说明有人把 DynamicEmail 注册回了推送中枢。
func TestNotifyHubHasNoEmailChannel(t *testing.T) {
	InitHubs()
	names := NotifyHub.Names()
	for _, name := range names {
		if name == "email" {
			t.Fatalf("推送中枢不应包含邮件通道（当前注册: %v）", names)
		}
	}
	if len(names) == 0 {
		t.Fatal("推送中枢至少应包含微信 PushPlus 通道")
	}
}
