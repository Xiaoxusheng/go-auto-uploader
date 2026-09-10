# Go Auto Uploader — Phase 1 架构审计

> 生成时间：基于 commit `ea6f9ef` 工作树  
> 范围：main.go / webapi.go / builtin_recorder.go / qq_bot.go / telegram_bot.go / 测试 / 部署脚本

---

## 1. 当前目录与体量

| 文件 | 行数 | 角色 |
|------|------|------|
| builtin_recorder.go | 3069 | 内置录制 + 平台爬虫 + 加密 + API |
| webapi.go | 1882 | HTTP/WS/鉴权/加密/配置/Docker |
| main.go | 1797 | 启动 + 扫描 + 队列 + Worker + 上传 + Hash + 统计 + 邮件 |
| telegram_bot.go | 1424 | TG UI + 控制 + OTA |
| qq_bot.go | 1051 | QQ UI + 控制 + OTA |
| index.html | ~3850 | 嵌入式 Vue 单页 |
| 测试 | ~800 | auth / recorder / race / ws / convert |

**全部为 `package main`，无 internal 分层，无依赖注入。**

```
go.mod: module upload, go 1.25
依赖: gorilla/websocket, chromedp, telegram-bot-api
```

---

## 2. 运行时模块关系（现状）

```text
flag 解析
   ↓
main()  ──加载 config.json / hash / success / dirStatus──
   ├── go StartWebServer  ──mux + auth + AES session + WS hub──
   ├── go manageWorkers   ──for range globalTaskCh → handleFile──
   ├── go queueStatusLoop / reportLoop / *PersistLoop
   └── for { scan → login? → runOnce → sleep/trigger }
              │
              ├─ WalkDir → fileTask[] → 排序穿插 → globalTaskCh
              │
              handleFile
                 ├─ (可选) convertTSToMP4
                 ├─ detectRoot / cleanFileName / fileHash
                 ├─ upload (OpenList PUT + ProgressReader)
                 ├─ hash / history / successRecords / trendStats
                 └─ delete local
   builtin_recorder (独立 Init)
   qq_bot / telegram_bot (独立 Init，读写全局状态)
```

---

## 3. 全局状态地图（耦合核心）

### main.go
| 状态 | 保护 | 问题 |
|------|------|------|
| `appConfig` + `appConfigMu` | RWMutex | 与 CLI flag 双源 |
| `globalTaskCh` (cap 100000) | channel | 永不 close，无背压策略说明 |
| `enqueuedFiles` sync.Map | 无 | 与 LoadOrStore 存在 TOCTOU |
| `liveTasks` sync.Map | 无 | 成功任务不 GC → 内存泄漏 |
| `queue{Uploading,Success,Fail,Retrying}` | Map+atomic | 仅计数/集合，状态机散落字符串 |
| `history` + `historyMu` | RWMutex | 环形 1000 条 |
| `successRecords` + `successLogMu` | Mutex | 大切片拷贝持锁 |
| `hashCache` sync.Map + `hashFileMu` | 混用 | 追记非原子 rename |
| `token` + `tokenMu` | Mutex | 远端 Alist token |
| `running` + `runningMu` | **在 webapi.go** | 跨文件隐式依赖 |

### webapi.go
| 状态 | 说明 |
|------|------|
| `wsClients` sync.Map + `wsBroadcast` chan 1024 | 无 Hub 结构体 |
| `sessionKeys` sync.Map (cap 4096) | AES PFS 会话 |
| `authSessions` | 登录 token 24h |
| `logs` / `logsMu` / `logChan` | 日志环形缓冲 |
| `dirStatuses` sync.Map | 目录统计 |

### builtin_recorder.go
| 状态 | 说明 |
|------|------|
| 7× sync.Map（ActiveTasks/Status/States/Cancels/Names/Flags/Debounce） | 生命周期靠约定 |
| `builtinConfig` / `builtinCookies` | 裸指针 + Mutex 混用 |
| 3 平台爬虫 + SM3/RC4 + chromedp | 与录制业务缠在一起 |

### bots
- 直接读 `appConfig`、`liveTasks`、`trendStats`、`logs`
- 直接改 `builtinConfig`、`builtinStatusMap`、`builtinCancels`
- 与 TG 共享控制函数（QQ 调用 `tgExecutePreciseControl`）

---

## 4. 并发与生命周期问题

1. **无 context**：所有 daemon 为裸 `for`，不能优雅停机（用户要求 SIGTERM 有序退出当前做不到）。
2. **Worker 不可取消**：pause 只在出队后丢任务；进行中上传继续跑。
3. **`liveTasks` 泄漏**：成功/秒传任务几乎不删除。
4. **成功记录**：`append` 全量持锁；落盘全量重写 JSON。
5. **Hash 文件**：追记而非 temp+rename，崩溃可能半行损坏。
6. **`rand.NewSource` 丢弃返回值**（已知死代码）。
7. **跨文件全局**：`running` 定义在 webapi，main 大量读写。
8. **Windows 无法 `-race`**：本机无 gcc/CGO，race 基线在 Linux CI 做。

---

## 5. HTTP / API 问题

- Handler 内嵌业务：`handleCookies` 写 INI、`handleStreamers` 写名单、`handleRecorderControl` 直接 `docker`、队列清理直接改 Map。
- 响应封装：加密路径 `sendJSONSuccess/Error`；与用户期望的 `{success,data,message}` 近似但字段不完全统一。
- 鉴权：登录 token + 可选 RSA/AES 载荷加密，逻辑正确但与 handler 紧耦合。
- WS：Fan-out 设计可用，但慢客户端丢弃策略、心跳、Hub 结构应独立成包。

---

## 6. 录制 / Bot 问题

- 平台 switch（Douyin/Kuaishou/Soop）重复 8+ 处。
- Notifier 无接口：`sendWeChatNotify` 内部扇出 TG/QQ。
- FFmpeg 进程管理可用，但无统一 `ProcessRunner`。
- OTA 升级逻辑在两个 bot 中复制。

---

## 7. 持久化问题

| 文件 | 写入方式 | 风险 |
|------|----------|------|
| config.json | 直接写 / 有 temp 未统一 | 崩溃截断 |
| upload_success.json | 脏标记合并全量写 | 大文件、持锁 |
| dir_status.json | temp+rename（较好） | — |
| uploaded_hash.db | 追记 | 半行损坏 |
| builtin_urls.txt | 直接写 | 截断 |

需要统一封装 `atomicWriteFile`。

---

## 8. 可测试性

- 几乎无法对上传/队列做单元测试：依赖全局 + 真实 HTTP。
- 现有测试集中在 auth、解析、封面抽帧、WS 压测 — **必须保留迁移**。
- 无 FakeRemoteStorage，无 integration 目录。

---

## 9. 重构优先级（建议顺序）

| 优先级 | 项 | 理由 |
|--------|-----|------|
| P0 | `internal/fsutil` 原子写 + `internal/hashstore` | 低风险、立刻降低损坏面 |
| P0 | `internal/config` 收敛双源配置 | 后续一切依赖 |
| P1 | `internal/convert`（已有独立逻辑） | 刚实现、边界清晰 |
| P1 | `internal/uploader`：Task 类型状态机 + Queue + Worker(ctx) | 核心并发 |
| P1 | `internal/remote`：OpenList 接口 | 可测 |
| P2 | `internal/scanner` | 与 queue 解耦 |
| P2 | `internal/notification` Notifier 接口 | 扇出统一 |
| P2 | `api/http` thin handlers + `internal/ws` Hub | API 可测 |
| P3 | `internal/recorder` 拆分 builtin | 体量大、后置 |
| P3 | bots 依赖注入 | 依赖 Notifier/Recorder |
| P3 | 前端目录拆分 | 不改 UI |

---

## 10. 基线验证命令

```bash
go build ./...
go test ./... -count=1          # 本机 PASS
go test -race ./... -count=1    # 需 Linux/CGO（本机无 gcc，PRE-EXISTING 环境限制）
go vet ./...
```

当前基线：`go test ./...` = **ok upload**。
