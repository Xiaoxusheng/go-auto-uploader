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

视觉语言：**沉浸暗色 · 单一强调色（绿）· 药丸按钮 · 低透明度白线分层**。真值来源是 `web/index.html` 顶部 `:root` CSS 变量块，新样式只许引用变量，禁止硬编码颜色。

**颜色与层级**
- Surface 四层：`--bg`（页面底 #070908）→ `--panel`（卡片）→ `--panel-2`（hover/次面）→ `--panel-3`（激活态）；背景由双 radial-gradient 光斑（绿+蓝）+ 底色构成。
- 文字四级灰度：`--t1` 主文 → `--t2` 次文 → `--t3` 辅助/标签 → `--t4` 弱化；层级靠灰度表达，不靠加粗堆砌。
- 语义色只用 5 个：绿 `--green`（唯一强调色，主按钮/激活/成功）、蓝（信息）、红（危险）、橙（警告）、青；每个语义色必须配 `*-dim` 半透明底（如 `--green-dim`）做标签/告警底色，品牌色占比控制在 5–15%。
- 分层靠 `--line`（rgba 白线 7%）而非阴影；阴影只用 `--shadow`，克制。
- 圆角阶梯：卡片 `--r-lg` 16px / 次级卡 `--r-md` 12px / 小元素 `--r-sm` 9px / 按钮与徽章 999px 药丸。

**必须复用的现有类，禁止自造第二套**：`.pill`（按钮，`.primary` 绿渐变主按钮 / `.danger` / `.sm`）、`.icobtn`、`.card`/`.card-head`/`.card-title`、`.stat-card`+`.statrow`（统计卡，数字 26px/700）、`.chip`、`.dot`（状态点，`.run` 带呼吸发光）、`.tag`（blue/green/red/orange/gray）、`.inp`（统一 focus 绿边框）、`.toolbar`、`.sec-head`/`.sec-title`、`table.tbl`+`.tbl-empty`（空态：大图标+标题+副文案三行式）；弹窗/抽屉/下拉用现成的 `x-modal`/`x-drawer`/`x-select` 组件；通知用 `.toasts`（顶部居中）与 `.notifies`（右下）。

**排版**：系统字体栈（PingFang SC / Microsoft YaHei），基准 14px/1.5；页头 h1 23px/700，卡片标题 14px/700，辅助文字 11–12.5px；一切数字加 `font-variant-numeric: tabular-nums`。

**动效**：只服务状态变化/空间关系/操作反馈，120–350ms、ease-out；按钮按压 `scale(0.97)`；页面切换 pageIn 浮入；弹窗 mask 渐隐+卡片浮入；禁止漂浮/发光/粒子等装饰动画；`prefers-reduced-motion` 全量降级必须保留。

**响应式三断点**：≤1200px 网格降列；≤900px 侧栏转为顶部横向图标条（文字隐藏，不是简单堆叠）、统计卡 2 列；≤600px 表单/卡片单列、h1 降至 19px。新增页面按同一断点出三档布局。

**主题与层级**：任何新样式必须同时适配 `html[data-theme='light']`（同一变量被亮色值覆盖，无需写双份样式）；z-index 只用现有阶梯：下拉 60–70 < 弹出卡 80 < 抽屉 90/91 < 模态 100 < toast 200 < 通知 210 < 图片预览 300。
