# 姿态门使用指南（highlight_pose_gate）

> 面向运维与使用者。读完这篇你能：开/关姿态门、调阈值、验证它在跑、换门头模型、出问题时回退。
> 算法推导见 `docs/highlight-spatial-de.md`，上线验收计划见 `docs/pose-gate-rollout-plan.md`，
> 服务器部署细节见 `docs/POSE_GATE_SERVER_RUNBOOK.md`。

## 1. 它是干什么的

高光切片的候选段由「画面运动量」打分选出，但纯运动量分不清**跳舞**和**近景说话 / 礼物特效 /
机位切换**——后者帧差同样大。姿态门在候选段进入切片产出之前加一道语义过滤：

```
高光候选段（运动量选出）
   │
   ▼
姿态门三阈值（det_min / vis_min / face_max）──拒绝→ 丢弃（日志 [POSE-GATE]）
   │ 通过
   ▼
学习型门头段级投票（GBDT，head_filter_enable）──frac < head_frac → 整段丢弃
   │ 通过
   ▼
产出高光切片 <主播>/<日期>/高光/<原片名>_highlight.mp4
```

关键性质：**门和门头都只会删段、不会加段**。关掉它们，行为回到纯运动量高光，不会丢任何现有产出。

## 2. 前置条件（两条硬性要求）

1. **必须用 CGO 构建**。纯 Go 构建（`CGO_ENABLED=0`）下姿态门是空桩：不报错、不抽帧、**静默不生效**。
   本机 Windows 用 `CGO_ENABLED=1 go build`；Linux 服务器构建见 Runbook（sysroot 方案）。
2. **运行时依赖两个文件**，默认放在 exe 同目录：
   - `onnxruntime.dll`（运行库，1.30+）
   - `yolov8n-pose.onnx`（人体姿态模型，输入 640）
   也可以在配置里用 `onnx_dll` / `onnx_model` 指定绝对路径。

## 3. 怎么开

配置在 `config.json` 的 `builtin.highlight_pose_gate`。最小启用配置：

```json
{
  "builtin": {
    "highlight_enable": true,
    "highlight_pose_gate": {
      "enable": true,
      "det_min": 0.30,
      "vis_min": 0.70,
      "face_max": 0.12
    }
  }
}
```

保存方式二选一：控制台设置页保存（走 `PUT /api/v1/config`，即时生效），或直接改
`config.json` 后**重启进程**（配置文件不热重载）。

## 4. 全部字段

| 字段 | 默认 | 含义 |
| --- | --- | --- |
| `enable` | false | 姿态门总开关 |
| `det_min` | 0.30 | 每帧人体检出率下限：段内检出人体的帧占比低于它 → 整段拒绝（纯风景/游戏画面挡在这里） |
| `vis_min` | 0.70 | 关键点可见性均值下限：画面里人太糊/太小 → 拒绝 |
| `face_max` | 0.12 | 脸部关键点占比上限：**太高说明是近景特写**（贴脸说话），不是跳舞 → 拒绝 |
| `keep_ratio` | 0.30 | 段内通过三阈值的帧占比下限（默认值由代码提供，一般不动） |
| `fps` | 5 | 段内抽帧率（<1 或 >30 回落 5）。5fps 覆盖 1.7~2.3Hz 舞曲节拍，一般不动 |
| `onnx_dll` | exe 同目录 `onnxruntime.dll` | 运行库路径 |
| `onnx_model` | exe 同目录 `yolov8n-pose.onnx` | 姿态模型路径 |
| `head_filter_enable` | false | 学习型门头（GBDT 段级投票）开关。**默认关**，先跑通三阈值再开 |
| `head_model` | `_diag/train/gate_head/gate_head_v2_trees.json`（相对部署工作目录） | 门头模型 JSON 路径（内置默认是 v2 灰度模型；生产配置当前指向 v5） |
| `head_frac` | 0.5 | 段内头判舞窗占比 ≥ 它才保留整段；配置层限制在 0.3~0.9 |

生产推荐组合：三阈值 `det 0.30 / vis 0.70 / face 0.12` + 门头 `head_frac 0.5`。
段级投票是**整段全有或全无**（段内判舞窗过半才保留），这不是缺陷而是段级最优策略，不要改成段内二次切分。

## 5. 怎么验证它在跑

1. **看日志**。生效后日志里会出现 `[POSE-GATE]` 前缀的行，拒绝原因一目了然：

   ```
   [POSE-GATE] <片名> 段[120-180s] det关拒: 帧30 检出3 det率=0.100 (<0.30)
   [POSE-GATE] <片名> 段[60-120s]  keep关拒: 检出帧30 过12 keep率=0.400 (<0.30) 平滑后均值 vis=0.83 face=0.05
   [POSE-GATE] <片名> 段[240-300s] 头投票拒: 段内8窗 frac_dance=0.250 (<0.50)
   ```

   三条分别对应三阈值、段级通过率、门头投票三道闸。**如果一条 `[POSE-GATE]` 都没有**：
   要么没开启（`enable=false`）、要么是 CGO=0 空桩构建、要么 dll/onnx 加载失败（会有 `⚠️ 头加载失败（放行）`）。

2. **看产出**。判断高光是否真的有效，只看 `data/highlight_status.json` 里 `output` 非空的条目数，
   不要看候选段数量。

## 6. 常见操作

| 想做什么 | 怎么做 |
| --- | --- |
| 临时关掉门头（一键回退） | `head_filter_enable=false`，立即生效，段产出恢复到三阈值口径 |
| 彻底关掉姿态门 | `highlight_pose_gate.enable=false` 或删掉整个 `highlight_pose_gate` 对象 |
| 换门头模型（如 v5 → v6） | 改 `head_model` 指向新 JSON，**重启进程**（模型按路径懒加载缓存，不重启不生效） |
| 回退门头版本 | 把 `head_model` 指回旧文件 + 重启；或删掉 `head_model` 字段回落内置默认（v2：`gate_head_v2_trees.json`） |
| 查当前生效值 | `GET /api/v1/config`，看 `builtin.highlight_pose_gate` |

## 7. 服务器部署注意

生产服务器是 CGO 姿态门版，**绝不能把本机交叉编译的 `CGO_ENABLED=0` 二进制传上去**——
姿态门会静默退化成纯 Go 桩，日志里不会有任何报错，只有高光产出消失这一个症状。
正确流程（源码包 + 服务器上 CGO=1 构建）完整写在 `docs/POSE_GATE_SERVER_RUNBOOK.md`，
部署前先跑 `CGO_ENABLED=0 GOOS=linux go build ./...` 排除 Linux 专属文件冲突。

## 8. 训练侧怎么配合

门头模型由离线工具训练产出，闭环在 `cmd/hleval`：

- 采集/预标：`hleval review-ingest`（控制台「姿态训练」页可视化）
- 训练与重定标：`hleval autogold-sweep`（守护脚本 `continuous_training.ps1` 事件驱动）
- **验收必须用冻结集 v3**（`_diag/train/freeze_v3_gt.json`），红线：冻结集标注永不拷入金标
- in-sample 的提升不算数，换装前必须在冻结 v3 上 OOS 复测
