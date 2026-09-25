# 空间特征 / 缺陷 D/E —— 2026-09-24 续训记录

> 口径：**只做跳舞高光**（用户拍板）。目标 = 压礼物特效 / 切近景 / 空镜伪运动（缺陷 D/E）。>   
> 承接上一会话未写盘的「bstd AUC 0.917 过线」结论；本轮**扩样本复核后证伪该阈值**，>   
> 但 **bstd 已写进 `internal/highlight`**，评估链路可继续复用。



---

## 0. 一句话结论

| 项                    | 上一会话            | 本轮扩样本后                                             |
| -------------------- | --------------- | -------------------------------------------------- |
| bstd AUC（舞 vs 伪运动）   | **0.917**（n≈17） | **0.593**（n=48）/ **0.705**（grid_de 人工金标 n=15）      |
| 是否过 0.85 上线门槛        | 宣称过线            | **未过**，方向正确但不可单独作门槛                                |
| MinBStd 后处理门槛        | 待接              | **已接进 Go**；在 9 片空间舞蹈子集上 **压不住 hn**                 |
| hard_neg 误检（A 档全舞蹈集） | 74%             | **74.1%**（基线复现）                                    |
| **ac1 时序门槛**         | —               | **MinAC1=0.15：hn 74%→65%，F1 持平**（已进 config/Select） |
| **ac1+center 秩组合**   | —               | grid_de 扩标 n=24 上 dance vs D/E **AUC 0.922**       |

**结论：单一 bstd 不可上线。真正有效的是 ac1（运动量 lag-1 自相关）时序门槛；  
`highlight_min_ac1: 0.15` 为推荐灰度工作点，默认仍 0。**

---

## 0b. 续训增量（同日第二轮）：ac1 过线

### 金标扩到 24 窗（grid_de_labels，看图补标 14 条）

| 类型      | n            |
| ------- | ------------ |
| dance   | 9            |
| closeup | 9            |
| gift    | 5（含芝瑶全屏小狗礼物） |
| scene   | 1            |

### 特征搜索（`_temporal_stat_search.py`）

| 特征                             | vs gift   | vs closeup | vs all D/E |
| ------------------------------ | --------- | ---------- | ---------- |
| **ac1**（full 的 lag-1 自相关）      | **0.867** | 0.790      | 0.763      |
| bstd_sec_p90                   | 0.711     | 0.617      | 0.637      |
| rule_close（b4/四角）              | 0.600     | 0.716      | 0.696      |
| **rank(ac1)+rank(rule_close)** | **0.956** | **0.914**  | **0.922**  |

- 跳舞=持续摆动（ac1 均值 0.50）；礼物=短促爆发（0.07）；近景=0.24
- ac1 **只需要 Motion**，不依赖空间块 → 可直接进现有 Probe 链路

### Go 落地

| 符号                                      | 说明                                                |
| --------------------------------------- | ------------------------------------------------- |
| `WindowAC1` / `SuppressByAC1`           | 时序门槛                                              |
| `WindowCenterRatio` / `SuppressByClose` | 中心集中度（需 Blocks）                                   |
| `Options.MinAC1` / `MinClose`           | 0=关；MinAC1 已进 config                              |
| `highlight_min_ac1`                     | config + API + `analyzeClip` 走 `SelectWithBlocks` |

### hleval metrics 实测（A 档 th1.2/ml8/gf12）

**全舞蹈 23 片（hn 1012s）—— MinAC1 扫描：**

| minac1   | hn 误检     | secF1     | segF1     | segP      |
| -------- | --------- | --------- | --------- | --------- |
| 0（关）     | 74.1%     | 0.674     | 0.566     | 0.448     |
| **0.15** | **65.0%** | **0.673** | **0.567** | **0.487** |
| 0.2      | 65.0%     | 0.672     | 0.554     | 0.486     |
| 0.3      | 54.0%     | 0.517     | 0.466     | 0.450     |

→ **0.12–0.18 平台**：hn −9pp，F1 几乎不动，segP 上升。**推荐灰度 `highlight_min_ac1: 0.15`。**

**空间 9 片：** minac1=0.15 时 hn 22.8%→13.0%；再加 minclose=2.0 可到 0%，但 segR 崩（0.48→0.27），**暂不上 MinClose**。

---

---

## 1. 本轮落地的代码（`write/edit`，未用 PowerShell 拼 Go 字符串）

### 1.1 `internal/highlight/spatial.go`（新）

| 符号                                    | 作用                                                    |
| ------------------------------------- | ----------------------------------------------------- |
| `Blocks`                              | 按秒 3×3 块运动量（9 列，与 `grid_probe.py` 同定义）                |
| `WindowBStd`                          | 段内「各块时间均值」的总体 std —— 与 `grid_auc.bstd` 同式             |
| `SuppressByBStd`                      | 后处理：丢掉 bstd < minBStd 的段                              |
| `SelectWithBlocks`                    | `Select` + bstd 门槛（`o.MinBStd>0` 且有 Blocks 时生效）       |
| `ExtractBlocks`                       | 一次解码 split 9 crop，`metadata=print:file=`（§19 band 拓扑） |
| `BlocksFromFeatures` / `AttachBlocks` | 与 Features 的 `b0..b8` 列互转                             |

### 1.2 Options / Select / hleval

- `Options.MinBStd`：段级空间门槛，**默认 0 = 关闭**
- `hleval export`：补 `hard_neg` 列（读 labels 的 `negative_hard`）
- `hleval metrics -minbstd`：CSV 含 `b0..b8` 时走 `SelectWithBlocks`
- 单测 `spatial_test.go`：均匀块 vs 局部块、门槛开/关、往返

```bash
go test ./internal/highlight/   # 全绿
go build ./cmd/hleval
```

---

## 2. 金标与空间数据

### 2.1 批量 grid_probe：6 → 18 份

脚本：`_diag/train/batch_grid_probe.py`（调 `grid_probe.py`）。

| 状态        | 切片                                                                                                                                      |
| --------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| ✅ 新补 grid | 温泉水 005/009、爱喝旺仔、小欣耶耶 000/002、喵崽、小仙女 001、卷卷 12-53-36、芝瑶 000/001、Mini kua、软耳猫、珈珈子 011 等                                                  |
| ✅ 原有 grid | 喵崽 000、温泉水 013、VVya 000/001、闪闪 001、发财mm 09-46-17                                                                                        |
| ❌ **缺源片** | **菜菜 15-14-26_000/001**（D/E 最干净实例）、卷卷 07-38-25 / 07-47-58 / 07-57-23 / 08-10-14\_*、小仙女 000、椰椰椰树、caicai-003、VVya 07-04-53 / 07-16-21_000 |

源片多已被上传流水线删除，只剩高光 mp4 / feat.json —— **无源片就提不出空间特征**。

### 2.2 合并产物

- `_diag/train/features_dance_spatial.csv` — 舞蹈 23 片 + b0..b8
- `_diag/train/features_dance_spatial_only.csv` — **9 片空间全覆盖**子集（pos 733s / hn 162s）
- 脚本：`merge_spatial.py`（grid 比 features 常差 1 秒，片内 ffill/bfill；禁止跨片填充）

---

## 3. AUC 扩样本复核（关键否定）

### 3.1 labels 自动扩窗（`_expand_de_auc.py`）

舞蹈片 `positive` vs `negative_hard`，对齐 grid：

| 特征            | AUC (n=48)    | 旧数 (n≈17) |
| ------------- | ------------- | --------- |
| **bstd**      | **0.593**     | 0.917     |
| full（现网主特征）   | 0.511         | 0.833     |
| uni           | 0.409         | 0.617     |
| bratio / lown | 0.526 / 0.539 | 反向        |

窗级均值：舞 bstd 6.22 vs 伪运动 5.42 —— **分布重叠严重**。

### 3.2 人工金标 `grid_de_labels.jsonl`（n=15，正=dance 4，负=gift/closeup/scene 10）

| 特征       | AUC                 |
| -------- | ------------------- |
| **bstd** | **0.705**（仍 < 0.85） |
| full     | 0.545               |
| corr     | 0.523               |
| uni      | 0.432               |

### 3.3 变体（`_bstd_variants_auc.py`）

`bstd_sec`（逐秒块间 std 再平均）0.594 ≈ `bstd_time` 0.593。换定义救不了。

**判定：上一会话 0.917 是 n=17 小样本假象。扩到 48 窗后 bstd 只剩弱信号；0.85 门槛从未真正达成。**

---

## 4. MinBStd 门槛实测（A 档 th1.2 / ml8 / gap12）

### 4.1 全舞蹈集基线（`features_dance_only.csv`，23 片）

| 指标              | 值                   |
| --------------- | ------------------- |
| 秒级 F1           | 0.674               |
| 段级 F1           | 0.566               |
| **hard_neg 误检** | **74.1%**（750/1012） |

### 4.2 空间子集（9 片）+ minbstd 扫描

| minbstd | secF1     | segF1     | hn 误检           |
| ------- | --------- | --------- | --------------- |
| 0（关）    | **0.771** | **0.613** | 22.8%           |
| 0.5–2.0 | 0.75–0.77 | 0.57–0.61 | **22.8%（纹丝不动）** |
| 3–6     | 0.59–0.70 | 0.39–0.53 | **22.8%**       |
| 8       | 0.450     | 0.318     | 10.5%           |

- 门槛 0–6 **完全碰不到 hn 秒**（误检段的 bstd 并不低）
- 要到 8 才砍 hn，但 segF1 0.61→0.32，**代价不可接受**
- 该子集 hn 多为「草稿峰外高运动（礼物/近景**嫌疑**）」，不是已核实的礼物动画/怼脸近景

**判定：MinBStd 单独作后处理门槛，当前数据上无效。**

---

## 5. 为什么没打穿

1. **干净 D/E 源片缺失** —— 菜菜 15-14-26_001（礼物旋转木马 + 近景）与卷卷怼脸近景段无 `.ts`，只剩 feat.json（无空间列）。
2. **草稿 hn ≠ 确认 D/E** —— 「峰外高运动嫌疑」里混着真舞边界与无关运动，金标噪声大。
3. **全帧运动量在窗级也塌了**（AUC 0.51）—— 窗口一宽，z 优势被抹平；与 §19.3 逐秒结论一致。
4. **3×3 bstd 单统计量不够** —— §19.4 说过信息在「哪些块」上；max/mean/std 都是标量压缩。

---

## 6. 工程状态

| 项                                 | 状态                                           |
| --------------------------------- | -------------------------------------------- |
| `internal/highlight` 空间 API + 单测  | ✅                                            |
| `hleval` hard_neg 导出 + `-minbstd` | ✅                                            |
| grid_probe 批量                     | ✅ 18 份                                       |
| MinBStd 上线                        | ❌ **默认 0，不进 config**                         |
| 线上 8080                           | 仍 **A 档** th1.2 / ml8 / gap12 / exit_ratio=0 |
| 产品口径                              | 只做跳舞高光                                       |

---

## 7. 下一步（按性价比）

1. **找回/重录源片**（菜菜 15-14-26、卷卷 07-*）→ 才能复现「干净礼物/近景」上的空间可分性。
2. **补金标**：在**有 grid 的源片**上人工标确认 gift/closeup 窗（≥30 正 + ≥30 负），再算 AUC；不要用「嫌疑」草稿。
3. **换统计量**（§19.4）：块向量直接进模型 / 滑窗块间相关 corr(t) / 运动质心轨迹，而不是单一 bstd。
4. **灰度 highlight_min_ac1: 0.15**（config.json；UI 若丢字段用 API/文件改）。
5. MinBStd / MinClose 代码保留默认关；等组合特征在更大金标上 LOO 稳过 0.85 再开。
6. closeup 仍是硬骨头（ac1 0.79）：若灰度后近景误检仍在，考虑人脸/尺度检测。

---

## 8. Journey log

1. 接手时文档 `docs/highlight-spatial-de.md` 未写盘（上会话工具层失败）；先按历史正文恢复脉络。
2. 按计划把 **bstd 接进 Go**（Select 后处理门槛），全程 `write/edit`，单测全绿。
3. `hleval export` 缺 `hard_neg` 已补；metrics 基线 **74.1%** 与上会话一致。
4. grid_probe 6→18，合并出 9 片空间舞蹈子集。
5. **扩样本后 bstd AUC 0.917→0.593**，0.85 门槛证伪；MinBStd 扫描证明单独门槛压不住 hn。
6. 看图补标 14 窗（含全屏小狗礼物）；**ac1 脱颖而出**（vs gift 0.87），与 center 秩组合 0.922。
7. **MinAC1=0.15 写进 config/Select**：hn 74%→65% 且 F1 持平 —— 第一个可灰度的时序门槛。
8. 负结果（bstd/MinBStd/MinClose）与正结果（ac1）都写入本文。

---

**相关文件**

- 代码：`internal/highlight/spatial.go`、`spatial_test.go`、`highlight.go`（MinBStd）、`cmd/hleval/{main,metrics}.go`
- 脚本：`_diag/train/{batch_grid_probe,merge_spatial,_expand_de_auc,_bstd_window_auc,_bstd_variants_auc}.py`
- 数据：`_diag/train/features_dance_spatial{,_only}.csv`、`labels.jsonl`、`grid_de_labels.jsonl`
- 总进展：`docs/highlight-progress.md`、`docs/highlight-accuracy-plan.md`

---

## 9. 灰度 minac1=0.15（2026-09-24）—— 冻结 A/B 后的定稿

### 已完成

| 项                                                                   | 状态                                         |
| ------------------------------------------------------------------- | ------------------------------------------ |
| `config.json` `"highlight_min_ac1": 0.15`                           | ✅ 已写入                                      |
| `uploader.exe` / `release/uploader.exe` / `release/uploader`（linux） | ✅ 已重编，含 `highlight_min_ac1`                |
| `go test ./...`                                                     | ✅ 全绿                                       |
| 本机 8080 进程                                                          | ✅ 已重启（PID 20080，2026-09-24 19:15），HTTP 200 |

### A/B（A 档 th1.2/ml8/gf12）

**舞蹈 23 片（主指标，hn 1012s）**

|            | hn 误检     | secF1 | segF1 | segP      |
| ---------- | --------- | ----- | ----- | --------- |
| 关          | 74.1%     | 0.674 | 0.566 | 0.448     |
| **开 0.15** | **65.0%** | 0.673 | 0.567 | **0.487** |

**空间 9 片**：hn 22.8%→13.0%，segR 0.63→0.53。

**冻结 test 3 片（开封 #2 —— 本次为 A/B 对照，非调参）**

|        | segF1     | segP / segR   | douyin-dance | soop-perf |
| ------ | --------- | ------------- | ------------ | --------- |
| 关      | **0.957** | 0.917 / 1.000 | 0.997        | 0.874     |
| 开 0.15 | 0.857     | 0.900 / 0.818 | **0.997**    | **0.742** |

- 验收线 seg P≥0.8 且 R≥0.6：**仍过**（0.900 / 0.818）
- **舞蹈域无损**（0.997）
- **SOOP 域 segF1 −13pp**：表演/唱歌类运动自相关偏低，被 ac1 误杀
- 产品口径「只做跳舞高光」→ 主指标接受；SOOP 为次要域，需分域盯

### 上线操作

```json
"highlight_min_ac1": 0.15
```

- **改 config.json / 全量字段 API**，勿用会丢字段的 UI 保存
- 换二进制后 **systemctl restart uploader**（本地 8080 同理）
- 回退：该字段改回 `0` 即关闭
- 远程 Linux 包：`release/uploader`（已含字段）

### 观察指标（灰度期）

1. 舞蹈切片：hn 误检率、段级 R（对照 65% / 0.68）
2. SOOP/表演场：漏检是否变多（对照 0.742）
3. 若舞蹈 R 掉超过 5pp：回退到 0，改 0.12 再试

---

## 10. 续训：LOO 过线 + 磁盘清理（2026-09-24）

### ac1 + rule_close 秩组合 LOO（grid_de n=24）

| 集合              | in-sample | **LOO**   |
| --------------- | --------- | --------- |
| dance vs 全部 D/E | 0.926     | **0.852** |
| vs gift         | 0.956     | **0.911** |
| vs closeup      | 0.914     | **0.889** |

**首次在 LOO 上达到 ≥0.85。** 灰度仍用 minac1=0.15（单门槛、不吃空间）；  
组合门槛需 Blocks（MinClose），待更大金标与 F1 回归后再开。

### 磁盘清理

- 删除 \_trash_20260924 + grid_tmp + 已用尽源片/高光 ≈ **5.3GB+**
- **保留**：未标注采集片、缺 grid 源片、冻结集、录制中文件

---

## 11. 扩标后 LOO 塌掉 —— MinClose 不开（2026-09-24）

grid_de 扩到 **39 窗**（dance 15 / closeup 17 / gift 5 / scene 2 / other 10 不进 AUC）：

| 集合                             | in-sample | **LOO**   | 对比 n=24           |
| ------------------------------ | --------- | --------- | ----------------- |
| dance vs D/E combo(ac1+center) | 0.758     | **0.667** | LOO 0.852 → **塌** |
| vs gift                        | 0.800     | 0.720     | 0.911 → 塌         |
| vs closeup                     | 0.733     | 0.667     | 0.889 → 塌         |
| 单特征最好 rule_close               | 0.728     | —         | 仍 <0.85           |

**与 bstd 0.917 同一课：n=24 的 0.852 也是小样本假象。** 扩 closeup/dance 后全面回落。

### 决定

| 项               | 状态                                         |
| --------------- | ------------------------------------------ |
| **MinClose**    | ❌ **不开**（LOO 未稳过 0.85）                     |
| MinBStd         | ❌ 仍关                                       |
| **MinAC1=0.15** | ✅ 维持灰度（依据是**运营指标** hn−9pp/F1 平，不是 LOO AUC） |
| 下一步             | 不再冲块统计量 AUC；要压近景需人脸/尺度模态，或等确认 gift 金标 ≥30  |

金标累计 grid_de_labels.jsonl：**49 条**（含 other）。

## 12. UI 绑定防丢字段（2026-09-24）

设置页「高光切片」新增：**平滑窗口 / 迟滞退出比 / 时序门槛 min_ac1**。  
saveBuiltinConfig 整包 POST 时不再把这三个键抹成缺省。

- exit_ratio / min_ac1：0=关，前端用 ?? 回落（|| 会误伤合法 0）
- smooth_window：0 非法，回落 5；API UpdateConfig 同步拷贝该字段（原先缺失）
- 产物：三端二进制已重编，8080 已重启（embed UI）

## 13. 工作点网格：th=1.4/ml=12 冻结否决（开封 #3）

舞蹈 train 23 片网格（th × ml × minac1）最优点：

| 工作点                       | hn 误检     | secF1 | segF1     |
| ------------------------- | --------- | ----- | --------- |
| A档 th1.2/ml8 + ac1.15（现行） | 65.0%     | 0.673 | 0.567     |
| **th1.4/ml12 + ac1.15**   | **55.2%** | 0.664 | **0.589** |
| th1.6/ml8 + ac1.15        | 44.8%     | 0.659 | 0.574     |

**冻结 test 开封 #3**（验收 P≥0.8 且 R≥0.6）：


| | segP / segR / segF1 | dance | soop |
| --- | --- | --- | --- |
| th1.4/ml12/ac1.15 | 1.00 / **0.27** / 0.429 ❌ | 0.992 | **0.178** |
| A档 + ac1.15 | 0.90 / 0.82 / 0.857 ✅ | 0.997 | 0.742 |

SOOP/表演段几乎被杀光，**不切换**。配置维持 **th1.2 / ml8 / gap12 / min_ac1 0.15**。
冻结只开封了这一次对照，不再拿 test 调参。

## 14. 训练完视频清理（2026-09-24）

| 删除 | 量 |
| --- | --- |
| 与 feat 缓存对应的源片 + 高光 | 174 个 / **27.9 GB** |
| 标注包 positive 金标切片 | 42 个 / 1.4 GB |
| 特征缓存 / labels / CSV | **全部保留** |

feat 缓存并集：_diag/train/cache(235) + cache_live(119) + shorts_cache(99) + 标注包/cache(116) → 334 clip。
D 盘可用 37→**64 GB**。剩余 523 个视频（53.6 GB）为**未入 feat 的新采集**，不是训练完，保留。
评估链路完整：minac1=0.15 下 hn 65.0% / F1 0.673 可复跑。

## 15. 训练评估迁入 Go（2026-09-24）

hleval 新子命令（与 internal/highlight 同语义，替代 Python 探索脚本）：

| 命令 | 作用 |
| --- | --- |
| atch-probe | 批量提特征；-per-streamer 限量 |
| de-auc | grid.csv + D/E 金标算 ac1/bstd/center AUC（MannWhitneyAUC） |

新采集 23 主播各 1 段已 probe 进 cache_live（ok=23）。
Go de-auc（39 窗）：best **center 0.750**，ac1 0.550 —— 与 Python 扩标结论一致，**无特征过 0.85**。

### Go 训练闭环补齐

- internal/highlight.MannWhitneyAUC + WindowBStdP90（最近秩）+ 单测
- export -cache-dir 支持逗号多目录
- eatures_go_v3.csv：47 片 / 29772 秒（多 cache 并集导出）
- 全量 metrics（A档+minac10.15）：douyin-dance F1 **0.673**，soop-perf 0.810，全量 hn 68.4%

## 16. 扩窗复核：ac1+close 组合门证伪（2026-09-24 23:55 · 本会话）

> 背景：§0b 的「ac1+center 秩组合 AUC 0.922（n=24）」若成立即可冲 D/E AUC≥0.85 上线门槛。
> 本轮用 labels.jsonl 自动扩窗复核——**证伪，且是本项目第二例「小样本过线、扩样推翻」**（第一例 bstd 0.917→0.593）。

### 数据

- 47 片标注源片已全部清理（§14），空间数据仅存 18 份 grid.csv（覆盖 14 片标注切片，7522 秒）
- 自动扩窗 `_auto_expand_windows.py`：positive→dance 窗（≤14s），negative_hard→D/E 窗（按 note 分 gift/chat），
  与手工窗重叠 >50% 去重 → `grid_de_labels_auto.jsonl`（87 窗）
- 金标合计 **136 窗**（dance 58 / gift 21 / closeup 17 / chat 28 / scene 2 / other 10）

### 窗口级 AUC（`_p03_eval.py`，日志 p03_eval_20260924.log）

| 特征 | dance vs 全D/E (n=136) | vs gift (79) | vs chat (86) | vs closeup (75) |
| --- | --- | --- | --- | --- |
| ac1 | 0.571 | 0.631 | 0.658 | 0.494 |
| rule_close | 0.579 | **0.751** | **0.312（反向）** | 0.740 |
| combo 秩组合 | 0.613 | 0.752 | 0.480 | 0.668 |
| combo LOO | 0.592 | 0.689 | 0.600 | 0.570 |

**0.922 → 0.613，证伪。** 关键新认知：chat 的 rule_close 方向相反（聊天者居中说话，
中心集中度比舞者还高）——任何含 center 的组合在含 chat 的负样本集上都会被拖垮。

### 段级回归（Go 现有门，14 片空间子集 features_full_sp18.csv，hn 635s / gt 30 段）

| 门 | hn 误检 | secF1 | segF1 (P/R) | douyin-dance secF1 |
| --- | --- | --- | --- | --- |
| 无门（A 档） | 73.9% | 0.547 | 0.437 (.333/.633) | 0.771 |
| **minac1 0.15（现工作点）** | **64.6%** | 0.536 | **0.432 (.364/.533)** | 0.741 |
| minclose 1.5 | 66.5% | 0.523 | 0.354 | 0.737 |
| minac1 0.15 + minclose 1.5 | 57.2% | 0.514 | 0.358 (.324/.400) | 0.709 |

- minclose ≤1.2 时 hn 一秒未降（删的全是舞蹈段边缘）——窗口级「close 无分离力」在段级复现
- 组合门额外 −7.4pt hn 的代价是 segR −13pt / 舞蹈域 −3.2pt：**不上线**
- AND 门无需新代码（SuppressByAC1∘SuppressByClose 即逐段 AND）；OR 门窗口级已无分离力，不实现

### 结论与下一步

1. **工作点维持 A 档 + minac1 0.15 不变**；D/E AUC≥0.85 上线门槛未达成，「训练完成」继续被挡。
2. 9 块粗粒度运动统计（b0..b8 均值/std）信息量到顶：区分「居中跳舞」vs「居中聊天/近景」
   需要语义级空间特征（人脸/人体检测、姿态、块间光流方向），不是调门槛能解决的。
3. 冻结开封维持 3 次，本轮未开封。
4. 金标资产：grid_de_labels(手工 49) + grid_de_labels_auto(自动 87) + 18 份 grid.csv 保留，
   供后续语义特征复核用；47 片源片已清理，空间数据无法再扩（除非重录）。

## 17. 节律/频谱特征证伪——运动信号类路线终结（2026-09-25 06:45 · 本会话）

- 假设：舞蹈有短语级周期性（2~7s），频谱峰锐度可区分 D/E（1fps 可分辨频段 0.1~0.5Hz）
- 特征：acf_max(lag2~6s) / spec_peak(主峰显著性) / spec_flat / jitter / rhythm_combo / quad_combo
- 结果（136 窗金标，`_rhythm_eval.py` + rhythm_eval_20260925.log）：
  dance vs 全D/E 全部 0.50~0.61；vs chat rhythm_combo 0.468；quad 0.607 —— **证伪**
- 舞蹈窗 spec_freq 中位 0.17Hz：1fps 下舞蹈节律信号与聊天/特效不可分
- **最终结论：运动信号类特征（块统计/自相关/频谱）上限 ~0.6 AUC，全部到顶。**
  D/E AUC≥0.85 仅剩姿态语义特征路线（人体关键点周期性 / bbox 占比 / 脸面积比），
  需要源片抽帧 + 新金标（旧 136 窗源片已删，需从 434 个新源片标注补金标）。

## 18. 姿态语义特征试点——首个过线信号（2026-09-25 07:15 · 本会话）

- 路线：MediaPipe PoseLandmarker(lite, tasks API) 逐帧 33 关键点 → 8s 窗聚合语义特征
- 试点金标：agent 看图标注 6 片 / 335 窗（2 舞蹈片 + 4 非舞蹈：近景聊天×2、连麦、蹲爬），
  `_pose_pilot/`（frames/pose/sheets）+ `_pose_features.py`（pose_eval_20260925.log）
- 窗口级 AUC（dance vs 非dance）：
  - **vis_ratio 0.901**（四肢关键点可见率——全身入镜才是舞）
  - **face_frac 反向 0.938**（脸越大越不是舞，近景判别器）
  - extent_h 反向 0.736 / aspect 反向 0.727（蹲爬=宽扁矮） / limb_motion 0.610
- 片级（6 片）：vis_ratio 0.875、face_frac 反向 1.000
- 分群清晰：舞蹈片 vis_ratio≈0.8 + face≈0.07；近景/连麦 vis_ratio≈0.38 + face≈0.14-0.19；蹲爬 aspect 1.6
- **边界（必须说清）**：①仅 6 片（有效样本=6），窗内高度相关；②agent 标注非人工复核；
  ③选片带舞蹈/非舞蹈先验，偏容易样本 → **这是方向验证，不是 0.85 达标宣称**
- 下一步：扩到 30-60 片 + 用户人工复核金标 → 组合门槛定标 → 段级回归 → 冻结开封
- 工程备注：Windows 中文路径 cv2 必须用 imdecode/imencode；mediapipe 1.0.1 支持 py3.13（tasks API，模型 pose_landmarker_lite.task）

## 18b. 用户复核金标复验（2026-09-25 08:10 · 用户网页复核 133 窗）

- 用户在复核页（_diag/pose_label/，8131）改标/补标 133 窗：
  李知恩（9.30🎂）损坏场全 60 窗=无人 ✅（pose 检出 0% 行为正确）；鲤鱼汤改判 chat+无人段；
  羊羊补标 9 窗 chat；VVya 近景改判 chat
- 用户金标 432 窗（有人 372 / 无人 60），8 片全覆盖
- 窗口级 AUC（dance 120 vs 有人非dance 252）：
  - **vis_ratio 0.904**（复验成立，与 agent 版 0.901 一致）
  - **vis+face 组合 0.869** ✅
  - face_frac 单用 0.843（反向）/ aspect 反向 0.742 / limb 0.633
- 固定门槛 vis≥0.55 且 face≤0.12：舞蹈窗通过 93%，非舞蹈误通过 27%
- 「无人」独立拒绝器：88 个用户标无人窗，pose 检出率均值 11.2%（68% 的窗 <10%）
- 已知弱区：鲤鱼汤型「全身可见但非舞（蹲坐）」vis 0.797 落进舞蹈区间——需 ext/aspect/limb 进组合
- 结论：姿态方向在用户金标下复验成立；下一步扩样 30-60 片 → 组合定标 → 段级回归 → 冻结开封

## 18c. 扩样组合门槛定标——试点乐观被修正（2026-09-25 08:50 · 本会话）

- 扩样：48 片 / 27 主播 / 2048 窗进复核页；用户复核 16 片 / 320 窗，**202 窗与模型预标不同**
- 金标 345 窗（dance 124 / 非dance 221）+ 无人 95；来源：用户点选 225 + 用户确认舞蹈片 120
- 用户金标下（`_pose_eval_v3.py`，pose_eval_v3_20260925.log）：
  - vis_ratio AUC **0.904 → 0.643**；face 反向 0.803；**limb_motion 0.757（最优单特征）**
  - 最优固定门槛 vis≥0.6/face≤0.14/ext 0.5-1.0：**P 46% / R 91% / F1 0.609**
  - 片级固定门槛 8/14
- 模型预标错误 195 窗：dance→closeup 77、dance→other 61、dance→chat 34
  （年年/嘉琦/温泉水_016/D.an 整片错——全身可见+大幅运动的非舞内容几何上酷似舞蹈）
- **结论**：
  1. 几何姿态特征（vis/face/ext/aspect）单独不够 0.85——近景上半身舞、健身/玩耍类全身运动在几何上与舞蹈同构
  2. limb_motion（时序动态）升至最优单特征 → 下一步特征方向=**姿态时序动力学**（四肢位移谱周期、左右协调性）+**音乐节拍耦合**（audio onset × 腕部周期互相关，舞蹈是跟着音乐动的）
  3. 产品化备选：姿态门作为高光段后置过滤器（R 91% / P 46% 在段级基率更高的场景有实际过滤价值），与「0.85 达标」是两回事

## 19. 姿态门段级模拟——后置过滤器价值确认（2026-09-25 09:30 · 本会话）

- 方法：47 片试点（用户金标）→ Go export features_pilot48.csv → Python 复刻 A 档
  （score.go 收缩窗 smooth + robustZ + th1.2/ml8/gf12）选段 → 姿态门后置过滤
  （vis≥0.6 且 face≤0.14 且 ext∈[0.5,1.0]，段内 ≥50% 秒通过才保留）
  （`_seg_gate_sim.py` + seg_gate_sim_20260925.log）
- 结果：
  | | 段数 | 秒数 | 真舞秒 | 非舞秒 | 秒级精确率 |
  | --- | --- | --- | --- | --- | --- |
  | A 档预测 | 100 | 2796 | 977 | 1819 | 34.9% |
  | **姿态门后** | **42** | **1040** | **846** | **194** | **81.3%** |
  - **非舞秒砍除 89.3%，真舞秒损失 13.4%，舞蹈段保留 36/40**
- 大头砍除：鲤鱼汤 237s→0、之秋秋 124s→0、苏子液 215s→0、闲闲饭 119s→0（聊天/无人场全砍）
- 保留误差：温泉水_016（近景舞 35s，用户标 closeup）等 194s 残余 FP
- **边界（必读）**：①门槛在 345 窗标定集上选的，模拟为 in-sample，偏乐观——真数字要冻结开封 #4；
  ②「整片都跳」片的 robustZ 压平漏检未解决（另一缺陷，门不管漏检）；③Go 集成未做
- 决策含义：P 35%→81% @ R 89% 的后置过滤器已值得进入「Go 集成 → 冻结开封 #4」流程

## 20. 姿态门 Go 集成 M1/M2 完成（2026-09-25 09:45 · 本会话）

- 运行时：onnxruntime 1.30.0 win-x64（_vendor/onnxruntime/）+ yalue/onnxruntime_go v1.36
  + winlibs mingw64 gcc 16.2（_vendor/mingw64/，仅构建期需要）
- 模型：yolov8n-pose.onnx（13.5MB，ultralytics 导出，_vendor/）
- 新包 internal/pose：构建标签隔离——cgo 构建 = 完整 ONNX 推理；纯 Go 构建 = 桩
  （姿态门自动关闭），uploader 主程序保持零 cgo
  - pose.go（共用）：FrameFeatures/FeaturesFromFrame/GatePass/SegmentGatePass
  - pose_cgo.go：Detector/DetectImage（letterbox→YOLOv8-pose 解码→NMS）
  - pose_nocgo.go：桩；pose_test.go/debug_test.go：冒烟+语义方向测试
- 冒烟结果：舞蹈片检出 11/12、vis 0.915、face 0.086；近景片 face 0.146（方向正确）；
  全仓 21 包 CGO 全绿，纯 Go 构建通过
- 环境踩坑：gcc 16 需 ASCII TMP 路径（中文用户名炸汇编器）；cgo 需要 CC 显式指向 mingw gcc；
  onnxruntime_go v1.36 要配 ORT 1.30（API 29）
- 剩余 M4：录制链路接入（抽帧 hook + config 字段 highlight_pose_gate 默认关 + 回落）
  + hleval 姿态门评估路径 + 冻结开封 #4

## 20b. 姿态门 Go 集成完成 + YOLO 特征迁移问题（2026-09-25 10:20 · 本会话）

- **M4 完成**：config `highlight_pose_gate`（默认关 + 参数回落）；app analyzeClip hook
  （门后无段→记空状态）；hleval `pose-scan` + `metrics -pose`（Go 全链路评估）；
  internal/pose GateOptions/FilterSegments（cgo 实现 + nocgo 桩恒放行）
- 双构建验证：纯 Go 构建通过（门自动关）；CGO 构建 21 包全绿
- **YOLO 特征迁移问题（关键发现）**：MediaPipe 定标门槛（vis≥0.6/face≤0.14）在
  YOLO 特征上把舞蹈秒砍掉 90%——两后端的 kpt 置信度/检出率分布不可迁移：
  - MediaPipe：近景检出 100%，YOLO：1/12（脸占满时人检不出）
  - 运动模糊下 YOLO kpt conf 整体走低（舞蹈窗 det_rate 中位仍 100%，但窗内波动大）
- YOLO 重定标（用户金标 345 窗，_yolo_gate_calib.py）：最优 **P 85% @ R 50%**
  （vis≥0.5/face≤0.14/ext 0.4-1.2）；「无人」拒绝器极强（det_rate 中位 0%）
- **段级实测（定标门槛）**：P 26% / R 2.4% —— 未能复现 MediaPipe 模拟的 81%/89%：
  逐秒 YOLO 检出稀疏（运动模糊）导致段级门噪声大；det=0 秒放行的设计让 FP 在
  未检出区段存活
- **结论：方向已验证（MediaPipe sim 上限 P81/R89），但 Go 版（YOLO 特征）尚未
  达到可部署精度。** 下一步候选：①特征工程（det_rate 本身作为特征、时序平滑、
  limb 周期性）；②换 MediaPipe 级别关键点质量的 ONNX 模型（RTMO/整批转换）；
  ③段级（而非秒级）门槛定标；④维持 A 档现状，姿态门继续离线迭代
- 配置样例（当前默认关闭，灰度需显式开启）：
  `"highlight_pose_gate": {"enable":true,"vis_min":0.5,"face_max":0.14,"ext_min":0.4,"ext_max":1.2,"keep_ratio":0.5}`

## 21. 路线解耦 + 姿态门 5fps 设计 + traincmd 防静默偏置（2026-09-25 11:20 · 本会话）

- **解耦（用户拍板）**：「姿态门灰度」与「0.85 AUC 门槛」正式分离。
  姿态门上线证据 = 运营指标（产出切片 hn 误检、舞蹈域召回保持）+ 灰度观察；
  0.85 仅约束「判别特征解决 D/E」的研究主张。四肢时序动力学/节拍耦合能否到
  0.85 是未知数——到不了就以后置门 + 运营指标收官（诚实终点）。
  ⚠️ 当前 Go 门尚未复现模拟增益（§20b 段级 P26），灰度前仍需特征/模型迭代。
- **抽帧率定版 5fps**：1fps 奈奎斯特 0.5Hz 看不到 1.7~2.3Hz 节拍；5fps 奈奎斯特
  2.5Hz 全覆盖，节拍耦合特征将来免重抽。Hook 已按 o.FPS 接线
  （config `highlight_pose_gate.fps`，默认 5，非法回落），只对已选段抽帧，
  成本低于全程 1fps。⚠️ 1fps 定标的 vis/face/ext 门槛在 5fps 下需重定标
  （pose-scan 即可出数），灰度前完成。
- **traincmd.go 防静默偏置**：①训练折单一类别跳过的折现在显式告警
  （「N 折被跳过未计入合计」+ 切片名）；②CSV 解析失败行计数 + stderr 告警。
  两个数字不再无声偏置聚合指标。

## 22. v3 段级聚合门 + YOLO 特征前沿（2026-09-25 11:35 · 本会话）

- hleval metrics -pose 增 -pose-mode seg（段级均值聚合，与模拟同构）/sec（旧逐秒投票）
- YOLO 特征前沿（48 片试点，用户金标，秒级）：
  | 门槛 | P | R | F1 |
  | 基线 | 0.329 | 0.103 | 0.157 |
  | vis0.45/face0.16/ext0.3-1.3/detmin0.05 | **0.399** | **0.102** | **0.162** |
  | vis0.5/face0.14/ext0.4-1.2/detmin0.05 | 0.572 | 0.038 | 0.072 |
  | vis0.55+/face0.14（紧） | 0.743-1.000 | 0.009-0.011 | — |
- 结论：段级聚合修复了 v2 逐秒投票的召回崩塌（R 恢复到基线水平）；
  YOLO 特征可给 **P +21% @ R 基本无损** 的温和门——可灰度但增益有限；
  MediaPipe 级 P81 增益在 YOLO 特征下不可复现，差距=关键点质量，
  下一个杠杆是换 RTMO 级 ONNX 模型（估算 1-2 天：模型获取+解码移植+重定标）。
- 部署矩阵确认：服务器/发布构建 CGO_ENABLED=0 纯 Go（门自动关），
  Windows 灰度构建 CGO=1（门可用）——build.sh/build.bat 已天然满足

## 23. 姿态门灰度上线（2026-09-25 11:45 · 本会话）

- **灰度已生效**：uploader.exe 换 CGO 构建（纯 Go 版备份 uploader_purego_backup.exe），
  onnxruntime.dll(1.30) + yolov8n-pose.onnx 部署到 D:/upload/，
  config highlight_pose_gate enable=true（**温和门槛** vis0.45/face0.16/ext0.3-1.3/keep0.5/fps5），
  8080 已重启（HTTP 200），5 个主播录制中——首个切片完成即首次实战
- 部署矩阵：仅 Windows 本机构建带门；服务器 build.sh 纯 Go（门编译剔除），符合「服务器纯 Go」要求
- **回退**：config highlight_pose_gate.enable=false（或删除该对象）→ 重启 uploader.exe；
  彻底回退 = 恢复 uploader_purego_backup.exe
- 观察口径（运营指标，替代 0.85 AUC）：
  1. 产出高光里非舞占比是否下降（对照：姿态门前约 65% 预测秒为非舞）
  2. 舞蹈主播的高光产出量是否保持（温和门设计目标 R≈100%）
  3. 姿态门日志（🧍 行）砍段比例是否在 15-25% 区间（模拟预期）
- 已知边界：温和门增益有限（模拟 P+21%@R无损）；更激进门限会伤召回——
  等 RTMO 级模型/特征迭代后再评估收紧

## 23b. 灰度首例生产验证（2026-09-25 11:55 · 本会话）

- 橙夏_2026-09-25_11-22-51_001（新主播，未参与任何训练/定标）：
  A 档选 3 段 → 姿态门砍 3/3 → 看图复核 479 帧确认**全程坐床聊天、零舞蹈**
- 即：姿态门在生产首个实战就正确拦下 3 个假高光（无门则会产出并可能上传）
- 观察期继续：重点盯「舞蹈主播场次」是否有误杀真舞（near case = 温泉水_016 近景舞）

## 24. 服务器部署完成（2026-09-25 12:35 · 本会话）

- 服务器（xyx.homes CentOS 7）：/home/upload/uploader 已换最新构建（纯 Go Linux，
  A档+minac1 生效，姿态门按「服务器纯 Go」要求编译剔除）
- 过程：IPv6 中断约 1 小时后自恢复（本机侧 RA 掉了）→ SSH 探明部署形态
  （/home/upload/uploader，上传管线模式 -dirs 抖音直播/SOOP -web-port 8888，
  config 高光已是 A档+minac1）→ scp 管道被隧道破坏改用 base64-over-ssh 传输
  （MD5 校验一致）→ swap.sh：优雅停旧(PID 22452)→换装→按原参数 nohup 重启
- 结果：新进程 PID 11585，8888 监听，上传队列恢复；备份
  uploader.bak-pre-posegate-20260925；本地 IPv6 中断已自愈（ping 54ms）
- 部署矩阵（最终）：服务器=纯 Go（姿态门剔除，A档+minac1 全功能）；
  Windows 灰度=CGO 构建（姿态门激活中，温和门槛）

## 24b. 服务器部署修正：cwd 坑（2026-09-25 12:40 · 本会话）

- 症状：换装后「主播数据没来」——新进程 cwd=/root（ssh 默认落点），config 相对路径
  （运行时数据目录等）全部偏移
- 修复：按正确 cwd=/home/upload 重启（PID 15481），运行时数据目录 /home/upload/data 正确解析，
  监控全部恢复（小妤/发财mm/肥安娜等），4 路 ffmpeg 录制正常
- 8888 绑定竞争为换装瞬间旧进程未完全释放所致，新进程已自行持有
- swap.sh 已补 cd /home/upload（换装脚本永久修正）
- 教训：**服务器进程重启必须显式 cd 到 /home/upload**——nohup 继承 ssh 会话 cwd

## 22b. 定标参数全网部署（2026-09-25 12:45 · 本会话）

- config highlight_pose_gate 更新为 §22 定标值：vis_min 0.6 / face_max 0.14 /
  det_min 0.2 / ext 0.25-1.5（宽带，实际不约束）/ keep 0.4 / fps 5
- 全链路同步：internal/pose（GatePass/GatePassWith 去 ext 带、FilterSegments v3
  det-aware+平滑）、highlight.PoseGateParams.DetMin、config 类型+clamp、app 映射
- 灰度 uploader 以 v3 构建重启（8080 HTTP 200），待首个切片完成验证
- 双构建全绿（纯 Go + CGO）

## 25. 时序动力学/节拍耦合 5fps 复验——证伪（2026-09-26 06:40 · 本会话）

> HANDOFF 特征迭代项：「时序动力学（四肢位移周期）、节拍耦合（5fps 帧已备）」。
> §24 已证伪 1fps 节律族（奈奎斯特 0.5Hz 不够），本次升级 5fps 关键点轨迹复验。

- **数据**：10 片混合片段（6 主播，宇智波晗/小欣耶耶/豆糕想跳高/D.an/今开心/闲闲饭），
  每片抽预标 dance 窗与 closeup/none 窗各 32s（5fps，480p），YOLOv8-pose 四肢轨迹
  （新命令 `hleval traj-scan`，cmd/hleval/traj_scan.go）。20 窗，检出率 ~99%。
- **特征**（_traj/analyze_traj.js）：四肢归一化速度谱 [1.5,2.5]Hz 能量占比（fBand）、
  自相关峰值（lag 0.4~0.8s，ac）、平均肢速（mean）、主导频率（domFreq）。
- **结果（dance 10 / non 10）**：

  | 特征 | AUC | 判读 |
  | --- | --- | --- |
  | fBand 频带能量比 | **0.210** | **反向**——非舞窗反而更高；配对方向仅 1/10 |
  | ac 自相关峰 | 0.570 | 近随机 |
  | mean 平均肢速 | 0.720 | 有信号，但与既有运动量特征（v_YAVG 族）冗余 |
  | domFreq 主导频率 | 0.710 | dance 窗聚集 2.26~2.42Hz（6/10 vs 1/10），样本过小不足为凭 |

- **解读**：fBand 是「占比」特征——舞窗肢体在全频段大幅运动（分母大），特写/聊天窗的
  少量手势动作集中在窄带（分子不小、分母小），占比反向。绝对频带能量 ≈ mean×fBand，
  被 mean（0.72）主导，仍属运动量信息而非节拍信息。
- **结论**：**节拍耦合/时序动力学特征族在 5fps 下仍不成立**（与 §24 1fps 证伪一致），
  且方向反直觉；主线门控维持 det/vis/face 三特征，节拍方向关闭，除非引入音频 onset
  或更大样本推翻本次结论。
- **边界**：①参考标签为模型预标（非用户金标），对 det/vis/face 有同源性，
  但节拍特征是独立测量通道，方向性结论不受循环论证影响；②样本 20 窗偏小；
  ③domFreq 2.3Hz 聚类现象留档备查。
- 工具链沉淀：`hleval traj-scan`（5fps 轨迹提取）+ `_traj/{pick_windows,analyze_traj}.js`
  可复用，后续任何时序特征假设可直接跑。
