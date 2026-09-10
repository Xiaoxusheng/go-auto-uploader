---
feature: record-screenshot-switches
status: delivered
updated: 2026-02-14
branch: feat/record-screenshot-switches
commits: 6691b0c..HEAD
---

# 录屏/截屏独立开关 + 上传排序 + Bug 修复

## Report

**What was built** — 内置录制引擎支持每个主播独立的「录屏 / 截屏」两个开关：开关以 `,录屏:0/1,截屏:0/1` 后缀写在 `builtin_urls.txt` 行尾，旧名单默认双开；运行时分流为完整录制、仅录不截、仅截不录（短 TS 抽帧后清理）、双关仅探测四种模式；Web 控制台任务表提供双开关并经 `set_flags` API 热更新与落盘。上传队列仍做最大/最小穿插，但绝对最大文件挪到序列末尾，避免开场 Worker 被大文件占死。另修复了抽帧路径 `log.Fatal` 崩溃、水印 `#RRGGBBAA` 与 FFmpeg 颜色格式不兼容、名单解析 `Index`/`LastIndex` 不一致等问题。

**Verification** — `go build -o %TEMP%\uploader_check.exe .` EXIT=0；`go test ./... -count=1` EXIT=0 `ok upload 6.018s`。

**Journey log** —
1. 仅截屏默认沿用用户选的「短暂写临时 TS 再抽帧」，但临时片必须落在上传扫描路径内并主动清理。
2. 评审发现临时 TS 清理仅按 mtime>2min，会话结束会漏删；已改为结束时 force 清空、抽帧失败也清理。
3. 旧格式 `url,别名,录屏:0` 先剥开关再解析别名，否则 roomID 会带上别名残段。
4. 双关空转不应再走「断流缓冲中」，否则状态每 ~45s 横跳。
5. `set_flags` 仅在开关实际变化时才 cancel 正在跑的 FFmpeg，避免无意义断流。

## [S1] Problem

1. 内置录制引擎对每个主播强制「录屏 + 旁路截屏」一起跑，无法按主播选择只录不截、只截不录。
2. 上传队列按体积降序再穿插，开场第一个任务常是最大文件，Worker 被大文件占住，整批队列看起来卡住。
3. 代码中存在若干可修复缺陷（进程级 log.Fatal、水印颜色格式、名单解析不一致等）。

## [S2] Design

### 录屏/截屏开关

- **落盘格式**：`builtin_urls.txt` 行尾追加可选后缀，与主播名单一体热重载：
  - `https://...,主播:名字,录屏:1,截屏:0`
  - `录屏:0/1`、`截屏:0/1` 可省略；省略时默认 **都开（1,1）**，兼容旧文件。
  - 以 `#` 开头仍表示暂停，语义不变。
- **解析顺序**：先解析并剥离 `,录屏/截屏` 后缀，再解析 `,主播:` 或旧别名，避免 `url,别名,录屏:0` 把开关吃进 URL。
- **内存模型**：`builtinTaskFlags sync.Map`，key=`platform_roomID`，value=`{Record, Screenshot bool}`。
- **API**：`/api/v1/builtin_recorder/control` 新增 action `set_flags`，body 含 `platform`、`room_id`、`record`、`screenshot`；仅当值变化时才取消正在跑的 FFmpeg；成功后写回 txt 并广播任务列表。
- **状态回传**：`BuiltinTaskStatus` 增加 `record` / `screenshot` JSON 字段。
- **运行行为**：
  - 仅录屏：FFmpeg 写 TS，不启动旁路抽帧协程，不归档 Screenshots。
  - 仅截屏：FFmpeg 写短分片 TS（segment，无分片则 2 分钟），抽帧后删冷却片；会话结束 force 清空残留；封面与 Screenshots 照常。
  - 录屏+截屏：维持现状（完整 TS + 周期抽帧归档）。
  - 两者皆关：不启动 FFmpeg，仅保持监控状态轮询；不进入断流冷却，状态稳定为「监控中」。
- **前端**：内置引擎任务表增加「录屏 / 截屏」两个 switch，变更即调 `set_flags`；「截屏中」视为活跃。

### 上传排序

- 仍按体积 **降序** 排列后做最大/最小穿插。
- **首包禁止最大文件**：把绝对最大的那一个挪到穿插序列 **末尾**，其余从第二大开始交替，避免开场 Worker 被最大文件长期占满。

### Bug 修复（本规格覆盖范围内）

| # | 位置 | 问题 | 处理 |
|---|------|------|------|
| B1 | `extractBuiltinCoverFromLocalFile` | `os.Executable` 失败时 `log.Fatal` 整进程退出 | 改为跳过水印并继续抽帧 |
| B2 | 水印 `fontcolor` | 前端存 `#FFFFFFE6`，FFmpeg drawtext 需 `0xRRGGBB[AA]` 或颜色名 | 写入滤镜前做格式归一 |
| B3 | `parseBuiltinLine` vs `apiRecorderAdd` | 一处 `Index`、一处 `LastIndex` 解析 `,主播:`，名字含该子串时错切 | 统一 `LastIndex`，并解析新后缀 |
| B4 | 上传穿插 | 开场即最大文件占满 Worker | 见上文排序调整 |

## [S3] Out of Scope

- 外部 Docker DouyinLiveRecorder 的 per-room 配置。
- 上传失败重试策略、限速逻辑、鉴权加密层。
- 不重做前端整体 UI。
- Bot（QQ/TG）侧无开关控制界面（仅状态文案适配「截屏中」）。

## Tasks

- [x] T1: 名单解析/序列化支持 `,录屏:x,截屏:y` 与 flags 内存表 — acceptance: 读旧 txt 默认双开；带后缀行解析正确并可写回 (covers: S2)
- [x] T2: 录制路径按 flags 分流（仅录/仅截/双开/全关） — acceptance: 仅录不写 Screenshots；仅截抽帧后删 TS；全关无 FFmpeg (covers: S2)
- [x] T3: control API `set_flags` + 任务状态字段 + 前端双开关 — acceptance: Web 切换后 txt 更新、列表刷新、重启后恢复 (covers: S2)
- [x] T4: 上传穿插首包不放最大文件 — acceptance: 序列第二大/最小交替，最大在末尾 (covers: S2; B4)
- [x] T5: B1–B3 修复 + 评审中等问题 — acceptance: 无 Fatal 崩溃路径；`#RRGGBBAA` 转 `0x`；解析一致；临时 TS force 清理；双关不横跳 (covers: S2)
- [x] T6: `go test ./...` 与编译通过 — acceptance: 测试与 build 无失败 (covers: 全部)
