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

## 进程停机（优雅退出）

`app.Run` 收到 SIGINT/SIGTERM 后的序列（`internal/app/run.go`）：

1. `SetRunning(false)` + `AppCancel()` —— 停扫描与上传池
2. `recorder.StopAllRecordings()` —— 置停机标记 + 取消所有录制会话
3. `recorder.WaitActiveTasks(15s)` —— 等监控协程全部退出
4. `os.Exit(0)`

**为什么必须显式停录制**：`AppCtx` 只传给了上传池；内置录制的监控协程用的是
`context.WithCancel(context.Background())` 自建的 ctx，**`AppCancel` 管不到它**。
`RecordStream` 收到取消后会向 ffmpeg stdin 发 `q`，最多等 10s 封装（超时才 Kill），
所以等待窗口必须显著大于 10s（当前 15s）。

**不这么做的后果**：`os.Exit` 不执行 defer、不杀子进程 → ffmpeg 被丢成孤儿，继续录制
同一路流、持续写盘且永远不会被上传（2026-09-26 实测：每次重启必产生一批，一天累积 15 个，
`爱喝旺仔` 等主播被 5–6 个并发会话重复录制，磁盘 20 分钟掉 10GB）。

**停机标记为何不复用 `builtinTaskStates="deleted"`**：API 层的 delete 会连带调用
`syncBuiltinAnchorToTxt` 修改用户手编的 `builtin_urls.txt`，而停机只该影响内存状态。
因此用独立的 `builtinShuttingDown` 原子标记，监控协程在循环**顶部**检查后退出（不再重开）。

**其他 ffmpeg 派生点**：`highlight/*` 用 `exec.CommandContext(ctx,…)` 有 ctx 保护；
`extractBuiltinCoverFromLocalFile`（抽封面图）由受 `recordCtx` 管的 goroutine 调用，
且是单帧任务（`-frames:v 1`）；`convert`/`pose`/`publish` 的 ffmpeg 无 ctx 但都是短命任务，
会自行退出。**只有录制是无限运行的**，所以停机保护只需覆盖它。

排查孤儿进程：`bash _diag/kill_orphans.sh [--apply]`
（判据 = ffmpeg 启动时间早于当前 uploader 实例；`ps -W` 的**第 4 列才是 WINPID**）。

## 兼容承诺

- CLI 参数名不变（含 `-rate` 现已生效）
- `config.json` 扁平 JSON 字段不变；新增可选 `"rate": 0`
- REST 路径 `/api/v1/*`、`/ws/live` 不变
- 数据文件路径/格式不变
- 构建入口：`go build -o uploader ./cmd/uploader`
