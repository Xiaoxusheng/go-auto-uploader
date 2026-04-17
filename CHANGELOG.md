# 📝 Go Auto Uploader 迭代更新日志 (Changelog)

### [v2.4.1] - 2026-04-17 (Telegram Interactive UI & OTA & Performance)
#### 🌟 核心特性 (New Features)
* **可视化多级交互面板 (Interactive UI Panel)**：彻底重构 Telegram 交互逻辑。引入九宫格主菜单与动态二级菜单，通过 `Inline Keyboard` 实现全按键操控，支持“一键挂起”、“极速恢复”、“画面截取”等功能，告别命令行输入。
* **海量主播分页引擎 (Pagination Engine)**：针对 200+ 以上的大规模主播列表，开发了智能内存分页系统。支持自动排序、翻页导航（上一页/下一页）及状态就地刷新，突破了 Telegram 单条消息 100 按钮的物理限制。
* **OTA CGroup 逃逸更新 (CGroup-Escape Hot Update)**：引入企业级 `systemd-run` 逃逸技术。在执行二进制热替换后，通过独立的临时系统单元触发重启，彻底解决了重启脚本被原 Systemd 进程组连坐杀死的 Bug，并支持重启成功后的 ChatID 定向回执。
* **零指令智能链接嗅探 (Zero-Command Sniffer)**：为机器人注入了链接感知识别引擎。用户只需在聊天框直接发送直播间链接，系统将自动拦截并触发解析挂载流程，实现无缝添加任务。

#### 🚀 性能与架构极致优化 (Performance & Architecture)
* **全链路防假死网络动力引擎 (Anti-Deadlock Network Engine)**：针对旁路由/透明代理环境，注入了自定义的高心跳 `http.Client`。开启 15s 级 TCP Keep-Alive 并缩短轮询超时，主动预防长连接被路由器静默掐断导致的“僵尸假死”现象。
* **MD5 指纹映射系统 (Key Fingerprint Mapping)**：利用 MD5 算法为主播生成 8 位短指纹替代原始超长 URL 塞入按键数据。完美规避了 Telegram 64 字节 Callback Data 限制及 UTF-8 截断导致的 UI 报错。
* **内存级日志秒发 (Memory Log Export)**：直接从系统高性能环形日志队列中实时抽取数据并生成 `.txt` 附件发送，实现 0 磁盘读写损耗的实时日志导出。

---

### [v2.4.0] - 历史更新 (Telegram Bot & Headless Chrome & Performance Ultra)
#### 🌟 核心特性 (New Features)
* **全功能 Telegram 机器人接管 (Telegram Bot Console)**：引入了纯原生的 Telegram Bot 控制台底座。通过绑定专属 `ChatID` 实现严格的安全鉴权。支持通过 `/list`, `/add`, `/pause`, `/resume`, `/status` 等指令实现脱离 Web UI 的全天候极速移动端管控，并内置了针对超长监控列表的“智能分片发送”引擎，突破单条消息字符限制。
* **无头浏览器短链解析 (Headless Chrome Parsing)**：彻底重构短链接解析引擎。引入 `chromedp` 无头浏览器进行深度穿透，完美绕过重定向与前端风控滑块；同时辅以原生 HTTP 客户端作为保底降级策略 (Fallback)，实现抖音等平台短链到真实房间号的 100% 极速无感解析。
* **现代级 PushPlus 微信通知 (Modern WeChat Notify)**：升级了通知模块，全面接入 PushPlus 接口。结合现代化的 SVG 矢量图标与情感色彩体系（科技蓝、现代绿、警示橙、危险红），为开播、下播、系统熔断等关键事件提供高颜值的富文本卡片推送。

#### 🚀 性能与架构极致优化 (Performance & Architecture)
* **O(1) 增量聚合图表引擎 (O(1) Incremental Stats)**：彻底重构了数据大盘的计算逻辑。废弃了每次刷新均需全量遍历五十万条记录的 O(N) 灾难级消耗，改用 `sync.Map` 构建高并发无锁的增量聚合池 (`trendStats` 与 `rankStats`)。微秒级吐出图表数据，彻底铲除了高负载下的 CPU 尖峰卡顿。
* **成功记录脏标记落盘剥离 (Dirty Write I/O Optimization)**：【修复 Bug 5】重构了海量成功日志的持久化机制。引入 `successLogDirty` 原子脏标记与独立的 `successLogPersistLoop` 后台守护协程。将全量 JSON 重写操作改为在内存中缓存并在每 15 秒批量合并落盘一次。将磁盘 I/O 压力与 GC 开销降低了数个数量级。
* **HTTP 连接池全局复用 (HTTP Connection Pool)**：为全局 `httpCli` 配置了底层的 TCP Keep-Alive 连接池（`MaxIdleConns` 等参数）。在高频检测与海量小文件通信时，免去了频繁握手挥手的开销，极大降低了网络层的 CPU 与内存消耗。
* **WS 广播长短周期分离 (Slow/Fast WS Broadcaster)**：优化前端 WebSocket 推送性能。将高频变化数据（队列、流量）的 2 秒级快刷新与重度 I/O 数据（硬盘容量、目录遍历）的 10 秒级慢刷新进行分离降级，避免前端雷达高频拉取打满底层 I/O。

#### 🔧 核心修复与稳定性 (Bug Fixes & Stability)
* **Token 语义安全隔离 (Token Semantic Isolation)**：【修复 Bug 7】修复了远端 Openlist 服务器鉴权 Token 与前端 Dashboard 本地登录 Token 共用单一变量导致互相覆盖的严重鉴权混乱问题。现已将两者在内存与加锁维度（`tokenMu` / `dashboardTokenMu`）完全物理隔离。
* **开播通知防抖护盾 (Notification Debounce)**：针对弱网环境下 FFmpeg 频繁断流重连导致的微信/TG“开播-下播”消息轰炸现象，在内核中引入了 `builtinNotifyDebounce` 并发防抖字典。强制设立 3 分钟的冷却缓冲期，实现无感知的底层静默重连恢复。
* **ECharts 渲染重叠修复 (ECharts Render Fix)**：修复了由于 Vue 响应式数据频繁驱动导致 ECharts 饼状图中心文字残留与图层重叠的排版 Bug（新增 `show: false` 显式屏蔽）。

---
### [v2.3.2] - 历史更新 (Concurrency Lock & Huge File Fix)
#### 🔧 核心修复与优化 (Bug Fixes & Optimization)
* **超大文件上传超时修复 (Huge File Timeout Fix)**：将全局 HTTP 客户端 (`httpCli`) 的 `Timeout` 强制设为 `0`。彻底解决了在上传数十 GB 超大文件或极端弱网环境下，因传输耗时过长导致底层连接被系统强行掐断，进而触发安全熔断误挂起的严重 Bug。
* **高并发读写锁重构 (RWMutex Concurrency Polish)**：全面重构了底层的并发数据结构（`wsClients`, `sessionKeys`, `dirStatuses`, `hashCache` 等）。摒弃了粗暴的全局大锁，转而采用 Go 原生 `map` 配合细粒度的 `sync.RWMutex` 读写锁。特别是在 WebSocket 广播流中引入了“读锁快照拷贝”安全分发机制，从根本上消除了 `concurrent map iteration and map write` 的引擎崩溃风险与广播假死问题。
* **哈希秒传全内存化 (In-Memory Hash Cache)**：重构了 `uploaded_hash.db` 的读写隔离逻辑。秒传判定 `hashExists` 现已完全剥离磁盘 I/O 阻塞，依靠纯内存级别的并发读写锁实现 O(1) 极速匹配，极大提升了十万级海量文件并发扫盘时的系统效能。
* **前端录制列表防闪烁 (Built-in Tasks Anti-Flicker)**：优化了前端 `index.html` 接收内置引擎状态推送 (`builtinTasks`) 的渲染逻辑。摒弃了低效的数组全量覆写，改为精准追踪 `room_id` 进行属性的“原地操作与合并”。完美化解了 WebSocket 每秒高频推送时引发的 DOM 重绘与直播封面图频闪问题。

### [v2.3.1] - 历史更新 (Zero-Byte Shield & UI Stability)
#### 🔧 核心修复 (Bug Fixes)
* **空切片物理粉碎 (Zero-Byte File Shredding)**：修复了由于 FFmpeg 异常断流或卡顿残留的 `0 Bytes` 空视频切片，导致上传引擎向远端发起无载荷的非法请求，进而触发远端网关 `500 Internal Server Error` 以及本地系统连续 30 次失败后最高级安全熔断挂起的致命 Bug。现在，扫描引擎与处理器已构构筑双重护盾，一旦发现 0 字节死文件将直接在底层物理销毁，彻底净化磁盘环境。
* **浮点除零异常修复 (NaN Progress Fix)**：修复了底层文件流进度读取器 (`ProgressReader`) 在处理空文件时，因分母计算为 0 导致浮点数运算出现 `NaN` (Not a Number) 的安全隐患。彻底清除了由此引发的前端 WebSocket 数据解析异常与界面崩溃风险。
* **远端拦截明细透传 (Remote Error Exposing)**：优化了 HTTP 客户端的错误捕捉逻辑。当远端服务器拒绝接收文件（如格式不符或抛出异常）时，系统不再笼统提示“解析响应体失败”，而是精准截获并向前端拦截气泡透传远端的真实状态码（如 500）与详细报错信息，极大提升了排错效率。

---
### [v2.3.0] - 历史更新 (Zero Dependency & Origin Quality)
#### 🌟 核心特性 (New Features)
* **抖音原画级录制 (Origin Quality Support)**：全面升级内置录制引擎的画质嗅探逻辑。当用户选择最高画质时，底层将优先去平台字典中提取 `ORIGIN1` 或 `ORIGIN` 无损原画流，若主播未开启原画则自动平滑降级至 `FULL_HD1` (蓝光)，彻底释放最高录制画质潜力。
* **画质天花板级截帧 (Lossless Cover Extraction)**：全面升级内置引擎的旁路抽帧黑科技。采用 I 帧 (关键帧) 精准提取与无损 PNG 编码引擎 (`-c:v png`)，彻底解除原有 JPEG 的有损压缩限制。直播封面截图画质达到无损天花板（单张体积可突破 1MB~3MB），完美保留原始直播画面的每一处高清细节。

#### 🔧 底层与架构调整 (Backend & Architecture)
* **零依赖硬件探针 (Zero-Dependency Monitoring)**：为了大幅降低跨平台编译的复杂度和最终二进制文件的体积，彻底剥离了沉重的 `gopsutil` 第三方硬件库。重构为利用 Go 原生标准库调用底层跨平台 Shell 命令 (`wmic`/`df`/`tasklist`/`ps`) 来实现极低开销的磁盘与内存监控。
* **高并发 IO 锁修复 (Concurrency I/O Fix)**：修复了在高并发触发秒传或写入成功日志时，导致 `uploaded_hash.db` 数据库文件尾部出现乱码、结构破坏或漏检的严重 Bug。全面引入 `sync.RWMutex` 读写锁，保障多 Worker 状态下的绝对数据安全。

#### 🎨 界面与交互优化 (UI/UX)
* **严谨的界面术语 (UI Terminology)**：为配合底层原画抓取能力的升级，将前端引擎全局设置中的“蓝光/超清”术语严谨地更正为“原画/蓝光 (最高画质)”。
* **语法规范修复 (Syntax Polish)**：修复了前端 Vue/Arco Design 组件树中存在的下拉框闭合标签错误 (`</select>` 修正为 `</a-select>`)，提升了 DOM 的健壮性。

---
### [v2.2.0] - 历史更新 (Hardware Monitoring & UI Polish)
#### 🌟 核心特性 (New Features)
* **全局硬件资源监控 (Hardware Resource Monitoring)**：底层引擎全面接入 `gopsutil` 硬件级探针，新增对系统磁盘剩余可用空间（Disk Free）与底层 FFmpeg 录制进程物理内存占用（RSS Memory）的毫秒级实时计算与下发。帮助用户直观洞察系统存储瓶颈与运行期内存负载，彻底告别“盲盒式”录制。

#### 🎨 界面与交互优化 (UI/UX)
* **自适应流式卡片重构 (Responsive Cards Polish)**：重构了【系统概览】顶部核心数据指标的响应式栅格系统（由三列优化为平滑的四/六列布局）。
* **防撑爆折行保护 (Anti-Wrap Protection)**：针对数据面板引入了严格的 `white-space: nowrap` 防换行保护机制与动态字号（Desktop 22px / Mobile 18px）自适应缩放引擎。彻底解决了当监控数值字符过长（如 "18.06 MB"）时，文本换行导致卡片高度被异常撑爆、整体排版错位的不美观问题。

---
### [v2.1.0] - 历史更新 (Dynamic RSA+AES API Encryption)
#### 🌟 核心特性 (New Features)
* **商业级动态安全信道 (Dynamic Security)**：引入了基于 RSA-2048 和 AES-256-GCM 的完美前向保密 (PFS) 动态密钥交换机制。前端每次加载自动换取专属 SessionID 与 AES 临时密钥，对 API 传输载荷进行全链路端到端加密，彻底防止中间人 (MITM) 抓包窃听与重放攻击。
* **可视化加密开关 (Encryption Toggle)**：在前端“上传参数”设置面板中新增了“API加密通信”热开关。可随时在明文调试与密文安全模式间无缝动态切换，兼顾极客开发与生产环境的高强度安全需求。
* **无感加解密拦截器 (Seamless Interceptor)**：重构了前端 Axios 请求/响应拦截器与后端 HTTP 中间件，自动完成 JSON 结构体到 Base64 密文的装箱与拆箱，对上层业务代码实现 100% 零侵入透明兼容。

---
### [v1.9.0] - 历史更新 (Ultra-Realtime & Cookie Engine)
#### 🌟 核心特性 (New Features)
* **终极无损内存截帧 (Memory Dual-Output)**：彻底重构内置录制引擎的 FFmpeg 底层调用，引入多通道输出（Dual-Output）黑科技。系统只建立 1 个网络连接，在解码时直接从内存中剥离视频流输出封面。彻底终结了“网络并发抢占被 CDN 403 拦截”与“读写本地 MP4 触发 `moov atom not found`”的行业级痛点。
* **智能防盗链代理 (Smart Anti-Leech Proxy)**：新增后端高防图片反向代理接口。自动携带全套真实浏览器请求头（Accept、Language 等）去获取主播默认头像，并内置了“智能无 Referer 降级重试”机制，100% 免疫各大平台的防盗链 403 拦截，彻底告别裂图。
* **独立截图归档目录 (Screenshot Archiving)**：内置引擎每 20 秒生成的实时画面不仅会推送到前端大屏，还会自动在视频录像同级创建一个独立的 `Screenshots` 文件夹，并按顺序存档高帧率封面图，交由扫描系统自动跟随视频上传至云盘，实现完美图文归档。

#### 🎨 界面与交互优化 (UI/UX)
* **全局独立大图预览 (Global Image Preview)**：重构了前端的图片预览逻辑，剥离了组件自带的内置预览，在系统顶级层挂载独立的 `<a-image-preview>`。现在即使后台每 20 秒不断推送新画面，正在打开的大图预览弹窗绝对不会消失，而是会像幻灯片一样无缝平滑闪切成最新画面！
* **DOM 原地复用防闪烁 (DOM Reuse & Anti-Flicker)**：为前端所有数据表格深度绑定了精准的 `row-key`。在 WebSocket 每秒高频狂推数据时，Vue 引擎只会原地精准更新变化的时间和图片 URL，即使进行勾选、输入、放大等交互也绝不会被打断。

#### 🔧 底层与架构调整 (Backend & Architecture)
* **安全环形报错日志 (Tail Buffer)**：为底层的 FFmpeg 挂载了自定义的 `builtinTailBuffer`。当发生任何导致录制中断的严重错误时，系统不再“沉默背锅”，而是将截获的底层原装红字报错全量打印至控制台。

---
### [v1.8.0] - 历史更新 (Dual-Engine Hot-Reload & WS Deadlock Fix)
* **内置引擎底层热重载**：实现底层 `urls.txt` 极速热重载，三秒内全终端同步。
* **主播名称自动固化**：抓取到的主播名称自动固化写入底层文件，重启永不丢失。
* **瞬间中断唤醒机制**：内置引擎捕获开播后下发硬件级硬中断，“踹醒”扫描休眠器极速接管任务。
* **彻底根除 WS 假死**：拆分全局大锁为 `WSClient` 独立读写小锁，杜绝因网页刷新导致的底层 I/O 阻塞死锁。
* **苹果 Safari 防溢出**：修复手机端横向撑爆问题，支持封面图原生 `Blob` 拦截下载。

---
### [v1.7.2] - 历史更新 (Dashboard & Real-time Alerts)
* **动态可视化大盘**：基于 ECharts 引入 7 日上传流量双 Y 轴面积趋势图与硬盘消耗 TOP 5 热力榜。
* **全局实时强告警**：底层遭遇 Token 失效、网络断开等严重故障时，立刻通过 WS 下推拦截式红色强弹窗。
* **断电/重启状态自愈**：重启后饼图等核心状态数据将从本地日志自动回填恢复。

---
### [v1.7.1] - 历史更新 (Quick Search & Mobile Fixes)
* **极速主播检索**：新增实时搜索栏，支持瞬间通过主播名或链接过滤列表。
* **键盘党福利**：引入 `Ctrl + X` 快捷键一键聚焦搜索框。
* **移动端布局重构**：修复窄屏下控制栏右侧按钮被挤出屏幕的问题。

---
### [v1.7.0] - 历史更新 (Ultra-Realtime & Cookie Engine)
* **无损 INI 覆写**：支持网页端可视化热修改多平台（Douyin, Kuaishou, Soop） Cookie 且绝不损坏原配置。
* **零延迟终端日志**：物理级终端滚动追尾引擎重构，改为事件驱动的 WebSocket 单点突发零延迟推送。
* **跨设备协同热刷新**：在一个设备上修改名单，其他在线设备均会全自动同步。

---
### [v1.6.1] - 历史更新 (Full-Duplex WS & API Refactoring)
* **全栈 WebSocket 双工通信接管**：彻底废弃 `setInterval` 轮询，将大盘状态与队列进度全面转移至 WebSocket。
* **智能推流引擎**：新增独立驻留协程，实现无客户端时不消耗任何资源的零网络开销时钟。