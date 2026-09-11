---
feature: engineering-refactor
status: in-progress
updated: 2026-09-11
branch: main
commits: e1f42e8..HEAD
---

# 工程化重构（分阶段抽包）

## Report

（交付时填写）

## [S1] Problem

功能正确但工程结构难维护：全局变量地狱、无 context 生命周期、Handler 内嵌业务、持久化不统一、难测。Phase 1–I 已抽出 16 个 `internal/*` 包，但 `main.go` 仍持有扫描/上传/报告编排（~1200 行），`webapi.go` 承载全部 HTTP Handler（~1500 行），未达 `main <150 行` 与 `api/http thin handlers` 目标。

## [S2] Design

见 `docs/architecture.md` 与 `docs/refactor-plan.md`。依赖方向：cmd → app → 领域包；api/http 不直接摸 main 全局。

本轮收口设计：

```text
cmd/main.go          仅 flag + app.Run + bots/web 注入（≤150 行）
compat.go            bots/tests 薄别名 → app / recorder
webglue.go           httpapi.Server 启动 + recorder hooks 装配
internal/app/        生命周期单源：state/run/scan/upload/report/streamers/notify/hubs
api/http/            Server + 全部 REST/WS Handler，依赖经方法注入 app 状态
telegram_bot.go      仍留 package main（经 compat 访问 app）
qq_bot.go            仍留 package main（经 compat 访问 app）
```

契约：

- CLI 参数名、`config.json` 扁平 JSON 字段、REST 路径 `/api/v1/*`、`/ws/live` 不变。
- 数据文件路径/格式不变：`config.json`、`uploaded_hash.db`、`upload_success.json`、`dir_status.json`。
- 业务状态真源在 `internal/app` 导出变量；`api/http` 与 bots 通过该包访问。
- 通知扇出：`app.NotifyHub` + `SetNotifyChannel`；main 注入 TG/QQ。
- 内置录制 HTTP 回调经 `recorder.SetHooks` 注入，避免 recorder→main 反向依赖。

## [S3] Out of Scope

- Vue UI 重做
- 换数据库
- 删功能 / 改 API 路径 / 改 CLI 参数名
- bots 迁出 package main（遗留，经 compat 别名访问）

## Tasks

- [x] T1: 抽出 fsutil/hashstore/naming/convert — acceptance: test PASS (covers: S2)
- [x] T2: config Store 单源 — acceptance: 旧 config.json 可加载 (covers: S2)
- [x] T3: remote OpenList — acceptance: 上传路径编译+测试通过 (covers: S2)
- [x] T4: uploader Queue/WorkerPool + 优雅停机 — acceptance: test PASS (covers: S2)
- [x] T5: scanner 解耦 — acceptance: test PASS (covers: S2)
- [x] T6: storage 三店 — acceptance: test PASS (covers: S2)
- [x] T7: ws Hub — acceptance: test PASS (covers: S2)
- [x] T8: notification Hub — acceptance: test PASS (covers: S2)
- [x] T9: recorder Docker + docs — acceptance: test PASS + 文档齐全 (covers: S2)
- [x] T10a: auth SessionStore/Middleware — acceptance: test PASS (covers: S2)
- [x] T10b: recorder 名单行/开关纯函数 — acceptance: test PASS (covers: S2)
- [x] T10d: cryptox RSA/AES 会话 + logx 日志环 — acceptance: test PASS (covers: S2)
- [x] T10c: builtin_recorder 运行时迁 internal/recorder + api/http 路由 + Pipeline — acceptance: test PASS (covers: S2)
- [x] T11: main 编排迁入 internal/app（upload/report/streamers/notify/hubs） — acceptance: main.go ≤150 行且 `go test ./...` PASS (covers: S2)
- [x] T12: webapi Handler 拆到 api/http 并注入依赖 — acceptance: webapi.go 删除；REST/WS 测试迁至 api/http 且 PASS (covers: S2)
- [x] T13: 全量验证 + 交叉编译部署 192.168.5.10 — acceptance: build/test/vet PASS；服务 active；:8888 HTTP 200 (covers: S2)
- [ ] T14: 同步 architecture.md / refactor-plan.md 遗留状态 — acceptance: 文档不再声称 webapi/main 内联编排 (covers: S2)
