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
	// 邮件通道：开播/下播/异常/统计报告统一扇出，配置来自统一 config.json，热更新即时生效
	NotifyHub.Register(&notification.DynamicEmail{
		CfgFn: func() notification.EmailConfig {
			c := AppCfg()
			return notification.EmailConfig{
				Host:     c.MailSMTPHost,
				Port:     c.MailSMTPPort,
				From:     c.MailFrom,
				AuthCode: c.MailAuthCode,
				To:       c.MailTo,
			}
		},
	})
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
