package main

import (
	"net/http"

	httpapi "upload/api/http"
	"upload/internal/app"
	"upload/internal/recorder"
)

var httpSrv *httpapi.Server

func startHTTP(port int) error {
	httpSrv = httpapi.New(httpapi.Options{
		IndexHTML: indexHTML,
		Extra:     func(m *http.ServeMux) { InitBuiltinRecorder(m) },
	})

	app.SetNotifyChannel("telegram", SendTelegramNotification)
	app.SetNotifyChannel("qq", SendQQNotification)

	recorder.SetHooks(recorder.Hooks{
		Broadcast:             app.BroadcastWS,
		Notify:                app.SendWeChatNotify,
		TriggerScan:           app.TriggerScan,
		ParseEncryptedRequest: parseEncryptedRequest,
		SendJSONSuccess:       sendJSONSuccess,
		SendJSONError:         sendJSONError,
	})

	return httpSrv.Start(port)
}
