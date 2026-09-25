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
- 夜训守护 `night_train.sh`：循环「新片入池→重定标」，可随时手动重跑（幂等）
- **定稿门参数**：det 0.3 / vis 0.6 / face 0.12 / keep 0.3 / fps 5（268 金标重定标最优）

## 5. 踩坑备忘

- scp 管道在家宽 IPv6 隧道上会坏 → 一律 base64-over-ssh + MD5 校验
- Windows 打 tar 传 Linux 会丢执行位 → 解包后 `chmod +x`（loader 就吃过亏）
- ar/tar 解 deb：git-bash 无 ar，用 node 按 deb 格式切 data.tar.xz 再解
- 服务器 config.json 里含 cookies 等敏感信息，不要回传/外发
- 复核页（8131）只是历史金标查看用，标注已全自动，不再需要人工操作
