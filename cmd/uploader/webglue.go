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
	// 内置引擎与主配置共用同一个 config.json，先注入仓库再挂载路由
	recorder.SetConfigStore(app.CfgStore)

	httpSrv = httpapi.New(httpapi.Options{
		IndexHTML: web.IndexHTML,
		Vendor:    web.VendorHandler(),
		Extra:     func(m *http.ServeMux) { recorder.Init(m) },
	})

	bots.SetDeps(httpSrv.BuildStatusData, httpapi.BuildQueueData)
	app.SetNotifyChannel("telegram", bots.SendTelegramNotification)

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
