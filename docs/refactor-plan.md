# Phase 2 — 目标架构与迁移方案

## 目标结构（结合现状裁剪，不机械照搬）

```text
upload/
├── cmd/uploader/main.go          # 仅 flag + app.Run + signal
├── internal/
│   ├── app/                      # 生命周期编排（Phase 末）
│   ├── config/                   # Config 类型 + Load/Save/Validate/Apply
│   ├── fsutil/                   # AtomicWrite / SafeJSON
│   ├── hashstore/                # Hash 缓存与落盘
│   ├── convert/                  # TS→MP4
│   ├── naming/                   # 文件名清洗 / 根目录探测
│   ├── storage/                  # history / success / dirStatus
│   ├── remote/                   # OpenList Client (接口)
│   ├── ratelimit/                # 日/夜限速
│   ├── scanner/                  # 目录扫描 → 候选文件
│   ├── uploader/                 # Task / Queue / WorkerPool / Executor
│   ├── notification/             # Notifier 扇出
│   ├── recorder/                 # builtin + docker 抽象
│   ├── auth/                     # login token + session
│   ├── ws/                       # Hub
│   └── logx/                     # 分级日志 + 前端投递
├── api/http/                     # thin handlers
├── web/ 或 embed index.html      # 前端暂不大改
├── docs/
├── deployments/
└── scripts/
```

## 依赖方向（禁止反向）

```text
cmd → app → {config, scanner, uploader, remote, storage, api}
uploader → {remote, storage, convert, ratelimit, hashstore, notification?}
api/http → services (uploader/scanner/auth/ws)  — 不直接摸全局
recorder / bots → notification, recorder API
```

## 迁移原则

1. **每次只抽 1 个低耦合包**，抽完 `gofmt` + `go test ./...`。
2. 旧 `package main` 函数改为薄包装或直接删除并改调用点。
3. CLI flag 与 `config.json` **扁平结构保持兼容**，内部可嵌套但 JSON 字段名不变。
4. 数据文件路径/格式不变：`config.json`、`uploaded_hash.db`、`upload_success.json`、`dir_status.json`。
5. 先不引入第三方框架。

## 阶段落地顺序

| Phase | 内容 | 验收 |
|-------|------|------|
| A | fsutil + hashstore + naming + convert | test PASS，行为不变 |
| B | config 单源 + 读路径收敛 | 旧 config.json 可加载 |
| C | remote.OpenList 接口 + ratelimit | 上传仍通 |
| D | uploader.Task 状态机 + Queue + Worker(ctx) | 扫描→上传通 |
| E | scanner 只产出候选 | 不再在 scan 里 handleFile |
| F | storage history/success/dirStatus | 落盘仍兼容 |
| G | auth + ws Hub + api/http | 现有 API 路径不变 |
| H | notification Notifier | 通知仍发出 |
| I | recorder 拆分 | 录制/开关仍可用 |
| J | main 瘦身 + signal 优雅退出 | main < ~150 行 |
| K | 文档 + 集成测试骨架 | go vet/test 通过 |

## 明确不做（本阶段）

- 不改 Vue UI 结构与视觉
- 不换数据库
- 不删功能
- 不改对外 REST 路径与 CLI 参数名
