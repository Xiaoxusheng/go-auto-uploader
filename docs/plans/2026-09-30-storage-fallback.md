# 计划：录制存储溢出护栏——主目录满了自动切备选盘（2026-09-30）

## 背景

7x24 录制持续写盘，主落盘目录所在分区写满后录制直接失败。用户需求：目录快满时自动把新录制切到其他盘，「但不能乱找」——只能在用户显式配置的白名单备选目录中选择。背景事件：TASK_STATE 已记录 D 盘曾跌破 10GB 护栏（守护降级仅重定标）。

## 现状与关键链路（已探明）

- 录制落盘根目录唯一入口：`internal/recorder/builtin_init.go getBuiltinSavePath()`（`config.json` → `builtin.save_path`，默认 `./downloads`）。
  新录制在 `builtin_ffmpeg.go` 组装 `<根>/<主播>/<日期>/`；高光产物 `<根>/<主播>/高光/`。
- `BuiltinConfig = config.BuiltinSettings` 是**类型别名**，config.json 单一真相源，`apiRecorderConfig` POST 走选择性拷贝 + `ApplyDefaults` + `PersistConfig`。
- 磁盘剩余空间探测已有平台实现，但锁在 `api/http` 包（`disk_windows.go` GetDiskFreeSpaceEx / `disk_unix.go` statfs），recorder 不能反向 import（api/http → app → recorder）。
- 上传管线用 `naming.DetectRoot(path, roots)` 反推文件归属根目录，**未命中直接 `[SKIP][NO_ROOT_MATCH]` 丢弃**。roots 来源三处：扫描 `app/scan.go RunOnce(cfg.Dirs)`、实时入库 `app/run.go:133 HandleFile(cfg.Dirs)`、归属反推 `app.DetectRoot`。
- 高光离线调度 `recorder.HighlightTargets` / 热路径过滤 `HighlightOnlyPrefixes` 都只认主目录；统计 `computeRecorderStats`、主播目录体积 `getBuiltinDirSizeStr` 同样只扫主目录。
- 源片兜底清理 `app/highlight.go highlightSweepRoots(AppCfg().Dirs, …)`。

**结论**：只改「录制落盘目录」是不够的——备选目录必须并入扫描/归属/高光/统计/清理全链路，否则文件传不上去也清不掉。

## 方案

主目录剩余空间 < 阈值时，新录制从**白名单备选目录**中选剩余空间最大且 ≥ 阈值的一个；主目录恢复后自动切回（主目录优先，天然无抖动）。全部不足则维持主目录并告警，绝不自作主张乱找盘。已在录制的文件不动（写一半不迁移）。

决策规则（每次新录制开始时评估，无粘性状态）：
1. 未配置备选目录 → 功能关闭，行为与现状完全一致。
2. 主目录剩余 ≥ 阈值（默认 10GB）→ 主目录。
3. 否则备选中剩余 ≥ 阈值且最大者 → 该备选（切换时记日志 + 控制台告警，30 分钟节流）。
4. 备选全不足 → 维持主目录 + 溢出告警（节流）。

## 修改文件

| 文件 | 改动 |
| --- | --- |
| `internal/config/config.go` | `BuiltinSettings` 增 `SavePathFallbacks []string`、`MinFreeGB float64`；`ApplyDefaults` 归一化（trim/去空/去重/去与主目录重复，配置了备选且阈值≤0 时默认 10） |
| `internal/fsutil/freespace_windows.go`、`freespace_unix.go` | 新增 `FreeSpace(path) int64`（跨平台，语义与现 api/http 实现一致：当前用户可用字节，出错 0） |
| `api/http/disk_windows.go`、`disk_unix.go` | 删除平台实现，`getDiskFreeSpaceStd` 委托 `fsutil.FreeSpace`（消除即将产生的重复实现，单一真相源） |
| `internal/recorder/builtin_savepath.go` | 新增：`FallbackRoots`/`RecordRoots`/`ResolveRecordRoot`（决策核心）、`freeSpaceFn` 测试注入点、`AlertHook`（app 注入 SendAlert，recorder 不可反向 import）、告警 30min 节流 |
| `internal/recorder/builtin_ffmpeg.go` | 新录制 `baseDir := ResolveRecordRoot()` |
| `internal/recorder/highlight_target.go` | `HighlightTargets`/`HighlightOnlyPrefixes` 遍历全部 RecordRoots（热路径保持纯内存字符串拼接） |
| `internal/recorder/builtin_init.go` | 主播目录体积跨根求和 |
| `internal/recorder/builtin_stats.go` | `computeRecorderStats` 跨根合并（主播按名字归并、日期目录去重） |
| `internal/recorder/builtin_api.go` | 配置 POST 拷贝新字段（fallbacks nil 判空可清空、MinFreeGB 直接赋值经 ApplyDefaults 归一） |
| `internal/app/run.go` | 新增 `app.ScanRoots()`（cfg.Dirs 原样 + recorder.FallbackRoots，去重）；`HandleFile`/`DetectRoot` 改用它；启动时接线 `recorder.AlertHook = SendAlert` |
| `internal/app/scan.go` | `RunOnce` 扫描根改 `ScanRoots()` |
| `internal/app/highlight.go` | 兜底清理 sweep 改 `ScanRoots()` |
| `api/http/server.go` | `buildStatusData` 目录卡列表改 `ScanRoots()`（备选盘自动出现存储卡） |
| `web/index.html` | 内置设置表单：备选目录（textarea 每行一个）+ 阈值 GB 输入；加载/保存换算 |
| 测试 | `internal/fsutil` FreeSpace、`internal/recorder` 决策矩阵（健康/切换/全满/恢复/未启用）、`internal/app` ScanRoots 合并 |

## 明确不做

- 不做运行中录制文件的迁移（写一半不动，切换只影响新录制）。
- 不做全盘自动扫描探测空闲盘（「不能乱找」：只认白名单）。
- 不把主目录自动加进上传扫描（保持既有 Dirs 语义，避免「录了但不传」的用户被反向改变行为；只并入显式配置的备选目录）。
- 不动 `internal/uploader/pipeline.go`/`pipeline_test.go`（用户未提交改动）。
- 不做备用盘之间的负载均衡/条带化；不做空间预估（按剩余字节数判定）。

## 风险

- 备选目录若指向含其他文件的目录，会被既有扫描语义上传并按删源闸清理 → UI placeholder 与文档注明「专用空目录」。
- 主播目录跨盘分裂：高光/统计/体积已跨根合并，唯「同日期同主播两盘都有」时天数统计按目录名去重合并，极小误差可接受。
- 相对路径备选（如 `../rec2`）：Clean 后照常工作，但建议 UI 引导绝对路径。

## 验收标准

- [ ] `go vet ./... && go test ./... && go build ./...` 全绿
- [ ] 决策矩阵单测通过（未启用/健康/切换最大空闲/全满保持/恢复切回）
- [ ] 备选目录文件可被扫描上传（ScanRoots 生效，HandleFile/DetectRoot 不再 NO_ROOT_MATCH）
- [ ] UI 可配置备选目录与阈值，保存后 config.json 落盘
- [ ] 不触碰用户未提交的 pipeline 改动
