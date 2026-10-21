# 配置说明

配置文件：工作目录下 `config.json`（扁平 JSON，字段名保持兼容）。

首次启动：若文件不存在，用 CLI 参数生成默认配置并写盘。

## CLI（与 config 对应）

| Flag | Config 字段 |
|------|-------------|
| `-dirs` | `dirs` |
| `-server` | `remoteServer` |
| `-workers` | `workers` |
| `-rate` | （手动限速，运行时由 day/night 覆盖） |
| `-day-rate` | `dayRate` |
| `-night-rate` | `nightRate` |
| `-scan-interval` | `scanInterval` |
| `-report-minutes` | `emailInterval` |
| `-web-port` | （进程监听端口，不在 config） |
| `-live-config` | `liveConfigPath` |
| `-recorder-container` | `recorderContainer` |
| `-recorder-config` | `recorderConfigPath` |

## 常用字段

```json
{
  "workers": 5,
  "scanInterval": 30,
  "dirs": ["/data/live"],
  "enableUpload": true,
  "convertMP4": false,
  "enableEncryption": false,
  "remoteServer": "http://127.0.0.1:5244",
  "remoteUser": "admin",
  "remotePass": "",
  "dashboardUser": "",
  "dashboardPass": "",
  "wechatToken": "",
  "telegramToken": "",
  "telegramChatID": 0,
  "qqBotWsUrl": "",
  "qqAdminId": 0
}
```

## 内置录制引擎（`builtin` 段）

内置录制引擎的参数都在 `config.json` 的 `builtin` 对象里，控制台「设置 → 录制」修改后热生效。

### 落盘与存储

| 字段 | 默认 | 说明 |
|------|------|------|
| `save_path` | `./downloads` | 主落盘目录，录制产物按 `<主播>/<日期>/` 归档 |
| `save_path_fallbacks` | `[]` | 备选落盘目录白名单。**为空 = 存储护栏整个关闭**（既不切换也不告警） |
| `min_free_gb` | `10` | 主目录剩余空间低于该值（GB）即切到备选目录。仅在配了 `save_path_fallbacks` 时生效，配 0 会被补成 10 |
| `segment_time` | `15` | 切片时长（分钟），录满自动切下一个文件且录制不中断 |
| `check_interval` | `30` | 开播探测间隔（秒） |
| `screenshot_interval` | `20` | 定期截图间隔（秒），产物存 `<日期>/Screenshots/` |

> 备选目录必须是**专用空目录**：其中的文件与主目录一样按「上传后删源」处理。
> 主目录恢复后新录制会自动切回主目录（主目录优先，无抖动）。

### 画面异常（花屏）检测

录制走 `-c:v copy` 直通落盘，ffmpeg 不解码视频，**色度丢失类花屏不会报错**，只能自己抽帧检测。

| 字段 | 默认 | 说明 |
|------|------|------|
| `flaw_detect_enable` | `false` | 总开关 |
| `flaw_detect_action` | `log` | 命中动作：`log` 只记日志 / `restart` 优雅收尾并重新拉流 |
| `flaw_detect_interval` | `30` | 检测间隔（秒），下限 10 |
| `flaw_detect_ratio` | `0.9` | 异常纯色像素占比阈值（正常直播 ≈0，绿幕 ≈0.79，整屏花屏 1.0） |
| `flaw_max_restarts` | `3` | 单场最多自动重录次数，防止上游持续异常时死循环 |

命中分片会改名 `xxx.ts.flaw` 保留（不会进入上传 / 高光 / 统计流程），便于排查成因。

### 高光切片

| 字段 | 默认 | 说明 |
|------|------|------|
| `highlight_enable` | `false` | 高光总开关 |
| `highlight_motion_weight` / `highlight_audio_weight` | `0.8` / `0.2` | 画面运动量 / 音频能量权重 |
| `highlight_threshold` | `1.2` | 判定阈值（越高越严格） |
| `highlight_min_duration` / `highlight_max_duration` | `8` / `180` | 单段最短 / 最长秒数 |
| `highlight_per_clip` / `highlight_merge_gap` | `3` / `12` | 单片最多产出段数 / 相邻段合并间隔 |
| `highlight_only_upload` | `false` | 只传高光：原片不上传，分析完即删 |
| `highlight_source_retention_days` | `3` | 源片最长保留天数（0 = 关闭兜底清理） |
| `highlight_analyze_workers` | `1` | 高光分析并行路数（1–4） |
| `highlight_pose_gate` | — | 姿态语义门，字段见下 |

`highlight_pose_gate` 子对象（仅 `CGO_ENABLED=1` 构建有效）：

```json
{ "enable": true, "det_min": 0.3, "vis_min": 0.6, "face_max": 0.12, "keep_ratio": 0.3 }
```

> 磁盘剩余空间低于 `max(min_free_gb, 5GB)` 时，高光会**主动让路**：不再认领源片，
> 上传完成后直接删源，认领回收 TTL 也从 2 小时压到 10 分钟 —— 优先保证录制。

## 热更新

Web「配置 → 保存」会：校验 → 整表替换 → 原子写盘 → 触发扫描/报告重置。

**不要**在运行中手改 `config.json` 后指望自动 reload（无 fsnotify）。

## 安全

- 生产必须改掉默认 `admin/admin`（设 `dashboardUser` / `dashboardPass`）
- `remotePass` / 邮件授权码勿提交到仓库

## 相关文件

| 文件 | 用途 |
|------|------|
| `uploaded_hash.db` | 秒传哈希（每行一个） |
| `upload_success.json` | 成功记录 |
| `dir_status.json` | 目录统计 |
| `builtin_config.json` | 内置录制参数 |
| `builtin_urls.txt` | 内置主播名单（可带 `,录屏:x,截屏:y`） |
