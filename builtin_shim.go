package main

// builtin 运行时已迁入 internal/recorder；本文件保留 main/webapi/bots 的薄别名与钩子装配。

import (
	"net/http"

	"upload/internal/recorder"
)

type (
	BuiltinTaskStatus   = recorder.BuiltinTaskStatus
	BuiltinTaskFlags    = recorder.TaskFlags
	BuiltinConfig       = recorder.BuiltinConfig
	BuiltinPlatform     = recorder.BuiltinPlatform
	BuiltinCookieConfig = recorder.BuiltinCookieConfig
)

var (
	isBuiltinLiveStatus         = recorder.IsLiveStatus
	GetBuiltinRecorderTasks     = recorder.Tasks
	ExtractBuiltinDouyinLiveURL = recorder.ExtractDouyinLiveURL
)

// initBuiltinHooks 注入 main 侧副作用，供 recorder 包调用。
func initBuiltinHooks() {
	recorder.SetHooks(recorder.Hooks{
		Broadcast:             broadcastWS,
		Notify:                sendWeChatNotify,
		TriggerScan:           triggerScan,
		ParseEncryptedRequest: parseEncryptedRequest,
		SendJSONSuccess:       sendJSONSuccess,
		SendJSONError:         sendJSONError,
	})
}

// InitBuiltinRecorder 保持原入口名。
func InitBuiltinRecorder(mux *http.ServeMux) {
	initBuiltinHooks()
	recorder.Init(mux)
}
