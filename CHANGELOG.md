# 📝 Go Auto Uploader 迭代更新日志 (Changelog)

### [v2.3.1] - 最新更新 (Zero-Byte Shield & UI Stability)
#### 🔧 核心修复 (Bug Fixes)
* **空切片物理粉碎 (Zero-Byte File Shredding)**：修复了由于 FFmpeg 异常断流或卡顿残留的 `0 Bytes` 空视频切片，导致上传引擎向远端发起无载荷的非法请求，进而触发远端网关 `500 Internal Server Error` 以及本地系统连续 30 次失败后最高级安全熔断挂起的致命 Bug。现在，扫描引擎与处理器已构筑双重护盾，一旦发现 0 字节死文件将直接在底层物理销毁，彻底净化磁盘环境。
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