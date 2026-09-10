---
feature: engineering-refactor
status: delivered
updated: 2026-09-10
branch: feat/record-screenshot-switches
commits: 6691b0c..HEAD
---

# 工程化重构（分阶段抽包）

## Report

**What was built** — 在不改对外 CLI/REST/配置 JSON 字段的前提下，将「巨型 package main」逐步拆为 `internal/*` 领域包：配置单源、原子写、秒传哈希、TS→MP4、OpenList 客户端、可取消 Worker 池、扫描候选、历史/成功/目录统计、WebSocket Hub、通知扇出、Docker 录制控制。`main`/`webapi` 改为调用 Store/Hub/Client，功能路径保持兼容。SIGINT/SIGTERM 可优雅停 Worker。

**Verification** — 每阶段 `go build ./...`、`go test ./...`、`go vet ./...` 均 PASS；最终全绿含 internal 各包单测。

**Journey log** —
1. `.gitignore` 裸 `uploader` 会忽略 `internal/uploader`，改为 `/uploader`。
2. `safeBaseDir` 变更导致 `DetectStreamer` 取错段，抽包时顺手修复。
3. `TrendSnapshot` 曾拷贝 `sync.Mutex`，改 DTO。
4. 裸 `dirStatuses.Load`→`Get` 后残留 `val.(*DirStatus)` 会编译失败，批量替换需扫全库。
5. builtin_recorder (~2700 行) 仍留 main，作为下一阶段边界。

## [S1] Problem

功能正确但工程结构难维护：全局变量地狱、无 context 生命周期、Handler 内嵌业务、持久化不统一、难测。

## [S2] Design

见 `docs/architecture.md` 与 `docs/refactor-plan.md`。依赖方向：cmd → internal 领域包；禁止业务包反向依赖 main。

## [S3] Out of Scope

- Vue UI 重做
- 换数据库
- 删功能 / 改 API 路径
- builtin_recorder 全量拆包（遗留）

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
- [ ] T10c: builtin_recorder 运行时 / api-http / main&lt;150 行 — acceptance: 待后续 (covers: S2)
