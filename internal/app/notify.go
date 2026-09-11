package app

import (
	"context"
	"time"

	"upload/internal/notification"
)

// NotifyHub 统一通知扇出；main 启动时可 Register/SetNotifyChannel 通道。
var NotifyHub = notification.New()

// SendWeChatNotify 经 NotifyHub 异步扇出。
func SendWeChatNotify(title, body string) {
	NotifyHub.NotifyAsync(notification.Message{Title: title, Body: body})
}

// SendAlert 向前端控制台下发系统弹窗警告。
func SendAlert(level, title, message string) {
	BroadcastWS("systemAlert", map[string]interface{}{
		"level":   level,
		"title":   title,
		"message": message,
		"time":    time.Now().Format("15:04:05"),
	})
}

// NotifySync 同步通知（测试/脚本用）。
func NotifySync(msg notification.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	NotifyHub.NotifyAll(ctx, msg)
}
