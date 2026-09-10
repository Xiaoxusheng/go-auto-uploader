// Package recorder — Hooks 由 main 在启动时注入，避免 recorder 依赖 package main。
package recorder

import (
	"net/http"
)

// Hooks 副作用回调；nil 时为空操作。
type Hooks struct {
	Broadcast             func(msgType string, payload any)
	Notify                func(title, body string)
	TriggerScan           func(reason string)
	ParseEncryptedRequest func(r *http.Request, target any) error
	SendJSONSuccess       func(w http.ResponseWriter, r *http.Request, data any)
	SendJSONError         func(w http.ResponseWriter, r *http.Request, status int, message string)
	EnableLogs            func() bool
	AddLog                func(level, message, errMsg string)
}

var hooks Hooks

// SetHooks 注入全局副作用（进程内一次）。
func SetHooks(h Hooks) { hooks = h }

func hookBroadcast(t string, p any) {
	if hooks.Broadcast != nil {
		hooks.Broadcast(t, p)
	}
}

func hookNotify(title, body string) {
	if hooks.Notify != nil {
		hooks.Notify(title, body)
	}
}

func hookTriggerScan(reason string) {
	if hooks.TriggerScan != nil {
		hooks.TriggerScan(reason)
	}
}

func hookParseEncrypted(r *http.Request, target any) error {
	if hooks.ParseEncryptedRequest != nil {
		return hooks.ParseEncryptedRequest(r, target)
	}
	return nil
}

func hookJSONOK(w http.ResponseWriter, r *http.Request, data any) {
	if hooks.SendJSONSuccess != nil {
		hooks.SendJSONSuccess(w, r, data)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
}

func hookJSONErr(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if hooks.SendJSONError != nil {
		hooks.SendJSONError(w, r, status, msg)
		return
	}
	http.Error(w, msg, status)
}

// Tasks 当前内置任务快照（供 main/webapi/bots）。
func Tasks() []BuiltinTaskStatus { return GetBuiltinRecorderTasks() }

// Init 挂载内置录制 HTTP 路由。
func Init(mux *http.ServeMux) { InitBuiltinRecorder(mux) }

// ExtractDouyinLiveURL 抖音短链解析。
func ExtractDouyinLiveURL(text string) (string, error) { return ExtractBuiltinDouyinLiveURL(text) }
