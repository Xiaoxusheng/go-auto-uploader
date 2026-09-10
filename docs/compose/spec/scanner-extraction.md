---
feature: scanner-extraction
status: designed
updated: 2026-09-10
branch: feat/record-screenshot-switches
commits: 
---

# Scanner 与上传解耦

## Report

## [S1] Problem

`runOnce` 同时负责：目录遍历、0 字节清理、录制中判定、目录统计、排序穿插、入队。扫描与上传生命周期耦合，无法单独测试，也难以复用候选过滤规则。

## [S2] Design

### 包边界

`internal/scanner` 只产出候选文件，不调用 `handleFile`、不碰远端、不改 `dirStatuses` 业务语义之外的状态。

```go
type Candidate struct {
    Path string
    Size int64
    ModTime time.Time
}

type Options struct {
    Dirs           []string
    SkipFreshFor   time.Duration // 默认 2min：最近写入视为录制中
    IsQueued       func(path string) bool
    OnActiveFile   func(path string) // 仍在写入的文件
    OnZeroByte     func(path string) // 已删除的 0 字节文件
    OnSkipArtifact func(path string) // .part/.tmp
}

func Scan(ctx context.Context, opts Options) (active int, candidates []Candidate, err error)
```

### 调度策略（仍由 main 决定）

`scanner` 返回未排序候选；`main.runOnce` 继续负责：
1. 体积降序
2. 最大/最小穿插，绝对最大文件放末尾
3. `taskQueue.Enqueue`

### 目录统计

扫描过程中对每个 root 累加 `PendingFiles`/`TotalSize` 由 main 在遍历回调里完成，或 scanner 提供 `Visit` 钩子。本阶段：scanner 返回完整候选与 active 计数；main 在调用前后重置/汇总 `dirStatuses`（逻辑保持现状，仅文件遍历下沉）。

### 0 字节与中间产物

- `*.part` / `*.tmp`：跳过，回调通知
- 0 字节：物理删除，回调通知
- `ModTime < SkipFreshFor`：计为 active，不入候选

## [S3] Out of Scope

- 不改上传排序策略
- 不改 dirStatus 落盘格式
- 不做 fsnotify 实时监视

## Tasks

- [ ] T1: 实现 `scanner.Scan` 与 Options 钩子 — acceptance: 单元测试覆盖 fresh/zero/artifact/多目录 (covers: S2)
- [ ] T2: `runOnce` 改为调用 Scan，排序入队逻辑仍在 main — acceptance: 行为与现网一致，`go test` 通过 (covers: S2)
- [ ] T3: `go build` / `go test ./...` / `go vet` — acceptance: 全部 PASS (covers: S2)
