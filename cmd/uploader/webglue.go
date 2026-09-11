package main

import (
	"net/http"

	httpapi "upload/api/http"
	"upload/internal/app"
	"upload/internal/bots"
	"upload/internal/recorder"
	"upload/web"
)

var httpSrv *httpapi.Server

func startHTTP(port int) error {
	httpSrv = httpapi.New(httpapi.Options{
		IndexHTML: web.IndexHTML,
		Extra:     func(m *http.ServeMux) { recorder.Init(m) },
	})

	bots.SetDeps(httpSrv.BuildStatusData, httpapi.BuildQueueData)
	app.SetNotifyChannel("telegram", bots.SendTelegramNotification)
	app.SetNotifyChannel("qq", bots.SendQQNotification)

	recorder.SetHooks(recorder.Hooks{
		Broadcast:             app.BroadcastWS,
		Notify:                app.SendWeChatNotify,
		TriggerScan:           app.TriggerScan,
		ParseEncryptedRequest: httpSrv.ParseEncryptedRequest,
		SendJSONSuccess:       httpSrv.SendJSONSuccess,
		SendJSONError:         httpSrv.SendJSONError,
	})

	return httpSrv.Start(port)
}
