package app

import (
	"context"

	"upload/internal/cryptox"
	"upload/internal/notification"
	"upload/internal/ws"
)

// InitHubs 初始化 WS / 通知中枢（Run 启动时调用）。
func InitHubs() {
	WSHub = ws.New(ws.WithEncrypt(
		func(plain, key []byte) (string, error) { return cryptox.Encrypt(plain, key) },
		func() bool { return AppCfg().EnableEncryption },
	))
	NotifyHub = notification.New()
	NotifyHub.Register(&notification.DynamicPushPlus{
		TokenFn: func() string { return AppCfg().WechatToken },
		Client:  HTTPCli,
	})
	// 注意：实时推送（开播/下播/异常）只走微信 PushPlus / Telegram，刻意不接邮件通道——
	// 邮件仅用于 reportLoop 的定时统计报告（见 app/report.go 的 sendQQMail），
	// 避免高频推送刷爆邮箱。如需恢复推送邮件，在此处注册 DynamicEmail 即可。
}

// SetNotifyChannel 注册外部通知通道（Telegram / QQ）。
func SetNotifyChannel(label string, fn func(title, body string)) {
	if fn == nil {
		return
	}
	NotifyHub.Register(notification.FuncNotifier{
		Label: label,
		Fn: func(_ context.Context, m notification.Message) error {
			fn(m.Title, m.Body)
			return nil
		},
	})
}
