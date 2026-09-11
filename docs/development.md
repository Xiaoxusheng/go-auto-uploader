# 开发指南

## 环境

- Go ≥ 1.25
- 可选：ffmpeg（录制 / TS→MP4）、docker（外置录制引擎）

## 常用命令

```bash
go build ./...
go build -o uploader ./cmd/uploader
go test ./...
go vet ./...
gofmt -w .
```

Windows 上 `-race` 需要 CGO + gcc；Linux CI 建议：

```bash
CGO_ENABLED=1 go test -race ./...
```

## 新增业务逻辑放哪

| 场景 | 包 |
|------|-----|
| 配置字段 | `internal/config` |
| 上传队列/Worker | `internal/uploader` |
| 远端存储 | `internal/remote` |
| 本地落盘状态 | `internal/storage` |
| 通知通道 | `internal/notification` |
| WS 消息 | `internal/ws` |
| 目录扫描规则 | `internal/scanner` |
| Telegram/QQ 机器人 | `internal/bots` |
| HTTP Handler | `api/http` |

**不要**继续往 `cmd/uploader` 堆业务；状态放进 Store/Service。

## 测试

- 单测跟包放：`internal/foo/foo_test.go`
- 依赖外部系统时注入 `Exec` / `Func` 等函数字段，勿在测试里真调 docker/网络

## 提交

- 每阶段：实现 → `gofmt` → `go test ./...` → `go vet` → commit
- 不提交二进制 / `downloads/` / 运行时 json
