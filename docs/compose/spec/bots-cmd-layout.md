---
feature: bots-cmd-layout
status: delivered
updated: 2026-09-11
branch: main
commits: 13a4e71..HEAD
---

# bots 迁包 + cmd/uploader + -rate 接线

## Report

**What was built** — 将 Telegram/QQ 机器人迁入 `internal/bots`（状态宽表经 `StatusFn`/`QueueFn` 注入，避免依赖 `api/http`）；入口收为 `cmd/uploader/{main,webglue}.go`；控制台页面改由 `web` 包 `//go:embed index.html` 提供；CLI `-rate` 接入 `config.Rate` 与 `app.CurrentRate`（`>0` 时覆盖日夜限速）。根目录不再保留 `package main` 业务文件。构建入口统一为 `go build ./cmd/uploader`。

**Verification** — `go build ./...`、`go build -o uploader.exe ./cmd/uploader`、`go test ./... -count=1`、`go vet ./...` 均 PASS；`internal/app` 新增 `CurrentRate` 手动覆盖/日夜回落单测。

**Journey log** —
1. `go:embed` 不能跨包引用 `../index.html`，故单独建 `web` 包承载 embed。
2. bots 机械迁包后缺 `BuiltinTaskStatus`/`BuiltinPlatform` 别名，补在 `internal/bots/compat.go`。
3. `-rate` 历史上从未接线；本轮以 `config.Rate` 字段（0=关闭）保持旧配置兼容。

## [S1] Problem

engineering-refactor 收口后仍有三处遗留：

1. `telegram_bot.go` / `qq_bot.go`（~2100 行）仍在 `package main`，经 `compat.go` 别名访问 app。
2. 入口仍在仓库根目录，未形成 `refactor-plan.md` 目标中的 `cmd/uploader/`。
3. CLI `-rate` 只解析未生效：`CurrentRate` 仅走日夜表，手动限速被忽略。

## [S2] Design

### 目标结构

```text
cmd/uploader/main.go     flag + app.Run + bots/http 装配
cmd/uploader/webglue.go  httpapi.Server + recorder hooks
web/embed.go             //go:embed index.html
web/index.html
internal/bots/           telegram + qq（经 app/recorder/status 注入）
```

### bots 依赖

- 直接 import：`internal/app`、`internal/recorder`
- 注入（避免 bots → api/http）：
  - `StatusFn() map[string]any` — 系统状态宽表
  - `QueueFn() map[string]any` — 队列计数
- 导出：`InitTelegram` / `InitQQ` / `SendTelegramNotification` / `SendQQNotification`

### -rate 契约

- `config.json` 新增可选字段 `"rate": 0`（0=关闭，沿用 day/night；>0 强制该 MB/s）。
- 旧配置无此字段时反序列化为 0，行为与历史一致。
- `app.CLI.Rate` 与 `config.CLI.Rate` 贯通；`CurrentRate`：`Rate>0` 则覆盖，否则 `ratelimit.Select`。

### 兼容

- REST / CLI 参数名 / 数据文件不变。
- `restart.sh` 仍依赖可执行文件旁的脚本路径（OTA 用 `filepath.Dir(exe)`）。

## [S3] Out of Scope

- 改 bots 业务逻辑 / UI
- Vue 前端
- 换通知通道

## Tasks

- [x] T1: web embed 包 + index.html 迁入 — acceptance: `web.IndexHTML` 非空且 build PASS (covers: S2)
- [x] T2: internal/bots 迁 telegram/qq + Status 注入 — acceptance: package main 无 bot 文件；bots 包 build PASS (covers: S2)
- [x] T3: cmd/uploader 入口 + 删除根 main/compat/webglue — acceptance: `go build ./cmd/uploader` PASS；根目录无 package main 业务文件 (covers: S2)
- [x] T4: config/app 接线 -rate — acceptance: Rate>0 时 CurrentRate 返回该值；单测覆盖 (covers: S2)
- [x] T5: 全量 build/test/vet + 更新 build 脚本与文档 — acceptance: 三命令 PASS；build 脚本指向 ./cmd/uploader (covers: S2)
- [ ] T6: 交叉编译部署 192.168.5.10 — acceptance: 服务 active；:8888 200 (covers: S2)
