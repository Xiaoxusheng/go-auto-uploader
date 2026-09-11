// Package bots 承载 Telegram / QQ 控制机器人；状态查询经注入函数获取，避免反向依赖 api/http。
package bots

import "sync"

// StatusFn 返回系统状态宽表（由 main/webglue 注入 httpapi.Server.BuildStatusData）。
var StatusFn func() map[string]interface{}

// QueueFn 返回队列计数。
var QueueFn func() map[string]interface{}

var depsMu sync.RWMutex

// SetDeps 注入状态查询回调。
func SetDeps(status, queue func() map[string]interface{}) {
	depsMu.Lock()
	StatusFn = status
	QueueFn = queue
	depsMu.Unlock()
}

func buildStatusData() map[string]interface{} {
	depsMu.RLock()
	fn := StatusFn
	depsMu.RUnlock()
	if fn == nil {
		return map[string]interface{}{}
	}
	return fn()
}

func buildQueueData() map[string]interface{} {
	depsMu.RLock()
	fn := QueueFn
	depsMu.RUnlock()
	if fn == nil {
		return map[string]interface{}{}
	}
	return fn()
}
