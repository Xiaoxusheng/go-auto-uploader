# AGENTS.md — Agent 工作约定

本文件对所有在本仓库工作的 AI Agent 生效（ZCode 每次会话自动加载）。以下规则为硬性约束，与用户指令冲突时先向用户确认。

## 一、Skill 调用硬规则（最高优先级）

**动手写/改代码之前，必须先通过 Skill 工具加载对应规范，不允许跳过：**

| 任务类型 | 必须加载的 skill |
| --- | --- |
| 任何 Go 代码改动（`api/`、`cmd/`、录制/上传/高光/投稿逻辑、并发、HTTP、文件、部署） | `go-dev-standard` |
| 任何 Web UI 改动（`web/index.html`、样式、交互、暗色/移动端适配） | `frontend-dev-standard`；涉及视觉设计、页面美化时加 `ui-design-master` |
| 中大型多阶段任务（新模块、大型重构、跨多文件连续修改、"继续上次任务"） | `long-task-execution` |
| git commit（写提交信息、暂存与分组） | `git-commit-standard` |
| 多 agent 并行协作开发 | `multi-agent-parallel-dev` |
| 生成 PPT / PDF / Excel / banner | `presentations:pptx` / `pdf` / `spreadsheets:xlsx` / `banner-design` |

- 不确定是否命中时，**宁可加载也不要裸写**。
- 用户点名"按 xx 规范写"或 `/skill-name` 时，无条件加载对应 skill。
- 子代理/后台任务启动 prompt 中必须显式写明上述要求，skill 不会自动继承给子代理。

## 二、项目速览

**Go Auto Uploader**：直播自动录制 · 智能切片归档 · 7x24 无人值守一体化系统。

- **架构**：Go 后端 + Vue 3 单文件控制台（`web/index.html`，经 `//go:embed` 打进二进制，单文件部署）
- **入口**：`cmd/uploader/`（主程序）、`cmd/hleval/`（高光判定离线评估工具）
- **HTTP 层**：`api/http/`（router / server / handlers / ws 等）
- **录制引擎**：抖音/快手/B站/Twitch/SOOP，FFmpeg 直拉流落盘，双引擎兼容外置 Docker
- **高光切片**：画面运动量 + 音频能量双因子判定，删源安全闸，B站自动投稿队列
- **姿态语义门**：ONNX（yolov8n-pose）推理 + 全自动训练管线（`api/http/pose_training.go`）

## 三、关键约定

- **构建**：`build.sh` / `build.bat`（`CGO_ENABLED=0 go build ./cmd/uploader`）；Linux 服务器部署走 `deploy_server/`，注意 CGO 差异。
- **测试**：改 `api/http/` 后必须跑 `go test ./...`，不许只编译不测。
- **敏感与运行时文件禁止入库**：`builtin_cookies.json*`、`bilibili_config.json*`、`config.json*` 等配置备份，以及录制产物（`*.mp4`、`downloads/`、`covers/`、`data/`、`wm_shot_*.txt`）。这些不是源码，不要改动、不要提交。
- **提交信息**：Conventional Commits 中文格式（`type(scope): 描述`）。
- **待实现规范（动效）**：`docs/pose-live-motion.md` —— 姿态训练页「实时过程」卡未来感动效（骨架叠加/传输线流动/到站瞄框等）的唯一实现真值来源，动这块代码前必读；配套视觉稿在 Ardot 画布 728995486208370 场景 F09。

## 四、改动原则

- **先读后改**：修改前先读目标文件现状，匹配相邻代码的注释密度与命名风格。
- **最小修改**：不顺手重构，不引入第二套平行实现。
- **每步验证**：改完即编译/测试，不攒到最后一起验。
- **单一真相来源**：同一逻辑只改一处；发现重复实现时先报告，不擅自择一删除。

## 五、Web 控制台 UI 风格速查（新增页面/组件必须遵守）

视觉语言：**Editorial Workspace / Automation Workspace（浅色默认 · 黑白灰为主体 · 排版驱动层级 · 细边框 · 大量留白）**。2026-09-30 全量重构，设计与实施真值来源：`docs/ui-editorial-workspace-plan.md`；Token 真值来源是 `web/index.html` 顶部 `:root`（浅色默认）与 `html[data-theme='dark']`（仅覆盖取值）两个变量块，新样式只许引用变量，禁止硬编码颜色。

**Shell（三栏 Workspace）**
- 布局：侧栏 240px（`--sb-w`，分组树：工作台/录制/智能/系统/设置，展开态存 localStorage）+ Header 52px（`--header-h`：面包屑 / 全局搜索 Ctrl+K / WS 状态 / 主题 / 退出）+ 内容区（max-width 1120px）+ Context 右栏（`--ctx-w`，仅姿态训练/运行日志显示本页目录）。
- 移动端 ≤900px：侧栏转抽屉（汉堡按钮 + mask），Header 收缩为图标；设置页左导航转横向 chips。

**颜色与层级**
- 浅色（默认）：`--bg` #ffffff / `--panel` #fafafa / `--panel-2` hover / `--panel-3` 激活；深色：#0f0f10 / #151516 / #1c1c1e / #262629。双主题布局完全一致，只覆盖 Token。
- 文字四级灰度 `--t1..t4`；语义色绿(成功/运行)/蓝(信息/链接)/红(危险)/橙(警告) 各配 `*-dim` 半透明底；颜色只承担状态，占比 5–15%。
- 分层靠 1px 实色 `--line`（浅）/ rgba 白线（深），禁止阴影堆砌（`--shadow`/`--shadow-pop` 只给弹层）；禁止渐变、Glow、光斑背景。
- 圆角阶梯：卡片 `--r-lg` 10px / 次级 `--r-md` 8px / 小元素 `--r-sm` 6px；按钮 7px；999px 药丸只允许 chip/tag/状态点。

**必须复用的现有类，禁止自造第二套**：`.pill`（按钮，`.primary` 黑底白字 / `.danger` / `.sm` / `.xs`）、`.icobtn`、`.card`/`.card-head`/`.card-title`、`.statrow`+`.stat-card`（无边框统计条，数字 21px/650）、`.chip`、`.dot`（状态点）、`.tag`、`.inp`（focus 黑边框 + `--ring`）、`.segpills`（分段筛选）、`.toolbar`、`.sec-head`/`.sec-title`、`.ed-list`/`.ed-item`（编辑式列表）、`.set-row` 系列（设置行：Label+Description+Control+Divider）、`.skel-rows`（骨架屏）、`table.tbl`+`.tbl-empty`；弹窗/抽屉/下拉/分页/菜单/开关用现成 `x-modal`/`x-drawer`/`x-select`/`x-pagination`/`x-menu`/`x-switch`；通知用 `.toasts` 与 `.notifies`；命令面板 `.cmdk-*`（openCmdk()）。

**排版**：系统字体栈，基准 14px/1.6；页头 h1 24px/600、描述 13px；卡片标题 13.5px/600；辅助文字 11–13px；等宽用 `--font-mono`；一切数字 `font-variant-numeric: tabular-nums`。层级靠字号/字重/间距/divider，不靠颜色和卡片堆叠。

**动效**：只服务状态变化/空间关系/操作反馈，120–260ms、ease-out；循环动画预算 ≤3（姿态/高光卡保留 docs/pose-live-motion.md 体系原样）；禁止漂浮/发光/粒子/装饰动画；`prefers-reduced-motion` 全量降级必须保留。

**响应式三断点**：≤1200px 网格降列、Context 隐藏(≤1100px)；≤900px 侧栏抽屉化、统计条 2 列；≤600px 表单/设置行单列、h1 20px、编辑式列表块状堆叠。

**无障碍**：交互控件保留 `:focus-visible`（蓝 outline）；图标按钮必须有 `aria-label`；弹窗/抽屉/命令面板支持 ESC；禁止 `user-scalable=no`（embed_test 断言）。

**z-index 阶梯**：下拉 60–70 < 弹出卡 80 < 侧栏抽屉 94/95 < 模态 100 < 命令面板 120 < toast 200 < 通知 210 < 图片预览 300。
