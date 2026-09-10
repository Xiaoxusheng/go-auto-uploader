---
feature: scanner-extraction
status: delivered
updated: 2026-09-10
branch: feat/record-screenshot-switches
commits: 4bf9664..4e8dc59
---

# Scanner 与上传解耦

## Report

**What was built** — 目录遍历下沉到 `internal/scanner`：`Scan` 并发扫描多个 root，通过钩子区分录制中 / 0 字节清理 / 中间产物 / 已入队 / 可上传候选。`runOnce` 保留目录统计重置、体积排序、最大文件后置穿插与 `taskQueue.Enqueue`。`enableUpload=false` 时强制 `IsQueued=true`，只统计不入队。评审后修正：`OnError` 仅首次触发 `SendAlert`，避免权限树刷屏；补充多目录与 `IsRunning` 测试。

**Verification** — `go build ./...` PASS；`go test ./...` PASS（含 `upload/internal/scanner`）；`go vet ./...` PASS。

**Journey log** —
1. 扫描与上传耦合在 `runOnce` 的 Walk 回调里，拆包时用钩子保持 `dirStatus` 统计语义不变。
2. 评审指出 per-path `SendAlert` 会刷屏，改为原子计数仅首次告警。
3. `enableUpload` 用 `IsQueued` 恒真表达「只统计」，语义略拧但零额外字段。

## [S1] Problem

`runOnce` 同时负责：目录遍历、0 字节清理、录制中判定、目录统计、排序穿插、入队。扫描与上传生命周期耦合，无法单独测试，也难以复用候选过滤规则。

## [S2] Design

### 包边界

`internal/scanner` 只产出候选文件，不调用 `handleFile`、不碰远端。

```go
type Candidate struct {
    Path string
    Size int64
    ModTime time.Time
}

type Options struct {
    Dirs           []string
    SkipFreshFor   time.Duration // 默认 2min
    IsQueued       func(path string) bool
    OnFile         func(root, path string, size int64)
    OnActive       func(path string)
    OnZeroByte     func(path string)
    OnArtifact     func(path string)
    OnError        func(path string, err error)
    IsRunning      func() bool
}

type Result struct {
    Active     int32
    Candidates []Candidate
}

func Scan(ctx context.Context, opts Options) Result
```

### 调度策略（仍由 main 决定）

1. 体积降序
2. 最大/最小穿插，绝对最大文件放末尾
3. `taskQueue.Enqueue`

### 错误告警

`OnError` 在 main 中仅对首次错误 `SendAlert`，其余只打日志。

## [S3] Out of Scope

- 不改上传排序策略
- 不改 dirStatus 落盘格式
- 不做 fsnotify 实时监视

## Tasks

- [x] T1: 实现 `scanner.Scan` 与 Options 钩子 — acceptance: 单元测试覆盖 fresh/zero/artifact/多目录/IsRunning (covers: S2)
- [x] T2: `runOnce` 改为调用 Scan，排序入队逻辑仍在 main；OnError 仅首次告警 — acceptance: 行为与现网一致，`go test` 通过 (covers: S2)
- [x] T3: `go build` / `go test ./...` / `go vet` — acceptance: 全部 PASS (covers: S2)
