// Package httpapi 注册控制台 HTTP 路由（Handler 仍在 package main，本包只做装配）。
package httpapi

import "net/http"

// Routes 业务路由表；Handler 由调用方注入。
type Routes struct {
	Index    http.HandlerFunc
	Login    http.HandlerFunc
	Logout   http.HandlerFunc
	PubKey   http.HandlerFunc
	Exchange http.HandlerFunc

	Status       http.HandlerFunc
	LiveTasks    http.HandlerFunc
	History      http.HandlerFunc
	Queue        http.HandlerFunc
	DirsStatus   http.HandlerFunc
	Config       http.HandlerFunc
	Logs         http.HandlerFunc
	LogsDownload http.HandlerFunc

	Streamers       http.HandlerFunc
	ActiveStreamers http.HandlerFunc
	Cookies         http.HandlerFunc

	CtlStart, CtlPause, CtlStop, CtlRelogin, CtlRescan http.HandlerFunc
	CtlClearFail, CtlRetryFail, CtlClearSuccess        http.HandlerFunc

	RecorderStatus, RecorderControl, RecorderLogs http.HandlerFunc
	WebSocket                                     http.HandlerFunc
	Extra                                         func(mux *http.ServeMux)
}

// Register 把路由挂到 mux。
func Register(mux *http.ServeMux, r Routes) {
	mux.HandleFunc("/api/v1/sec/pubkey", r.PubKey)
	mux.HandleFunc("/api/v1/sec/exchange", r.Exchange)
	mux.HandleFunc("/", r.Index)
	mux.HandleFunc("/api/v1/auth/login", r.Login)
	mux.HandleFunc("/api/v1/auth/logout", r.Logout)
	mux.HandleFunc("/api/v1/status", r.Status)
	mux.HandleFunc("/api/v1/tasks/live", r.LiveTasks)
	mux.HandleFunc("/api/v1/tasks/history", r.History)
	mux.HandleFunc("/api/v1/tasks/queue", r.Queue)
	mux.HandleFunc("/api/v1/control/start", r.CtlStart)
	mux.HandleFunc("/api/v1/control/pause", r.CtlPause)
	mux.HandleFunc("/api/v1/control/stop", r.CtlStop)
	mux.HandleFunc("/api/v1/control/relogin", r.CtlRelogin)
	mux.HandleFunc("/api/v1/control/rescan", r.CtlRescan)
	mux.HandleFunc("/api/v1/control/clear-fail-queue", r.CtlClearFail)
	mux.HandleFunc("/api/v1/control/retry-fail-queue", r.CtlRetryFail)
	mux.HandleFunc("/api/v1/control/clear-success-queue", r.CtlClearSuccess)
	mux.HandleFunc("/api/v1/dirs/status", r.DirsStatus)
	mux.HandleFunc("/api/v1/config", r.Config)
	mux.HandleFunc("/api/v1/logs", r.Logs)
	mux.HandleFunc("/api/v1/logs/download", r.LogsDownload)
	mux.HandleFunc("/api/v1/streamers", r.Streamers)
	mux.HandleFunc("/api/v1/streamers/active", r.ActiveStreamers)
	mux.HandleFunc("/api/v1/recorder/status", r.RecorderStatus)
	mux.HandleFunc("/api/v1/recorder/control", r.RecorderControl)
	mux.HandleFunc("/api/v1/recorder/logs", r.RecorderLogs)
	mux.HandleFunc("/api/v1/cookies", r.Cookies)
	mux.HandleFunc("/ws/live", r.WebSocket)
	if r.Extra != nil {
		r.Extra(mux)
	}
}
