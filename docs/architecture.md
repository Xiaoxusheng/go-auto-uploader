# 架构说明

> 基于 bots-cmd-layout 收口（2026-09-11）。

## 分层

```text
cmd/uploader (package main)
  ├── main.go              flag + app.Run
  └── webglue.go           httpapi.Server + bots/recorder 装配

web/                       //go:embed index.html

api/http                   Server + REST/WS Handler

internal/
  ├── app                  生命周期单源：Run/Scan/Upload/Report/Streamers/Notify/Hubs
  ├── bots                 Telegram / QQ 机器人（StatusFn/QueueFn 注入）
  ├── config               配置单源 Store（含 rate 手动限速）
  ├── scanner              目录扫描 → 候选
  ├── uploader             Queue / WorkerPool / Pipeline
  ├── remote               OpenList Login/Put
  ├── convert              TS→MP4
  ├── hashstore            秒传 SHA-256
  ├── storage              history / success / dirStatus
  ├── ratelimit            日夜限速
  ├── ws                   WebSocket Hub
  ├── notification         Notifier 扇出
  ├── auth                 登录会话 + Middleware
  ├── cryptox              AES-GCM + RSA 会话密钥
  ├── logx                 日志环形缓冲 + stdout 拦截
  ├── recorder             builtin 平台探测/录制 + Docker 控制
  ├── naming               文件名清洗
  └── fsutil               原子写
```

根目录不再有 `package main` 业务文件。

## 上传数据流

```text
app.RunOnce → scanner.Scan → TaskQueue.Enqueue
    → WorkerPool → app.HandleFile (uploader.Pipeline)
         → convert? → hashstore → remote.Put → storage
```

## HTTP / 控制台

```text
cmd/uploader.startHTTP
  → httpapi.New(Options{IndexHTML: web.IndexHTML, Extra: recorder.Init})
  → bots.SetDeps(BuildStatusData, BuildQueueData)
  → app.SetNotifyChannel(telegram/qq)
  → recorder.SetHooks(...)
  → Server.Start
```

## 限速

```text
CurrentRate:
  config.Rate > 0  → 强制该 MB/s
  else             → ratelimit.Select(DayRate, NightRate, now)
```

## 通知

```text
app.SendWeChatNotify → app.NotifyHub
                         ├── DynamicPushPlus (微信)
                         ├── telegram (bots.SendTelegramNotification)
                         └── qq        (bots.SendQQNotification)
```

## 兼容承诺

- CLI 参数名不变（含 `-rate` 现已生效）
- `config.json` 扁平 JSON 字段不变；新增可选 `"rate": 0`
- REST 路径 `/api/v1/*`、`/ws/live` 不变
- 数据文件路径/格式不变
- 构建入口：`go build -o uploader ./cmd/uploader`
