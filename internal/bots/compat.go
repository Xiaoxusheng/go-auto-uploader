package bots

import (
	"upload/internal/app"
	"upload/internal/config"
	"upload/internal/recorder"
)

func appCfg() config.Config { return app.AppCfg() }

var (
	appLogs                     = app.AppLogs
	liveTasks                   = &app.LiveTasks
	successStore                = app.SuccessStore
	GetBuiltinRecorderTasks     = recorder.Tasks
	isBuiltinLiveStatus         = recorder.IsLiveStatus
	ExtractBuiltinDouyinLiveURL = recorder.ExtractDouyinLiveURL
)

type (
	Task              = app.Task
	BuiltinTaskStatus = recorder.BuiltinTaskStatus
	BuiltinPlatform   = recorder.BuiltinPlatform
)
