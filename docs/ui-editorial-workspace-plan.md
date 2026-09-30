# UI 重构计划 — Editorial Workspace / Automation Workspace

> 状态：执行中（2026-09-30 启动）。本文件是本次 Web UI/UX 重构的唯一计划与进度真值来源。
> 目标：把 Go Auto Uploader 控制台从「沉浸暗色仪表盘」重构为「Editorial Workspace / Automation Workspace」——
> 一个成熟的自动化文件处理工作台，参考 Notion / Linear / GitBook / AI2CC Workspace 的信息架构与视觉语言。
> **不是**把深色页面换成白色，而是重新设计信息架构、页面层级、Shell、表格、列表、设置与移动端。

---

## 1. 当前 UI 结构分析

单文件前端 `web/index.html`（约 7,450 行，477KB，经 `web/embed.go` go:embed 打进二进制）：

- **技术栈**：Vue 3（vendor 自托管）+ axios + ECharts；Arco 已弃用不加载（embed_test 断言）。自定义组件：`x-switch` / `x-select` / `x-pagination` / `x-modal` / `x-drawer` / `x-menu`，`v-tween` 数字滚动指令。
- **安全层**：RSA+AES 加密信道（axios 拦截器 + WS 加密封装），登录 Token 存 localStorage。
- **实时层**：`/ws/live` WebSocket（指数退避重连、心跳、消息原地合并、页签不可见时拦截头像流量）；ECharts ×2（队列环形图、7 日趋势图）。
- **10 个页签**（`menuItems` 数组 + `activeTab` v-show 切换）：
  系统概览(overview) / 实时上传任务(live) / 内置轻量引擎(builtin_recorder，含高光实时队列卡) / 外部引擎 Docker(streamers) / 历史记录(history) / 系统终端日志(logs) / 上传参数(settings) / B站投稿(bilibili) / 姿态训练(pose_training) / 外部引擎凭证(cookies)。
- **CSS**：三个 `<style>` 块约 1,320 行。Token（`--bg/--panel*/--t1..t4/--green*/--line/--r-*`）+ 组件样式 + 姿态/高光动效体系。
- **动效真值**：姿态训练「实时过程」卡按 `docs/pose-live-motion.md` 实现（骨架叠加/传输线流动/到站瞄准框/能量边框），循环动画 ≤3、事件动画 ≤400ms、reduced-motion 全量降级。

## 2. 当前 UI 存在的问题

1. **传统 Admin Dashboard 感**：每页都是「KPI 统计卡行 + 卡片网格」，层级靠卡片堆叠而非排版。
2. **信息架构扁平**：10 个页签一线平铺，无分组；「上传/录制/智能/系统」的业务域没有体现在导航上。
3. **视觉语言过载**：暗色 + 双色光斑背景 + 绿渐变主按钮 + 药丸按钮 + 大量 16px 圆角卡片 + 彩色统计数字，装饰多于信息。
4. **没有 Workspace 骨架**：无全局 Header（面包屑/全局搜索/状态），无右侧 Context（本页目录），长页面（设置/姿态训练）迷路成本高。
5. **设置页**是「大表单网格」，配置项缺 Description，Label 与控件关系弱。
6. **日志页**是黑色终端大卡，与 Editorial 风格冲突（浅色主题下尤其突兀）。
7. **移动端**只是把侧栏压成横向图标条，未真正重新布局。

## 3. 新的信息架构（导航分组）

现有 10 个页签**一个不删**，全部映射进分组树：

```
工作台
  总览            (overview)          上传工作台：状态行 + 正在处理 + 最近完成 + 趋势
  上传队列        (live)              实时上传任务
  上传历史        (history)           历史记录
录制
  直播任务        (builtin_recorder)  内置轻量引擎 + 高光实时队列
  外部引擎        (streamers)         外部引擎 Docker
  引擎凭证        (cookies)           外部引擎凭证
智能
  B站投稿         (bilibili)
  姿态训练        (pose_training)
系统
  运行日志        (logs)
设置
  上传与引擎      (settings)          上传参数（OpenList 对接/并发限速/通知/目录/策略）
```

- 分组可展开/收起（120~180ms，无弹跳），状态存 localStorage；当前页所在组自动展开。
- 说明：规范基线中的「文件」「录制历史」「系统状态」「OpenList」独立页在本系统中不存在对应业务
  （文件扫描内嵌在总览/引擎内、历史记录即上传历史、系统状态在总览页、OpenList 配置在设置页），
  按「业务功能不能丢、也不虚构新后端能力」原则映射如上，不新增后端 API。

## 4. 新的页面结构（App Shell）

```
┌──────────────────────────────────────────────────────────────────┐
│ Header 52px：Go Auto Uploader / 当前页 ｜ 搜索 Ctrl+K ｜ WS·主题·退出 │
├──────────────┬────────────────────────────────────┬──────────────┤
│ Sidebar 240px│ Content（页面标题+描述 / 内容）       │ Context 右栏  │
│ 分组树导航    │ max-width 1200px，排版驱动层级        │ 本页目录（仅  │
│ 底部状态     │                                      │ 长页面显示）  │
└──────────────┴────────────────────────────────────┴──────────────┘
```

- Context 右栏只在有明显章节结构的页面显示：设置、姿态训练、运行日志；点击 scrollIntoView，当前章节高亮（滚动监听），纯文字无卡片无背景块。
- 移动端：Sidebar 转为抽屉（汉堡按钮唤起 + mask），Header 变 52px 移动顶栏（菜单 / 页名 / 搜索），Context 隐藏。

## 5. Design Tokens

沿用现有变量名（姿态/高光动效 CSS 与全部组件样式零改名成本），重定义取值；**默认 Light，`data-theme='dark'` 覆盖**：

| Token | Light | Dark |
|---|---|---|
| `--bg` | #FFFFFF | #0F0F10 |
| `--panel`（Sidebar/次表面） | #FAFAFA | #151516 |
| `--panel-2`（hover） | #F7F7F8 | #1C1C1E |
| `--panel-3`（active） | #F3F4F6 | #232326 |
| `--t1/t2/t3/t4` | #18181B / #71717A / #A1A1AA / #D4D4D8 | #F4F4F5 / #A1A1AA / #71717A / #52525B |
| `--line / --line-strong` | #E4E4E7 / #D4D4D8（实色，1px） | rgba(255,255,255,.08) / .14 |
| `--green（=accent/success）` | #16A34A | #22C55E |
| `--green-bright / --green-dim` | #15803D / rgba(22,163,74,.10) | #4ADE80 / rgba(34,197,94,.14) |
| `--orange / --red / --blue`(+dim) | #D97706 / #DC2626 / #2563EB | #F59E0B / #EF4444 / #3B82F6 |
| `--r-lg/md/sm` | 10px / 8px / 6px（卡片 10px 上限，禁止 16px+） | 同左 |
| `--shadow` | 0 1px 2px rgba(0,0,0,.04)（仅弹层用中阴影） | 0 8px 24px rgba(0,0,0,.5) |

- 删除背景 radial-gradient 光斑、按钮渐变、绿色 glow；黑白灰为主体，语义色只承担状态。
- 新增 token：`--font-mono`、`--header-h: 52px`、`--sb-w: 240px`、`--ctx-w: 200px`。

## 6. Sidebar 设计

- 顶部：绿色圆环标 + 「Go Auto Uploader」两行小字号 Workspace 标识。
- 中部：分组树（组标题 11px/600 灰 + 子项 13px）；子项 active = `--panel-3` 背景 + `--t1` 文字 + 左侧无彩色强调（靠背景与字重）；icon 15px。
- 底部：两行内联状态（扫描引擎 ● 运行中/已暂停 · 实时信道 ● 已连接/断开）+ 退出登录。**不伪造 OpenList 连接状态/版本号**（后端无此信号）。
- 背景 #FAFAFA、border-right 1px `--line`，无阴影无玻璃。

## 7. Header 设计

52px、白底、border-bottom 1px。左：面包屑「Go Auto Uploader / 当前页」。中：全局搜索框（只读按钮样式，点击或 Ctrl+K/⌘K 打开命令面板）。右：WS 状态点（含延迟 title）、主题切换、退出。不用 backdrop-filter、不透明漂浮。

## 8. Content Workspace 设计

- 页面标题 24px/600（不搞 28px+ 巨标题），描述 13px `--t3`；操作按钮在标题行右侧。
- 总览页（去 Dashboard 化）：
  1. **Inline Status Row**：扫描引擎 / 实时信道 / 录制引擎内存 / 上传并发 / 磁盘剩余——纯文字行 + 状态点，无卡片；
  2. **正在处理**：上传任务表（复用 live 表结构），空态有行动指引；
  3. **最近完成**：Editorial 列表（日期分组 + divider + 状态点）；
  4. **7 日上传趋势**（ECharts 重配色：蓝线 + 灰虚线 + 极淡网格）+ **磁盘 TOP5**（横条列表）两栏；
  5. **目录监控**：卡片网格改为一行一目录的表格化列表。
  - 原 5 张统计卡 → 一行紧凑数字摘要（14px tabular-nums，不做巨型数字），队列环形图移除（数据全保留在摘要行）。
- 上传队列/历史/录制任务/凭证：保持 Table/List 结构，重质感（更轻的表头、更干净的行、状态点代替大 Badge）。
- 直播任务页默认表格视图不变；卡片视图保留为用户可切换项（功能不删）。

## 9. Context Sidebar 设计

`pageContexts`（JS 配置）：每个长页面一组锚点 `{id, label}`；右侧栏渲染「本页目录」纯文字列表；点击 scrollIntoView；IntersectionObserver 高亮当前节；≤1100px 隐藏。

## 10. Table / List 设计

- 表头 12px/500 `--t3`、无边框底色；行高 44~48px；行 hover `--panel-2`；分隔线 `--line`。
- 状态一律「● + 文字」（绿=成功/运行，橙=等待/警告，红=失败，灰=中性）；彩色大 Badge 只保留 tag 语义（高光切片标记等），尺寸 11px。
- 空态：小图标 + 标题 + 副文案（+ 行动按钮），禁止 Emoji。

## 11. Settings 设计

设置页改为 **左侧设置导航 + 中间内容 + 右侧 Context**（1100px 以下收纳为页内锚点行）：

- 章节分组（锚点）：引擎对接 / 传输策略 / 通知推送 / 监控目录 / 任务策略。
- 每个配置项 = **Row**：左 Label(13.5px/500) + Description(12px/`--t3`)，右 Control（Input/Select/Switch，宽 ≤320px），行间 divider；桌面左右布局，≤600px 上下布局。
- 保存行：主按钮（黑底白字）+ 撤销 + 保存时间提示。
- 开关项合并进 Row（不再用 `fsw` 小卡片网格）。

## 12. Mobile 设计

- ≤900px：Sidebar 隐藏，Header 变移动顶栏（汉堡 → 侧栏抽屉 + mask；页名居中；搜索图标）。
- 表格 → 允许横向滚动（.tbl-scroll 已有）+ 关键页（上传队列/历史）在窄屏隐藏次要列。
- 统计摘要行换行堆叠；设置 Row 上下布局；Context 隐藏；无横向溢出。

## 13. Dark Mode 设计

- 布局与 Light 完全一致，只覆盖 Token；默认主题改为 Light（已保存偏好的用户保持其选择）。
- Dark：#0F0F10 底 / #151516 表面 / #1C1C1E 次表面，无渐变无光斑；ECharts 按主题切换文字/网格/系列色。
- 姿态/高光动效卡在双主题下均只依赖 Token（绿色系在浅色下用更深的 --green-bright 保证对比）。

## 14. 性能风险与对策

- 不引入新依赖、不加深度 watch、不新增轮询；命令面板搜索基于已加载的响应式数据 + 计算属性懒求值（面板打开才渲染）。
- Context 高亮用单个 IntersectionObserver（rootMargin 收敛），切换页签时 disconnect 重建。
- 保留现有优化：WS 原地合并、`_pending_avatar` 流量拦截、v-tween rAF、reduced-motion 全量降级。
- ECharts 仅在进入 overview/主题切换时渲染（现状保留），队列环形图移除后 `renderChart` 对缺失 DOM 自动 no-op。

## 15. WebSocket / 业务兼容风险与红线

- **不改** axios 拦截器、WS 协议、消息分支、轮询节奏、任何 API 调用与 payload。
- **不改** `web/embed.go`、`web/vendor/`；head 三个 vendor script 原样保留（embed_test 断言）。
- **必须保真的测试锚点**（`web/embed_test.go`）：viewport 可缩放；`v-model.number="builtinSettings.highlight_exit_ratio"` / `highlight_min_ac1` 绑定与脚本侧读写；姿态训练页标记 `{key: 'pose_training', icon: 'i-grid', label: '姿态训练'}`、`activeTab === 'pose_training'`、`/pose_training/summary|clips|thumb?clip=`；无 CDN；无 Arco 引用。
- **不删任何功能**：10 个页签、卡片/表格视图切换、内置引擎设置弹窗（含高光参数/水印/Cookie）、全部确认弹窗、姿态传送带动效体系、高光实时队列卡、加密开关重载提示。
- 姿态/高光动效 CSS（pstage/pose-rail/pose-cart/slot-reticle/pose-skel/hlq-*）原样迁移，只随 Token 换色值；循环动画 ≤3 纪律不变。

## 16. 实施顺序与验证

Phase 3 Tokens+CSS 重写 → Phase 4 Shell（Header/树状 Sidebar/移动顶栏）→ Phase 5 总览页 → Phase 6 各列表页质感 → Phase 7 设置页 Row 化 → Phase 8 命令面板 + Context → Phase 9 Dark/Mobile/无障碍 → Phase 10 验证。

**验证方式**：
- `go vet ./... && go test ./... && go build ./...`（embed_test 全绿 = 锚点无损）；
- 浏览器实测（本地构建起服务或静态打开 mock）：1440/1920 桌面、1024/768 平板、390/375 手机三档截图；
- Light/Dark 双主题、登录页、空态、命令面板、Context 滚动联动逐项过；
- `git diff` 确认只改 `web/index.html`、`docs/ui-editorial-workspace-plan.md`、`.agent/TASK_STATE.md`、`AGENTS.md`（§五 风格速查同步新体系），不触碰他人未提交改动（internal/uploader/*、docs/highlight-progress.md）。

## 17. 明确不做（范围红线）

- 不新增后端端点、不改 Go 业务代码（`web/embed_test.go` 只在断言措辞需要时才允许同步——目标是不改）。
- 不引入任何 UI 框架/构建链/状态管理；不重写为多文件工程。
- 不做：渐变、Glow、Neon、玻璃拟态、粒子、巨型 KPI、彩色统计卡、Emoji 图标、999px 药丸按钮（Tag/状态点除外）、20px+ 圆角、装饰动画。
- 不动：业务 JS 逻辑、API/WS 协议、加密层、轮询与数据流。

---

## 进度记录（随实施更新）

- [x] Phase 3 Design Token + 基础 CSS（三块 style 合并为单块；变量名全部保留、取值按双主题重定义）
- [x] Phase 4 App Shell（Header / 树状 Sidebar / Context / 移动顶栏 + 抽屉）
- [x] Phase 5 总览页 Workspace 化（Inline Status Row + 任务计数条 + 正在处理 + 最近完成 + 趋势/磁盘 + 目录表格）
- [x] Phase 6 列表页（队列/历史/录制/日志）质感重做（状态点化、表格 hairline、日志文档化）
- [x] Phase 7 设置页 Row 化 + 左侧章节导航（Label+Description+Control+Divider）
- [x] Phase 8 命令面板（Ctrl/⌘+K：页面/设置章节/直播任务/操作）
- [x] Phase 9 Dark Mode / Mobile / 无障碍（默认浅色；深色仅覆盖 Token；移动抽屉导航；focus-visible 全局保留）
- [x] Phase 10 全量验证（见下）

## 实施结果（2026-09-30）

**修改文件**：`web/index.html`（唯一产品代码文件）、本计划文档、`AGENTS.md` §五（风格速查同步新体系）、`.agent/TASK_STATE.md`（进度追加）。
未触碰：`web/embed.go`、`web/vendor/`、后端 Go 代码、他人未提交改动（internal/uploader/*、docs/highlight-progress.md）。

**结构变化**：
- CSS：3 块 style → 1 块（约 1,560 行）；Token 双主题（Light 默认 / `[data-theme='dark']` 覆盖），变量名零改动（姿态/高光动效与全部组件样式无迁移成本）。
- Shell：新增 52px Header（面包屑 / 全局搜索 / WS 状态 / 主题 / 退出）、侧栏分组树（工作台·录制·智能·系统·设置，展开态持久化 + 当前组自动展开）、Context 右栏（pose/logs 滚动高亮目录）、移动端顶栏 + 侧栏抽屉（≤900px）。
- 总览页：5 张 KPI 卡 → 无边框统计条 + Inline Status Row；队列环形图移除（数据全保留）；目录卡片 → 表格行；新增「正在处理」「最近完成」编辑式列表（含连接中骨架屏）。
- 设置页：表单网格 → 左侧章节导航 + Row（Label/Description/Control/Divider）；设置章节同时进入命令面板。
- 命令面板：Ctrl/⌘+K，页面 / 设置章节 / 直播任务（已加载快照）/ 操作（扫描、切主题、导出日志），键盘上下+Enter+ESC。
- 页题更名：系统概览→总览(上传工作台)、实时上传任务→上传队列、历史记录→上传历史、内置轻量引擎→直播任务、外部引擎 Docker→外部引擎、外部引擎凭证→引擎凭证、系统终端日志→运行日志、上传参数→设置(上传与引擎)。
- 图表重配色：趋势图蓝线+灰虚线+浅网格；环形图（保留代码路径，UI 已移除）双主题色板。
- 通知/Toast 文案去 Emoji；页面内联硬编码色 → Token。

**与规范的偏差（有意）**：
1. 规范基线导航中的「文件」「录制历史」「OpenList 独立页」在本系统无对应业务——按「不虚构后端能力」原则映射到现有页签（见 §3）。
2. 设置页采用左侧章节导航，不另加右侧 Context（两者内容重复，二选一）；姿态训练/运行日志用右侧 Context。
3. 侧栏底部不显示「OpenList ● 已连接 / 版本号」——后端无此信号，不伪造状态；显示扫描引擎与 WS 真实状态。
4. 移动端表格保留横向滚动（.tbl-scroll）而非逐表重排为 List（v1 权衡，卡片/表格切换能力保留）。

**验证记录**：
- `go vet ./...` ✅ `go test ./...` 22 包全绿（含 embed_test 全部断言：无 CDN / viewport 可缩放 / 高光灰度绑定 / 姿态页签标记）✅ `go build` ✅
- 两段内联 JS `node --check` ✅；静态类覆盖率清查通过（仅 3 个无样式包装类，均为有意）
- 浏览器实测（mock 服务器 `_diag/ui_preview/server.mjs`，1440×900 / 390×844）：
  总览（亮/暗）、上传队列、直播任务（高光队列卡/传送带动效体系完整）、设置、姿态训练（亮/暗，骨架叠加/瞄准框/流动线/日志全在）、运行日志、上传历史、B站投稿（空态）、命令面板（搜索过滤）、移动端总览 + 抽屉导航 —— 全部通过。
- 截图期间发现的 4 个问题已当场修复：①总览/设置两段拼接丢失 `</section>` 导致页签嵌套（切页整页空白）；②桌面端汉堡按钮未隐藏（`.icobtn` 优先级覆盖）；③`href` 触发链接默认下划线（补全局 `text-decoration:none`）；④设置页右栏与左侧导航重复（右栏排除 settings）。
- 注：无头后台标签页存在动画/过渡时钟冻结的截图伪影（pageIn 停在半透明、主题切换过渡停在中途），前台真实浏览器不受影响；截图验收时以注入冻结样式 + 重触发主题属性的方式绕过。

**遗留 / 后续可选**：
- 移动端表格的 List 化重排（上传队列/历史在 ≤600px 隐藏次要列）可作后续迭代。
- 命令面板的「任务」结果目前仅跳转到对应页签，不直接打开详情抽屉（避免跨页状态耦合）。
