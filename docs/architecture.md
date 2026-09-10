# 架构说明

> 基于 `feat/record-screenshot-switches` 重构后的模块边界。

## 分层

```text
cmd/main (package main)
  ├── internal/config        配置单源 Store
  ├── internal/scanner       目录扫描 → 候选
  ├── internal/uploader      Queue / WorkerPool / Task
  ├── internal/remote        OpenList Login/Put
  ├── internal/convert       TS→MP4
  ├── internal/hashstore     秒传 SHA-256
  ├── internal/storage       history / success / dirStatus
  ├── internal/ratelimit     日夜限速
  ├── internal/ws            WebSocket Hub
  ├── internal/notification  Notifier 扇出
  ├── internal/auth          登录会话 + Middleware
  ├── internal/cryptox       AES-GCM + RSA 会话密钥
  ├── internal/logx          日志环形缓冲 + stdout 拦截
  ├── internal/recorder      Docker 控制 + 名单行/开关纯函数
  ├── internal/naming        文件名清洗
  ├── internal/fsutil        原子写
  └── package main 遗留      webapi Handler / bots / handleFile 编排
```

内置录制已按域拆分（仍在 `package main`，便于依赖注入后再迁包）：

```text
builtin_types.go          状态/开关/配置类型
builtin_status.go         状态更新 + WS 防抖广播
builtin_init.go           启动/热重载/任务快照
builtin_txt.go            名单文件读写
builtin_douyin_crypto.go  短链 + SM3/RC4/a_bogus
builtin_douyin.go         抖音推流探测
builtin_kuaishou.go       快手
builtin_soop.go           Soop
builtin_proxy.go          封面反代 SSRF 防护
builtin_ffmpeg.go         抽帧 + 录制/截屏主流程
builtin_monitor.go        监控协程
builtin_api.go            Web API
```

## 上传数据流

```text
scanner.Scan
    → []Candidate
    → taskQueue.Enqueue (去重)
    → WorkerPool → handleFile
         → convert? → hashstore → remote.Put → storage
```

## 通知

```text
sendWeChatNotify → notification.Hub
                      ├── DynamicPushPlus (微信)
                      ├── SendTelegramNotification
                      └── SendQQNotification
```

## WebSocket

```text
broadcastWS → ws.Hub.Publish → Run → Client.WritePump
```

## 遗留（后续可再拆）

- `builtin_recorder.go`（~2700 行）仍为 package main
- `webapi.go` 鉴权/加密与 Handler 仍在同一文件
- `main.go` 上传编排 `handleFile`/`upload` 仍内联

## 兼容承诺

- CLI 参数名不变
- `config.json` 扁平 JSON 字段不变
- REST 路径 `/api/v1/*`、`/ws/live` 不变
- 数据文件：`uploaded_hash.db`、`upload_success.json`、`dir_status.json`
