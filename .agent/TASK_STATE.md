# TASK_STATE — 高光姿态门项目（2026-09-26 06:55）

状态：**✅ 正式收官**（用户拍板；验收 vet/build/test 21 包全绿；收官定稿见 GRAY_OBSERVE_20260925.md §八）

## 收官摘要
- 定稿参数：姿态门 det 0.3 / vis 0.6 / face 0.12 / keep 0.3 + A 档 th1.2/ml8/gf12 + minac1 0.15（8080 常驻）
- 训练池 418 片全自动（无人工标注）；自动标签一致率 79.3%
- 开封 #4：运动模型过线（段级 P 0.900/R 0.818）；姿态门 #5 待冻结 v2（freeze_v2.json 已备，源片禁删）
- 特征族全部定论：节拍耦合 5fps 复验证伪（spatial-de.md §25）
- 服务器部署守护运行中（DNS 恢复即自动部署，autodeploy_20260926.log）；夜训守护到 07:00 自然结束

## 服务器部署完成（2026-09-26 06:5x，用户令「开始部署」）
- 本机 IPv6 恢复（2409:8924:...）→ 手动执行部署：base64-over-ssh 传输 19.6MB →
  MD5 双端一致（9680e5cc）→ swap.sh 原子换装重启（PID 6090，cwd=/home/upload ✅）
- **版本确认**：/api/v1/bilibili/status 返回 401（路由存在=新二进制；旧版 404）
- **firewalld 曾放行 8888，按用户要求已撤销**（--remove-port + reload，列表与改动前一致）——
  用户只要内网访问控制台；外网 8888 已恢复不可达
- 自动部署守护已停（任务由手动完成）；服务器参数原样（-dirs 抖音/SOOP -workers 5 -web-port 8888）

## 姿态门上服务器（2026-09-26 07:1x，用户令「我也要把姿态门加进去」）
- **用户推翻「服务器纯 Go」旧要求**——训练成果（姿态门）必须上生产。改用 CGO Linux 构建
- 服务器 CentOS7 glibc 2.17 跑不动 ORT 1.30（需 2.25+）→ **sysroot 方案**（manylinux 同款）：
  Ubuntu20.04 glibc 2.31 + libstdc++10 提取到 /home/upload/_pose_runtime/sysroot/，
  经新 loader 启动（uploader_run.sh 包装），系统零改动
- 服务器现场构建：Go 1.27 tarball（golang.google.cn 自取）+ gcc 4.8.5 + goproxy.cn 模块，
  源码 tar 从本地工作树传输 → uploader_pose.new 含 POSE-GATE 诊断 ✅
- 冒烟：hleval_pose pose-scan 10 帧全链路（ORT 加载→模型推理→特征）✅
- 配置：highlight_pose_gate enable + 定稿参数（det0.3/vis0.6/face0.12/keep0.3/fps5）+
  onnx_dll/onnx_model 指向 /home/upload/_pose_runtime/；旧纯 Go 版已备份 uploader_purego.bak.*
- 换装：swap.sh PID 27464，8888 HTTP 200，进程存活，启动零异常（门为懒加载，首段分析时初始化）
- 回退：把 .restart_args.txt 首行改回 /home/upload/uploader + 换回备份二进制即可
- 8888 防火墙：曾放行又按用户要求撤销（内网访问策略不变）

## 训练页 + 分支（2026-09-26 07:5x，用户三连指令）
1. **分支就位**：`pose-gate` 分支两笔提交（e7172fe 姿态门+训练管线基线 42 文件；8d9a54c 训练页签）。
   **main 保持 0ffffce 无姿态门**（用户要求：main=无姿态门版，pose-gate 分支=带门版）。
   **⚠️ 推送红线：pose-gate 仅限本地，禁止 merge/push 到 main，origin/main 不动（用户 09-26 明确要求）**
2. **runbook 文档**：docs/POSE_GATE_SERVER_RUNBOOK.md（sysroot 原理/服务器路径/更新流程/回退/踩坑）
3. **控制台「姿态训练」页签上线**（本地 8080 已重启交付）：
   - 后端 api/http/pose_training.go 三端点（summary/clips/thumb），mtime 缓存；sweep 结果 JSON 化
   - 前端深色页签：进度卡 6 项 / 门参数只读 / 重定标 TOP10 / 片池分页表+缩略图 lightbox / 训练活动日志
   - internal/auth 白名单加 thumb（<img> 原生请求带不了 Bearer，走会话 Cookie）
   - 浏览器级验收通过：进度卡数字正确、TOP10 表格、片池 573 片缩略图 20/20 加载
4. 夜训守护 07:30 到点自然结束；片池已涨至 573 片（夜间自动入池），D 盘 27.8GB

## 训练页部署服务器（07:3x）
- 按 runbook 五步：源码 tar 传输 → 服务器 CGO 构建（BUILD_OK，含 pose_training 路由 ×11）
  → swap.sh 换装（PID 31552，sysroot loader）→ 8888 200 / 训练页路由 401=存在 ✅
- 注：服务器上训练页数据为空（训练数据只在本机 D:/upload/_diag/train）——属预期，页面显示空态；
  训练进度页只在 8080 控制台有数据

## 移交事项（按优先级）
1. ✅ 服务器部署完成（2026-09-26 06:5x，外网 8888 HTTP 200，firewalld 已放行）
2. 磁盘长期治理：enableUpload（上传后删源）或定期删非冻结源片——待用户定
3. 冻结 #5：姿态门 out-of-sample 终验（资产已备齐）
4. 代码改动未提交 git（本轮：traj-scan、门诊断日志、review-ingest 增量、det 0.3），需要时打一个收官 commit

## 任务来源
docs/HANDOFF-20260925.md（交接文档）。技术细节: docs/highlight-spatial-de.md §16-25。

## 任务来源
docs/HANDOFF-20260925.md（交接文档）。技术细节: docs/highlight-spatial-de.md §16-24。

## 今日（下午场）完成
1. **review-ingest 增量落盘改造**（cmd/hleval/review_ingest.go）：
   - 每处理完一片立即写 clips_config.json（原子落盘 tmp+rename），复核页边跑边见新片
   - 新增续跑快路径：pose 特征已存的片直接复用预标，不重抽帧不重推理
   - 后台续跑中（188 片，~75 完成，config 48→122）
2. **灰度观察首测（同口径非舞率）**：对严格门槛后产出的 13 个高光 1fps 抽帧+pose-scan：
   - 7末（金标舞区 835 窗）：非舞率 **7.7%**；年年：**8.2%**（对照门前预测 ~65%）
   - 颍颍呐：**87.1% 全是人脸特写聊天**却被门全放行 → 触发排查
3. **两个根因定位（均有实证）**：
   - 运行中的 uploader.exe 是 11:42 旧构建，**不含 §22 定标的 det 检出率段**，"无姿态秒不误杀"对全未检出特写 = 100% 放行
   - `gate_cgo.go` detmin 分母 bug：`detCnt/dur`（5fps 帧数÷秒数）把检出率放大 5 倍，detmin 0.2 实际只挡 4%
4. **修复+重建+重启**（17:29）：detmin 分母改 `len(ffs)`（帧率无关=检出秒占比语义），重建 uploader.exe（含 §22 全部代码），8080 已恢复，live 参数验证：vis 0.6/face 0.14/det 0.2/keep 0.4 + A 档(ml8/gap12)

## 修改文件
- cmd/hleval/review_ingest.go（增量落盘+续跑快路径）
- internal/pose/gate_cgo.go（detmin 分母修复+注释）
- 重建: _diag/train/hleval.exe（16:35）、uploader.exe（17:29，CGO 灰度版）

## 验证
- go vet ./internal/pose ./cmd/hleval ✅ go test 两包 ✅
- 8080 200 ✅ live config 严格值 ✅ 进程 PID 42004（Start-Process 隐藏窗口）
- 复核页 8131 运行中（node server.mjs，后台 exec_cc3fb276）

## 下一步
1. ✅ 17:29 重启后首批验证通过：莓了兔砍2留1全对（留段=结尾舞段）；苏子液留段84%舞窗
   （砍50%合理）；豆糕想跳高实为舞区、全留正确。修复后基线：21源50段砍14（28%），零误杀证据
2. ✅ 20:45 review-ingest 已续跑重启（新会话发现进程随上次会话回收被带走）：待入池 193 片
   （config 已 232 片、pose 特征 232 片）。复核页 8131 已同步重启（HTTP 200 验证过），
   用户浏览器已打开 http://127.0.0.1:8131/ 边跑边复核
3. 灰度报告: _diag/train/GRAY_OBSERVE_20260925.md 已更新至 §四/§五/§六；
   数据积累 1-2 天 → 用户拍板「正式收官」或「继续收紧」
4. ⚠️ 磁盘：21:00 删金标源片回收 38.3GB，余 46.5GB；按当前 26 路录制写入量 1-2 小时内会再度吃紧，
   长期治理（enableUpload / 减路数）待用户拍板
5. 舞区主播后续若砍>0 继续抽帧抽查；闲闲饭/李知恩产出比观察是否回落

## 磁盘紧急治理（20:45-21:10，已处置）
- D 盘 14.8→9.9GB（26 路黄金档录制 ~0.4-1GB/min 写入，约 10 分钟见底）；上轮 _trash_20260925 已被清掉，无现成可删池
- 源片普查：金标 240 片 38GB（全部已复核）/ 已入池非金标 1 片 / 未入池积压 248 片 31.5GB
- **用户拍板：删金标源片**。已删 242 片 .ts 回收 38.3GB（排除高光产物与 10 分钟内新写文件），零失败
- 审计日志: _diag/train/_deleted_gold_sources_20260925.log。帧/姿态特征/复核标签完好，训练与复核页不受影响
- 遗留：46.5GB 按当前写入量仅撑 1-2 小时；长期方案（开 enableUpload 上传后删源 / 减录制路数）待用户决定
- 注意：今后音频特征/换模型重提特征需要源片，金标源片已不可重提（代价已知悉）

## 晚间场（21:00-22:00，误杀排查 + 门诊断加固）
1. **舞区误杀抽查坐实**（GRAY_OBSERVE §4.1）：6 片全砍源中 3 片为真舞
   （知夏。两片舞窗 98%/93%、豆糕想跳高一片 64%）；门日志证实是姿态门砍的（3段→0段）
2. **离线复现矛盾**：同文件用门原始命令（-ss/fps5/480p）+ 门口径数学 → det率 0.990 / keep率 0.979
   应全留；seek 点帧目检正常 → **现场砍留=运行时环境故障**（26 路录制+训练任务 CPU 打满时段），非公式/阈值错误
3. **门诊断日志已上线**：gate_cgo.go 两处拒段分支补 det率/keep率/vis/face 数值
   （[POSE-GATE] 前缀），go vet/test ✅，重建 uploader.exe（备份 .bak-pre-gatelog-20260925）
   并 22:0x 重启 8080（HTTP 200，录制保持暂停）
4. **⚠️ 发现 live 门参数比 §22 定标更严**：config face_max=0.12 / keep_ratio=0.3
   （§22 定标 0.14/0.4，17:29 验证时还是 0.14/0.4）——17:29 后被改过（用户收紧或他动），
   待用户确认是否有意；离线复现证明该差异不改变误杀结论（余量巨大）
5. **录制 21:2x 已按用户指令全停**：pause_all（builtin_urls.txt 全注释，可 resume_all 恢复）
   + 终止 10 个未随暂停终止的 ffmpeg 写入进程；磁盘稳定 ~34.8GB
6. review-ingest 至 22:0x 约 42+/193 片，持续后台推进

## 自动标签转型 + 夜训（22:1x-22:4x）
1. **用户拍板：不再人工标注，全自动**。用 268 金标片 / 14286 窗做了阈值网格重定标
   （autogold_sweep.js，输出 _diag/train/autogold_sweep.log）：
   - 最优 **det 0.3 / vis 0.70 / face 0.12 → P 0.697 / R 0.916 / F1 0.792**（§22 旧点 F1 0.767）
   - **live 现值（face 0.12/keep 0.3）被新数据验证是对的**（F1 0.784，P +4.4pt 换 R -3.9pt 划算）
   - 自动标签 dance/非舞二分类一致率 **79.3%**——closeup/chat/gift 等非舞类全部当「非舞」吸收
   - detmin 0.2→0.3 是本轮最大收益（5fps 下 det 率分布右移所致）
2. **夜训守护已启动**（night_train.sh，日志 nightly_train_20260925.log）：等当前 review-ingest
   结束后，每 30 分钟循环「新片入池 → 重定标」，到明早 07:00 自动停。5 点每日 cron 按用户要求删除
3. 服务器连通性：DNS 解析失败（xyx.homes 无 AAAA 返回）——本机 IPv6 掉了（HANDOFF 踩坑已知），
   等 RA 自愈或重启路由器后再部署
4. review-ingest 至 22:3x 约 72+/193 片

## 重定标成果上线（23:5x）
- **det_min 0.2→0.3 已上 8080 生产**（268 片重定标最优：P 69.7%/R 91.6%/F1 0.792），
  重启后 HTTP 200，live 参数：det 0.3 / vis 0.6 / face 0.12 / keep 0.3 / fps 5
- 服务器部署预置工作待做：linux 构建可现做（~1 分钟）；卡本机 IPv6（xyx.homes 无 AAAA 解析）

## #3/#4 用户批准执行（2026-09-26 06:00-06:45）
1. **#4 开封 #4 完成**（GOLIVE_READINESS 已登记）：
   - 4a 运动模型冻结验收：段级 P 0.900/R 0.818/F1 0.857 过线（P≥0.8 且 R≥0.6），SOOP 0.742 与 #2 一致无回归
   - 4b 姿态门：冻结 3 片源视频已随清理灭失（姿态特征晚于清理建立）→ **无法在原冻结集运行**，
     已登记替代证据（灰度 91 源 + 268 金标重定标）；**冻结集 v2 已建立**（freeze_v2.json，
     小妤/今开心/D.an 三片、源片永久保留禁删）→ 补开封 #5 用
2. **#3 时序动力学/节拍耦合 → 证伪**（spatial-de.md §25）：20 窗 5fps 四肢轨迹
   （新命令 hleval traj-scan），fBand AUC 0.210 反向、ac 0.570 随机、
   mean 0.720（与运动量特征冗余）、domFreq 0.710（2.3Hz 聚类留档）。主线维持 det/vis/face
3. **review-ingest 全部完成：新增 186 片，config 总 418 片**（pose 特征同步 418）
4. 服务器部署：Linux 纯 Go 二进制已构建（deploy_server/uploader_linux，md5 9680e5cc），
   autodeploy_watch.sh 守护中（DNS 每 10 分钟一探，恢复即自动传输+MD5 校验+swap 重启+8888 健康检查）
5. 夜训守护继续到 07:00（每 30 分钟重定标轮询）

## 环境备注
- 宇智波晗/莓了兔等 0 分析记录：非门因素（外部引擎或未入队），与本门无关，报告已注明
- 本会话启动的后台任务（2026-09-25 晚，新会话）：review-ingest 续跑（exec_06730c33，日志 _diag/train/review_ingest_resume2.log）、复核页（exec_f5a89a1c，日志 _diag/pose_label/server_8131.log）。旧会话 exec_2fb61d81/exec_cc3fb276 已随会话回收失效
- 旧 UI 任务状态已归档 .agent/backup/TASK_STATE_ui_20260919.md

---

# TASK_STATE — B 站自动投稿高光片段（2026-09-25，另一会话）

状态：**已完成 100%**（代码 + 测试 + 浏览器验收；未进 8080 生产二进制，见遗留）

## 交付内容
1. `internal/bilibili`（新包，自研投稿客户端）：nav 登录校验、preupload 预上传、upos 分片上传（init/chunk/complete，X-Upos-Auth，固定缓冲分片不上内存）、cover/up 封面、add/v3 提交；APIError 保留 B 站业务码；标题按 rune 截断 80；httptest 全流程单测（分片字节级校验）。
2. `internal/config`：BilibiliSettings 扩展（copyright/source/dynamic/no_reprint/min_interval_minutes/daily_limit/max_retry/cover_enable 指针默认开）+ ApplyDefaults（tid=129 舞蹈、tag=直播,高光、间隔 10 分钟、日限 20、重试 3）。
3. `internal/app/publish.go`（新）：落盘队列 dataDir/bili_publish.json（原子写、500 条上限、崩溃恢复 uploading→pending）；publishLoop worker（30s tick；闸门：退避时刻 → 最小间隔 → 每日上限）；ffmpeg 截帧封面（时长 30% 处）；模板渲染 8 占位符；失败退避 n×10min，耗尽转 failed；analyzeClip 成功后入队（ID=产物绝对路径去重）。
4. `internal/app/highlight.go`：highlightEntry 扩展 start_sec/end_sec/score 落盘（旧文件兼容）。
5. `internal/app/run.go`：go publishLoop()；ApplyDataDir → resetPublishState。
6. `api/http`：GET /api/v1/bilibili/status（登录态 5 分钟缓存）、GET/POST /api/v1/bilibili/queue（列表 / retry|delete）+ 测试。
7. `web/index.html`：新「B站投稿」页签（统计卡×5、参数表单、策略开关、投稿队列表 + 空态、10s 驻留轮询、删除确认）。
8. README 新增「B站自动投稿」章节。

## 验证
- go vet ./... ✅ go test ./... ✅ go build ./... ✅（bilibili 12 项、app publish 9 项、api 2 项单测）
- gofmt：新文件已格式化；既有 CRLF 文件未动
- -race 因本机无 gcc 不可用（环境限制）；锁逻辑人工复核，修复 publishProcess 无锁读统计/写 CoverURL 的竞争
- 冒烟实例（临时目录 :18999）：默认配置正确、两端点 200、登录/保存链路 UI 实测通过
- 浏览器页面级验收（深色截图）：统计卡、表单默认值、封面开关反向绑定、队列空态、登录态轮询 chip 全部通过

## 遗留 / 上线步骤
1. ✅ 19:12 已部署 8080 生产实例（PID 71536）：CGO 工具链 `CC=D:/upload/_vendor/mingw64/mingw64/bin/gcc.exe TMP=D:/upload/_vendor/tmp`（HANDOFF 记录），旧版备份 uploader.exe.bak-pre-bili-20260925；生产 config 历史预留 tid=174/tag=直播回放,游戏 按原值保留；CGO 姿态门确认编入
2. 用户填 SESSDATA/bili_jct 后投稿一次做真实链路验证（协议按 biliup-rs 逆向实现，未对真实 B 站打过）
3. 后续增强：扫码登录、转载类型 source 前端预校验
