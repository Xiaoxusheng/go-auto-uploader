# UI SaaS 精修计划（V2.1）— ui-saas-polish-plan.md

> 状态：执行中（2026-09-30）。承接 `docs/ui-editorial-workspace-plan.md` 第三轮（Premium Minimal SaaS Dashboard）。
> **本轮不换设计方向**，只做：视觉系统统一 + 信息层级精修 + 组件质量提升。所有改动纯前端 `web/index.html`。

## 当前问题（自查）

1. **KPI spark 视觉重复**：4 张 KPI 卡有 3 张用同一组 7 日趋势柱——数据重复冒充多源。
2. **系统状态卡是「第五张 Card」**：两段 sys-block 占位过大，层级高于信息价值。
3. **进度条是绿色**：规范要求进度 = 主色蓝、失败 = 红。
4. **直播卡片状态是红色实心大药丸**（LIVE），视觉过重；脚注渐变 .82 偏深；封面占位图是渐变。
5. **趋势图有渐变面积填充**（规范禁止渐变面积）。
6. **外部引擎页首卡过重**：图标块 + 独立卡，应为一行状态摘要。
7. **最近完成日期分组**是「9月30日」式，规范要「今天/昨天」相对标签。
8. **Inline style 散落**（颜色/布局），部分死 CSS（.ov-status/.sys-* 在状态条改造后）。
9. **Token 微差**：边框 #EAECEF→#E8EAED、strong→#DADDE2、muted→#8A8F98、disabled→#B8BCC3、hover 阴影 16px、Modal 阴影独立 token、Input 圆角 9px、搜索框 34px。

## 改造方案

| # | 项 | 方案 |
|---|---|---|
| 1 | Token | --line #E8EAED / --line-strong #DADDE2 / --t3 #8A8F98 / --t4 #B8BCC3 / --shadow-hover 16px / 新增 --shadow-modal（0 12px 40px .12）/ .inp 圆角 9px / .hd-search 34px / grid-main 1.6:1 |
| 2 | Overview 状态区 | 两段 sys-block 卡 → **Compact Status Bar**（单行白底 12px 圆角横条：引擎/下次扫描/信道/并发/内存/磁盘/运行时长 + 休眠/狂暴/RSA chips），移除 .ov-status/.sys-* 死 CSS |
| 3 | KPI 去重 | KPI1 保留 7 日 spark 柱；KPI2 改**成功率进度条**（success/(success+failed) 真实数据）；KPI3 改「● 实时」chip；KPI4 保留 LIVE 标签——四种视觉、全部真实数据 |
| 4 | 最近完成 | 日期标签 → 今天 / 昨天 / M月D日 |
| 5 | 趋势图 | 移除渐变面积填充（quiet line）；主线 #1677FF（暗 #4096FF） |
| 6 | 进度条 | .prog 由绿改**蓝**（失败 orange→保留失败橙？规范：失败 red——改 .prog .bar.orange→.prog .bar.danger 红） |
| 7 | 直播卡 | .live-chip.on 红色实心块 → 深色 scrim 药丸 + 红点呼吸；脚注渐变 .82→.55；.rec-cv 占位渐变→纯色 |
| 8 | 外部引擎 | 容器卡 → 一行状态摘要（.status-bar 复用：Docker 状态/容器名 + 右侧控制按钮），移除 pg-tools 重复 chip |
| 9 | 行内样式 | 新增语义类 .text-danger/.text-success/.text-blue/.text-warning，替换高频颜色行内样式（有限清理，不造 utility 海） |
| 10 | 移动端 | ≤900px Header 只留 Menu/页名/搜索（隐藏主题/退出按钮，退出在抽屉内） |

## 数据诚实红线

不伪造 vs yesterday / 30 天 / CPU / Memory 等不存在数据；成功率来自 queueStatus 真实计数；spark 只在 KPI1（唯一有真实时序的卡）。

## 验收标准

- 浏览器 1440 桌面：Overview（紧凑状态条/KPI 差异化/无渐变面积/蓝进度）、直播任务（轻 card + table）、外部引擎一行摘要、设置分节卡；
- 暗色 #111113/#18181B 同构；390 移动端零溢出、Header 只剩三元素；
- `grep linear-gradient` 仅剩媒体缩略图 overlay 与姿态/高光动效体系（docs/pose-live-motion.md 真值，不在本轮范围）；
- embed_test 锚点无损；`go vet/test/build` 全绿；无 console.log/debugger。
