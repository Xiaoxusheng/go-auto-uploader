# 姿态训练「实时过程」未来感动效 · 实现规范

> 状态：**P0 / P1 / P2 已全部实现**（2026-09-26）。视觉稿在 Ardot 画布 728995486208370 场景 F09（`23:1`）。
> 实现与文档的三处有意差异（以后者为准）：
> ① 到站回弹用 **WAAPI**（`cartEl.animate`，watch `poseCart.slot`）而非 `.settle` class——与 `poseCartIn` 入场动画同元素，class 切换会顶掉入场动画重放；
> ② P2 blueprint 网格收窄到 `.pose-belt`（传送带区）而非整卡 ::before，避免压住卡内其他内容；
> ③ 轨道（`.pose-rail::before`）保持**静止细线**，旧的 `beltFlow` 虚线动画已删除——流动虚线只保留步进连接线 `.pstage-flow` 一套，且 `.pstage.lit::before` 置 `opacity:0` 让位，防止实线+虚线叠成双线。
> 已知遗留：`POSE_SKEL_DEMO=true`（骨架为 DEMO 轮播，真值接口 `GET /pose_training/skeleton` 待后端实现，见 §2 数据两条路）。
> 复现/验收工具：`_diag/ui_preview/server.mjs`（已带 pose_training mock，运行中状态）+ `node _diag/ui_preview/pose_check.mjs`（5 张分区截图到 `_diag/ui_shots/pose_*.png`）。

## 0. 硬性前提（先读）

1. **改 `web/index.html` 之前必须先加载 `frontend-dev-standard` skill**（AGENTS.md 第一节要求，不许跳过）。
2. 只动 CSS / 模板动效层 / 少量 Vue watch，**不碰业务逻辑、API 结构、数据流**。
3. 所有新样式只引用 `:root` CSS 变量（`--green` / `--green-dim` / `--line` / `--panel-*` / `--r-*`），**禁止新颜色字面量、禁止引外部资源**（`web/embed_test.go` 会断言 index.html 无 CDN 引用）。
4. 改完必须 `go test ./...`。
5. 动效节奏纪律（违反即返工）：
   - **同时循环的动画 ≤ 3 个**：呼吸点(1.6s) / 扫描+骨架(≈2.4s) / 能量边框巡游(8s)
   - **事件驱动动画 ≤ 400ms**（到站收拢、读数闪绿、缩略条入场等）
   - 主缓动沿用系统 token：120–350ms ease-out；循环类用 linear / ease-in-out
   - `prefers-reduced-motion` 全量降级（见 §6）
   - 不新增装饰粒子/漂浮/彩虹渐变——科技感来自"让机器的思考过程可见"，不来自特效堆砌

## 1. 现有动画清单（已存在，不要重复造）

| 名称 | 位置 | 参数 |
|---|---|---|
| `poseBreath` | 激活步进点辉光 | 1.6s ease infinite |
| `poseScan` | 小车扫描光带 | 1.8s ease-in-out infinite |
| `poseCartIn` | 小车入场 | 0.35s ease backwards |
| `poseDivert` | 帧不足幽灵坠落 | 1.8s ease forwards |
| `poseFadeUp` | 已入池/跳过缩略条 | 0.24s ease backwards（stagger 50ms） |
| `dotPulse` | run 状态点 | 1.6s ease infinite |
| cart 位移 | `.pose-cart` left transition | 0.4s ease |
| `v-tween` | KPI/队列数字滚动 | 0.6s |

## 2. P0-A 姿态骨架叠加（灵魂项）

**目标**：小车画面上实时可见姿态推理结果——关键点点亮 + 骨骼描线 + 检测角框 + HUD 读数。

**DOM**：`.pose-cart` 内、`<img>` 之后、`.pose-media-scrim` **之前**（scrim 的底部渐变保持在最上层保证文字可读）：

```html
<svg class="pose-skel" viewBox="0 0 150 84" preserveAspectRatio="none" aria-hidden="true">
  <path class="sk-bones" d="..."/><!-- 骨骼线，一个 path 多段 M -->
  <circle class="sk-p" v-for="(p,i) in poseSkel.kps" :key="poseSkel.epoch + '-' + i"
          :cx="p[0]" :cy="p[1]" r="2.1"/>
  <path class="sk-box" d="M38 16V6H50M110 6H122V16M122 74V84H110M50 84H38V74"/>
</svg>
<span class="pose-skel-hud">KEYPOINTS 17 · det {{ poseSkel.det }}</span>
```

**CSS**（颜色全部走变量）：

```css
.pose-skel { position:absolute; inset:0; width:100%; height:100%; pointer-events:none; }
.pose-skel .sk-bones { stroke: var(--green-bright); stroke-width:1.3; fill:none; opacity:.85;
  stroke-linecap:round; stroke-dasharray:64; stroke-dashoffset:64; animation: skDraw .3s ease-out forwards; }
.pose-skel .sk-p { fill: var(--green-bright); opacity:0; transform-box:fill-box; transform-origin:center;
  animation: skPop .18s ease-out forwards; }
.pose-skel .sk-p:nth-child(n) { animation-delay: calc(var(--i) * 30ms); } /* Vue 绑定 style="--i" */
.pose-skel .sk-box { stroke: var(--green-bright); stroke-width:1.5; fill:none; opacity:.9; }
.pose-skel-hud { position:absolute; top:7px; left:6px; font:500 8px 'Cascadia Mono',monospace;
  color: var(--green-bright); opacity:.95; letter-spacing:.2px; }
@keyframes skDraw { to { stroke-dashoffset:0; } }
@keyframes skPop { from { opacity:0; transform:scale(.4);} to { opacity:1; transform:scale(1);} }
/* 扫描线扫过时骨架常驻态：动画结束后整体降到 20% 透明度，随下一帧刷新重放 */
.pose-cart .pose-skel.idle { opacity:.2; }
```

**Vue**：`poseSkel = reactive({ kps:[], bones:[], det:null, epoch:0 })`；watch `poseCart.clip` / 抽帧信号时换数据并 `epoch++`（`:key` 变化重放入场动画）；800ms 后加 `.idle`。

**数据两条路（不许伪造判定值）**：
- 正路：后端加 `GET /api/v1/pose_training/skeleton?clip=&idx=` → `{w,h,kps:[[x,y,v]×17],det}`，从 pose ONNX 推理缓存取，无缓存返回 204、前端保留上帧骨架。后端改动单独开任务。
- 未就绪时：前端 `POSE_SKEL_DEMO = true` + `DEMO_SKELETONS` 预置骨架循环轮播，**代码注释必须标明 DEMO**，det 显示接口真值（没有就不显示数字）。

**DEMO 骨架**（150×84 框内，COCO 子集 13 点）：

```js
const DEMO_SKELETONS = [{
  kps: [[78,14],[60,24],[96,22],[48,38],[108,36],[44,52],[116,30],
        [64,50],[90,48],[58,66],[96,64],[52,80],[102,80]],
  bones: [[0,1],[0,2],[1,3],[3,5],[2,4],[4,6],[1,7],[2,8],[7,8],[7,9],[9,11],[8,10],[10,12]],
}];
// 再补 2-3 组不同舞姿变体即可轮播
```

**降级**：reduced-motion 时跳过 stagger/draw-on，直接显示终态骨架。

## 3. P0-B 传输线流动

步进条连接线从死线变流动虚线（只在 `.lit` / `.done` 的段显示）：

```css
.pstage .pstage-flow { position:absolute; top:11px; right:calc(50% + 16px); left:calc(-50% + 16px);
  height:2px; border-radius:1px; opacity:0; transition:opacity .25s ease;
  background: repeating-linear-gradient(90deg, var(--green) 0 7px, transparent 7px 18px);
  background-size:18px 2px; }
.pstage.lit .pstage-flow { opacity:.9; animation: flowMove 1.2s linear infinite; }
@keyframes flowMove { to { background-position: 18px 0; } }
```

原 `.pstage::before` 底线保留不动，flow 叠在它上面。到站瞬间的事件脉冲（一个 4px 光点沿线跑一次）为可选加分项，用 CSS `@keyframes` + Vue 一次性挂载节点实现，**别做成循环**。

## 4. P0-C 到站 corner-bracket 收拢

四个工位各一个瞄准框（四角 L 形括号），未到站 22% 透明度待命，小车落座瞬间收拢亮起：

```css
.pose-rail .slot-reticle { position:absolute; top:26px; width:40px; height:40px;
  transform:translateX(-50%); pointer-events:none; opacity:.22; transition:opacity .25s ease; }
.slot-reticle::before,.slot-reticle::after,
.slot-reticle i::before,.slot-reticle i::after { content:''; position:absolute; width:10px; height:10px;
  border:1.5px solid var(--green-bright); }
.slot-reticle::before { top:0; left:0; border-right:none; border-bottom:none; }
.slot-reticle::after  { top:0; right:0; border-left:none; border-bottom:none; }
.slot-reticle i::before { bottom:0; left:0; border-right:none; border-top:none; }
.slot-reticle i::after  { bottom:0; right:0; border-left:none; border-top:none; }
.slot-reticle.on { opacity:.9; animation: reticleSnap .2s ease-out; }
@keyframes reticleSnap { from { transform:translateX(-50%) scale(1.5); opacity:0; }
  to { transform:translateX(-50%) scale(1); opacity:.9; } }
/* 四个工位中心：12.5% / 37.5% / 62.5% / 87.5%（与 cart slot 公式 ((slot+0.5)*25)% 一致） */
/* 小车本体落座回弹，挂在 cart 到站判断后加 .settle 一次 */
.pose-cart.settle { animation: cartSettle .12s ease-out; }
@keyframes cartSettle { from { transform:translateX(-50%) scale(1.02); } }
```

Vue：watch `poseCart.slot` → 给对应 reticle 加 `.on`、cart 加 `.settle`，`animationend` 后移除 `.settle`。

## 5. P1 / P2（氛围层，时间紧可砍）

**P1-A 遥测读数闪绿**：`det/vis/帧数` 等真实值变化时给元素加 `.tick-flash` 300ms（`color: var(--green-bright)` 回落），`animationend` 移除。复用 `pose-queue` 里 `b.tick` 的思路。

**P1-B hero 能量边框巡游**：仅实时过程卡。`::after` inset:-1px + conic-gradient(透明 88% → `rgba(34,197,94,.55)`) + mask 挖空成 1px 边框，`--ang` 用 rAF 8s/圈。Firefox 不支持 `@property` 时 JS 兜底，兜不住就放弃此条（标记 optional，不阻塞交付）。

**P2 blueprint 网格**：实时过程卡 `::before` 铺 24px 网格（`rgba(255,255,255,.02)` 双向 linear-gradient），静止，z 序在内容之下。

## 6. prefers-reduced-motion 降级（必须全量）

```css
@media (prefers-reduced-motion: reduce) {
  .pose-skel .sk-p, .pose-skel .sk-bones { animation:none; opacity:1; stroke-dashoffset:0; }
  .pstage.lit .pstage-flow { animation:none; background: var(--green); } /* 虚线变实线 */
  .slot-reticle.on, .pose-cart.settle { animation:none; }
  .card-live::after { display:none; }  /* 边框巡游停用 */
}
```

## 7. 验收清单

- [ ] 同时循环动画 ≤ 3（呼吸点 / 扫描+骨架 / 边框巡游）
- [ ] 所有事件动画 ≤ 400ms；主缓动 120–350ms ease-out
- [ ] 无新颜色字面量（全部 `var(--…)`）；无外部资源引用
- [ ] reduced-motion 降级生效（骨架终态、流动线实线、巡游停用）
- [ ] `go test ./...` 通过
- [ ] 骨架数据：接了真接口，或 DEMO 有明确注释且 det 只显示真值
- [ ] 主观题：整体像"正在思考的精密仪器"，不像霓虹游戏厅

## 8. 视觉对照

Ardot 画布 `728995486208370` → 场景「F09 姿态训练·主视图」（`23:1`）：
小车骨架叠加 / 四工位瞄准框（激活亮、待命 .22）/ 流动虚线连接线均已画好，实现时截图对照即可。
