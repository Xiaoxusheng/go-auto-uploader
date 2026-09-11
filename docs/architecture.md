# 架构说明

> 基于 `main` 上完成的 engineering-refactor 收口（2026-09-11）。

## 分层

```text
cmd/main (package main)
  ├── main.go              flag + app.Run（≤150 行）
  ├── webglue.go           httpapi.Server 启动 + recorder hooks
  ├── compat.go            bots/tests 薄别名
  ├── telegram_bot.go      TG 机器人（遗留 main）
  ├── qq_bot.go            QQ 机器人（遗留 main）
  │
  ├── api/http             Server + REST/WS Handler（依赖注入 app）
  │
  └── internal/
      ├── app              生命周期单源：Run/Scan/Upload/Report/Streamers/Notify/Hubs
      ├── config           配置单源 Store
      ├── scanner          目录扫描 → 候选
      ├── uploader         Queue / WorkerPool / Pipeline
      ├── remote           OpenList Login/Put
      ├── convert          TS→MP4
      ├── hashstore        秒传 SHA-256
      ├── storage          history / success / dirStatus
      ├── ratelimit        日夜限速
      ├── ws               WebSocket Hub
      ├── notification     Notifier 扇出
      ├── auth             登录会话 + Middleware
      ├── cryptox          AES-GCM + RSA 会话密钥
      ├── logx             日志环形缓冲 + stdout 拦截
      ├── recorder         builtin 平台探测/录制 + Docker 控制
      ├── naming           文件名清洗
      └── fsutil           原子写
```

## 上传数据流

```text
app.RunOnce → scanner.Scan → TaskQueue.Enqueue
    → WorkerPool → app.HandleFile (uploader.Pipeline)
         → convert? → hashstore → remote.Put → storage
```

## HTTP / 控制台

```text
main.startHTTP
  → httpapi.New(Options{IndexHTML, Extra})
  → recorder.SetHooks(...)
  → Server.Start
       → Register(Routes) + auth.Middleware
       → sysStatsCollector / wsBroadcastLoop / logCollector / wsDashboardBroadcaster
```

## 通知

```text
app.SendWeChatNotify → app.NotifyHub
                         ├── DynamicPushPlus (微信)
                         ├── telegram (SetNotifyChannel → SendTelegramNotification)
                         └── qq        (SetNotifyChannel → SendQQNotification)
```

## WebSocket

```text
app.BroadcastWS → app.WSHub.PublishTyped → Run → Client.WritePump
```

## 遗留（后续可再拆）

- `telegram_bot.go` / `qq_bot.go` 仍为 package main，经 `compat.go` 访问 `internal/app`
- `index.html` embed 仍在 main（`//go:embed` 不能跨包）

## 兼容承诺

- CLI 参数名不变
- `config.json` 扁平 JSON 字段不变
- REST 路径 `/api/v1/*`、`/ws/live` 不变
- 数据文件路径/格式不变
