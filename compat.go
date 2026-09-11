package main

// 旧 webapi/main 兼容层：业务状态真源在 internal/app；bots/测试通过本文件别名访问。

import (
	"net/http"

	httpapi "upload/api/http"
	"upload/internal/app"
	"upload/internal/config"
	"upload/internal/recorder"
	"upload/internal/storage"
)

type (
	Task                = app.Task
	BuiltinTaskStatus   = recorder.BuiltinTaskStatus
	BuiltinTaskFlags    = recorder.TaskFlags
	BuiltinConfig       = recorder.BuiltinConfig
	BuiltinPlatform     = recorder.BuiltinPlatform
	BuiltinCookieConfig = recorder.BuiltinCookieConfig
	HistoryRecord       = storage.HistoryRecord
	UploadRecord        = storage.UploadRecord
	TrendPoint          = storage.TrendPoint
	DirStatus           = storage.DirStatus
)

func appCfg() config.Config { return app.AppCfg() }

var (
	appLogs      = app.AppLogs
	liveTasks    = &app.LiveTasks
	successStore = app.SuccessStore
)

func buildStatusData() map[string]interface{} {
	if httpSrv != nil {
		return httpSrv.BuildStatusData()
	}
	cfg := app.AppCfg()
	return map[string]interface{}{
		"running": app.IsRunning(), "uptime": app.UptimeSeconds(),
		"workers": cfg.Workers, "dirs": []map[string]interface{}{},
		"scanningInterval": cfg.ScanInterval,
		"dayRate":          cfg.DayRate, "nightRate": cfg.NightRate,
		"rate": app.CurrentRate(),
	}
}

func buildQueueData() map[string]interface{} { return httpapi.BuildQueueData() }

func diskFreeSpace(path string) int64 { return httpapi.DiskFreeSpaceStd(path) }

var (
	isBuiltinLiveStatus         = recorder.IsLiveStatus
	GetBuiltinRecorderTasks     = recorder.Tasks
	ExtractBuiltinDouyinLiveURL = recorder.ExtractDouyinLiveURL
	handleFile                  = app.HandleFile
	upload                      = app.Upload
	recordSuccess               = app.RecordSuccess
)

func broadcastWS(msgType string, payload interface{}) { app.BroadcastWS(msgType, payload) }
func addLog(level, message, errorMsg string)          { app.AddLog(level, message, errorMsg) }
func triggerScan(reason string)                       { app.TriggerScan(reason) }
func triggerReportReset()                             { app.TriggerReportReset() }
func saveConfigToFile()                               { app.SaveConfigToFile() }
func sendWeChatNotify(title, body string)             { app.SendWeChatNotify(title, body) }
func login() error                                    { return app.Login() }
func currentRate() int                                { return app.CurrentRate() }
func detectRoot(path string) string                   { return app.DetectRoot(path) }

// InitBuiltinRecorder 挂载内置录制路由。
func InitBuiltinRecorder(mux *http.ServeMux) { recorder.Init(mux) }

// 供 recorder hooks 使用的包级包装（httpSrv 在 webglue 启动后可用）。
func parseEncryptedRequest(r *http.Request, target interface{}) error {
	if httpSrv == nil {
		return nil
	}
	return httpSrv.ParseEncryptedRequest(r, target)
}

func sendJSONSuccess(w http.ResponseWriter, r *http.Request, data interface{}) {
	if httpSrv == nil {
		return
	}
	httpSrv.SendJSONSuccess(w, r, data)
}

func sendJSONError(w http.ResponseWriter, r *http.Request, statusCode int, message string) {
	if httpSrv == nil {
		return
	}
	httpSrv.SendJSONError(w, r, statusCode, message)
}
