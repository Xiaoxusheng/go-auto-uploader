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
