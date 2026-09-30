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

视觉语言：**Premium Minimal SaaS Analytics Dashboard（浅灰 App 底 #F5F6F8 · 白色卡片 · 轻边框轻阴影 · 蓝色主色 #1677FF）**。2026-09-30 第三轮定稿（Editorial 文档风已废弃：无 Context 右栏、无文档三栏）。Token 真值来源是 `web/index.html` 顶部 `:root`（浅色默认）与 `html[data-theme='dark']`（#111113/#18181B/#202023）两个变量块，新样式只许引用变量，禁止硬编码颜色。

**Shell**
- 布局：Sidebar 232px（白底，静态分组标签 工作台/录制/智能/系统/设置 + 38px 菜单项，active #F1F3F5 + 蓝 icon）+ Header 56px（面包屑 / 搜索框灰底 10px 圆角 / WS 状态 / 主题 / 退出）+ 内容区 max-width 1400px（灰底上放白卡）。
- 命令面板 Ctrl/⌘+K（.cmdk-*，openCmdk()）；移动端 ≤900px 侧栏抽屉化。

**颜色与层级**
- 浅色：App 底 `--bg` #F5F6F8；Sidebar/Header `--panel` #FFF；卡片 `--card` #FFF；hover `--panel-2` / active `--panel-3` #F1F3F5。深色：#111113 底 / #18181B 卡与侧栏 / #202023 次面。
- 语义色：主色蓝 `--blue` #1677FF（主按钮/链接/激活 icon/环图）、绿成功、橙警告、红危险、紫 #7C3AED 仅用于图表；颜色只用于状态与图表，禁止大面积彩色卡。
- 卡片 = 主要视觉容器：`.card` 白底 + 1px `--line` + `--shadow`(0 2px 8px .03)；hover `--shadow-hover`。禁止渐变/Glow/厚阴影。
- 圆角：卡片 `--r-lg` 14px / 控件 `--r-md` 10px / 小元素与按钮 `--r-sm` 8px；999px 药丸仅限 chip/tag/状态点。

**必须复用的现有类**：`.pill`（`.primary` 蓝底白字）、`.icobtn`、`.card`、`.kpi-grid`+`.kpi-card`（KPI：标题/30px 数字/.spark 迷你柱/kpi-info）、`.activity-strip`（任务动态+`.avatars`）、`.store-body`+`.sl-row`（存储环图图例）、`.statrow`（白卡统计条）、`.chip`/`.dot`/`.tag`、`.inp`（36px 圆角 8 focus 蓝环）、`.segpills`、`.toolbar`、`.sec-head`、`.ed-list`/`.ed-item`/`.ed-date`、`.set-layout`+`.set-nav`+`.set-sec`（分节卡）、`.skel-rows`、`table.tbl`+`.tbl-empty`、`.col-sm-hide`（≤640px 列裁剪）；弹层组件 `x-modal`/`x-drawer`/`x-select`/`x-pagination`/`x-menu`/`x-switch`；`.toasts`/`.notifies`。

**排版**：系统字体栈，基准 14px/1.6；页头 h1 30px/600（Overview）、描述 13px；卡片标题 13.5px/600；KPI 数字 30px/600 tabular-nums；辅助 11–12px；`--font-mono` 等宽。

**动效**：120–260ms ease-out；卡片 hover 阴影 150ms；循环动画预算 ≤3（姿态/高光卡保留 docs/pose-live-motion.md 体系）；`prefers-reduced-motion` 全量降级。

**响应式**：≤1200px 网格降列/KPI 2 列；≤640px KPI 1 列、表格列裁剪、min-width 480；≤900px 侧栏抽屉。无障碍：`:focus-visible` 蓝 outline、图标按钮 aria-label、ESC 关闭弹层；禁止 `user-scalable=no`。

**z-index**：下拉 60–70 < 弹出卡 80 < 侧栏抽屉 94/95 < 模态 100 < 命令面板 120 < toast 200 < 通知 210 < 图片预览 300。
