# 姿态门服务器部署 Runbook（2026-09-26 定稿）

> 目的：这份文档把「姿态门如何上到服务器」的全部细节固化下来，下次照着做即可，
> 不需要重新探索。技术背景见 docs/highlight-spatial-de.md，状态见 .agent/TASK_STATE.md。

## 0. 分支与两条部署线

| 分支 | 内容 | 部署到 |
| --- | --- | --- |
| `main` | **无姿态门**（纯 Go 构建，internal/pose 桩自动关门） | （历史基线保留） |
| `pose-gate` | **含姿态门** + 全自动训练管线 + hleval 工具 | 本机 8080 + 服务器 8888 |

> ⚠️ **推送红线（用户 2026-09-26 明确要求）**：`pose-gate` 分支**仅限本地**，
> **禁止 merge / rebase / push 到 main**，远程 origin/main 保持不动。
> 部署只从本地 pose-gate 分支构建，不经过 GitHub。

姿态门代码用构建标签隔离：`internal/pose/pose_cgo.go`（cgo 构建才编译）
vs `pose_nocgo.go`（纯 Go 桩）。所以**同一份 pose-gate 分支源码**：
`CGO_ENABLED=1` 构建 = 带门；`CGO_ENABLED=0` = 不带门。门代码进不进二进制由构建开关决定。

## 1. 服务器现状（xyx.homes:8888，内网访问）

- CentOS 7 x86_64，**glibc 2.17**，gcc 4.8.5，ffmpeg 在 /usr/local/bin/ffmpeg
- 运行：`/home/upload/uploader`（CGO 姿态门版，PID 见 pgrep），经 **sysroot 新 glibc loader** 启动
- 防火墙：8888 不放行（用户要求内网访问）；SSH 22 通

### 关键路径（服务器）
```
/home/upload/uploader            当前生产二进制（CGO 版）
/home/upload/uploader_run.sh     启动包装器：exec sysroot loader --library-path ... uploader "$@"
/home/upload/.restart_args.txt   重启参数（首行=包装器路径，其余=原参数）
/home/upload/uploader_purego.bak.*  纯 Go 旧版备份（回退用）
/home/upload/_pose_runtime/      libonnxruntime.so(1.30) + yolov8n-pose.onnx + sysroot/
/home/upload/_pose_runtime/sysroot/  Ubuntu20.04 提取的 glibc2.31+libstdc++10（运行时前缀）
/home/upload/_toolchain/         go1.27 tarball 解包 + gomod/gocache
/home/upload/_build/             源码树（本地 pose-gate 分支工作树打包上传）
/home/upload/uploader_console.log    控制台日志（看 🧍 姿态门 行）
```

### 为什么需要 sysroot（防下次踩坑）
ORT 1.30 的 libonnxruntime.so 需要 GLIBC_2.25~2.28，CentOS 7 只有 2.17，
dlopen 直接失败。manylinux 同款解法：把 Ubuntu 20.04 的 glibc 2.31 + libstdc++10
提取到 sysroot 目录，进程用 `ld-linux-x86-64.so.2 --library-path` 启动。
**系统零改动**。冒烟验证过 hleval pose-scan 全链路（ORT 加载→模型推理→特征输出）。

## 2. 重新部署/更新流程（pose-gate 分支有新代码时）

```bash
# ① 本地打源码包（工作树直接打包，含未提交改动也行）
cd /d/upload && tar czf _diag/train/_server_build_src.tgz go.mod go.sum cmd internal api web
# ② 传输 + 解包（base64-over-ssh，scp 管道在家宽隧道上会坏）
cat _diag/train/_server_build_src.tgz | ssh xyx.homes "cat > /home/upload/_build_src.tgz && tar xzf _build_src.tgz -C /home/upload/_build"
# ③ 服务器构建（CGO 必须 =1）
ssh xyx.homes 'cd /home/upload/_build && PATH=/home/upload/_toolchain/go/bin:$PATH \
  GOROOT=/home/upload/_toolchain/go GOPROXY=https://goproxy.cn,direct \
  GOMODCACHE=/home/upload/_toolchain/gomod GOCACHE=/home/upload/_toolchain/gocache \
  CGO_ENABLED=1 go build -o /home/upload/uploader.new ./cmd/uploader'
# ④ 换装重启（swap.sh：杀旧→mv uploader.new→按 .restart_args.txt 重启→健康检查）
ssh xyx.homes "bash -s" < /d/upload/deploy_server/swap.sh
# ⑤ 验证
ssh xyx.homes 'curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8888/'
```

验证姿态门在岗：首次高光分析时日志出现 `🧍 ... 姿态门: X 段 → Y 段` 或
`[POSE-GATE] ... det关拒/keep关拒`；出现 `姿态门异常（放行全部段）` = 门初始化失败（查 sysroot/ORT）。

## 3. 回退（一键回到纯 Go）

```bash
ssh xyx.homes 'sed -i "1s#.*#/home/upload/uploader#" /home/upload/.restart_args.txt'
# 再把任一 uploader_purego.bak.* 拷回 /home/upload/uploader.new，跑 swap.sh
```
纯 Go 版姿态门自动关闭（桩），其余功能不变。

## 4. 本机训练管线（8080 同机）

```
D:/upload/_diag/train/
├── hleval.exe                      训练/评估工具（review-ingest / pose-scan / traj-scan / metrics / train）
├── _pose_pilot/
│   ├── clips_config.json           入池片池（418+ 片，自动预标 spans）
│   ├── pose_features_go.json       每秒姿态特征（与片池同步）
│   ├── gold_review.json            用户历史金标（268 片/14286 窗，只读保存）
│   ├── freeze_v2.json              冻结集 v2（3 片，**源片永久保留禁删**）
│   └── frames/                     1fps 帧（复核页 8131 的图片源）
├── autogold_sweep.js               阈值网格重定标（输出 autogold_sweep.log / autogold_result.json）
├── nightly_train_20260925.log      夜训守护日志
└── _traj/                          5fps 关键点轨迹工具（§25 节拍证伪用）
```

- **自动标签已替代人工复核**（用户 2026-09-25 拍板）：review-ingest 自动预标，
  dance/非舞一致率 79.3%（268 金标验证）
- **训练方式（2026-09-26 更新）：常驻自动化已按用户要求停用，改为手动触发。**
  手动命令（幂等可续跑；结果写 autogold_result.json 供控制台「姿态训练」页展示）：

  ```bash
  # 新片入池（抽帧+姿态推理+自动预标）
  cd /d/upload/_diag/train && ./hleval.exe review-ingest \
    -downloads D:/upload/downloads -frames D:/upload/_diag/train/_pose_pilot/frames \
    -config D:/upload/_diag/train/_pose_pilot/clips_config.json \
    -pose-out D:/upload/_diag/train/pose_features_go.json \
    -dll D:/upload/onnxruntime.dll -model D:/upload/yolov8n-pose.onnx \
    -ffmpeg "D:/upload/ffmpeg-master-latest-win64-gpl-shared/ffmpeg-master-latest-win64-gpl-shared/bin/ffmpeg.exe"
  node autogold_sweep.js   # 重定标
  ```

  生产门参数不自动改，阈值调整由用户参考页面重定标结果另行拍板
- **定稿门参数**：det 0.3 / vis 0.6 / face 0.12 / keep 0.3 / fps 5（268 金标重定标最优）
- **ext 带已彻底移除**（2026-09-26）：门不读、配置字段删除、评估默认关闭。详见下方「评估口径」

### ⚠️ 评估口径（务必先读，否则数字会看错 7 倍）

`hleval metrics` 是**端到端**口径，`autogold_sweep.js` / `seg_gate_sim` 是**段内条件**口径，
**两者不可直接比较**：

| 口径 | R 的分母 | 同一组门槛的 R |
| --- | --- | --- |
| `hleval metrics` | CSV 里**全部**真舞秒（`features_pilot48.csv` = 7702） | **0.124** |
| `autogold_sweep` / 段内模拟 | **A 档已预测段内**的真舞秒（≈977） | **0.87** |

差 7 倍的根因是**分母**，不是门。`features_pilot48.csv` 的 A 档打分基线召回本身只有 12.7%
（`mw=1 aw=0 th=1.2 ml=8 gap=12`，见 `seg_gate_sim_20260925.log` 首行），
端到端 R 的天花板就是它 —— **不是姿态门砍的**。
比较门的效果看段内条件口径；评估端到端产出看 metrics。

### ext 带（2026-09-26 起默认关闭）

`metrics` 的 `-pose-extlo`/`-pose-exthi` 默认 **0/0 = 关闭**（打印行会显示 `ext 关`），
显式给非零范围才启用。历史默认 0.5/1.0 会让评估口径与线上门（不带 ext）不一致 ——
实测同参数下带 ext 带 TP **73**、不带 TP **956**（召回差 **13 倍**）。
`review-ingest` 的 `-extlo/-exthi` flag 已移除；自动化 prompt 若还带这两个参数会直接报错退出。

### 预标门槛 ≠ 生产门槛（刻意不同）

| 用途 | 常量 / 位置 | 值 | 取向 |
| --- | --- | --- | --- |
| 预标（生成候选金标） | `pose.PrelabelDetMin/VisMin/FaceMax` | 0.2 / 0.6 / 0.14 | 宽松，尽量覆盖 |
| 生产门（砍段） | `config.json` highlight_pose_gate | 0.3 / 0.6 / 0.12 | 严格，尽量精确 |

两者是**两套数**，不要互相对齐。判定函数已统一为 `pose.AggregateWindow` ——
`review-ingest` 落盘 spans 与控制台「姿态训练」页的窗口回放走同一个函数，
不会再出现「页面标签 ≠ 落盘 spans」。

## 5. 踩坑备忘

- scp 管道在家宽 IPv6 隧道上会坏 → 一律 base64-over-ssh + MD5 校验
- Windows 打 tar 传 Linux 会丢执行位 → 解包后 `chmod +x`（loader 就吃过亏）
- ar/tar 解 deb：git-bash 无 ar，用 node 按 deb 格式切 data.tar.xz 再解
- 服务器 config.json 里含 cookies 等敏感信息，不要回传/外发
- 复核页（8131）只是历史金标查看用，标注已全自动，不再需要人工操作
