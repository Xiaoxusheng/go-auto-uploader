package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"upload/internal/recorder"

	"upload/internal/config"
	"upload/internal/convert"
	"upload/internal/hashstore"
	"upload/internal/logx"
	"upload/internal/naming"
	"upload/internal/ratelimit"
	"upload/internal/remote"
	"upload/internal/scanner"
	"upload/internal/storage"
	"upload/internal/uploader"
)

// appCfg 读取配置快照（单源 cfgStore）
func appCfg() config.Config { return cfgStore.Get() }

/* ================= 全局配置 ================= */

var (
	dirs               string // 扫描目录
	server             string // 远端服务器地址
	workers            int    // 并发 Worker 数量
	rateMB             int    // 手动限速
	dayRateMB          int    // 白天限速
	nightRateMB        int    // 夜晚限速
	reportMinutes      int    // 邮件报告间隔
	scanningInterval   int    // 扫描间隔
	webPort            int    // Web 端口
	liveConfigPath     string // 录制配置路径
	recorderContainer  string // Docker 容器名
	recorderConfigPath string // 主配置路径

	dashboardUsername = "admin"
	dashboardPassword = "admin" // 安全审计：默认弱口令，可通过 config.json 的 dashboardUser/dashboardPass 覆盖，启动时若仍为默认值会打印高危告警

	// 优化点：为 httpCli 配置连接池，复用空闲连接，极大减少频繁新建 TCP 请求带来的内存和 CPU 消耗
	httpCli = &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			MaxIdleConns:        100,              // 最大空闲连接数
			MaxIdleConnsPerHost: 20,               // 每个 host 最大空闲连接
			IdleConnTimeout:     90 * time.Second, // 空闲连接保活时间
		},
	}

	// remoteClient OpenList/AList 远端客户端（登录 + PUT）
	remoteClient *remote.OpenListClient

	hashFile = "uploaded_hash.db" // 已经上传的文件哈希记录（秒传用）

	// hashDB 由 internal/hashstore 承载秒传去重
	hashDB *hashstore.Store

	queueCount   int64 // 等待队列数量（展示用镜像，真源为 taskQueue.Pending）
	activeWorker int64 // 当前活跃的 Worker 数量（展示用镜像，真源为 uploadPool.Active）

	// 暴露给前端的动态扫描周期与倒计时时间戳
	currentDynamicIntervalGlobal int64
	nextScanTimeGlobal           int64

	// 熔断保护机制：连续失败次数计数器
	consecutiveFailures int32
)

// 动态应用配置：单源 Store，禁止再散落 appConfigMu 双锁
var cfgStore = config.NewStore("config.json")

// appLogs 应用日志环形缓冲（Web 终端）
var appLogs = logx.NewStore(5000, 1000)

// 任务队列和历史记录状态 (极致性能优化：全量替换为 sync.Map 和 原子计算)
var (
	liveTasks sync.Map // key: taskID string, value: *Task

	// 队列并发安全字典，充当 Set 的功能，O(1) 的存取与删除
	queueUploading sync.Map
	queueSuccess   sync.Map
	queueFail      sync.Map
	queueRetrying  sync.Map

	// 高性能原子队列计数器
	queueUploadingCount int64
	queueSuccessCount   int64
	queueFailCount      int64
	queueRetryingCount  int64

	// 本地持久化（internal/storage）
	historyStore   = storage.NewHistoryStore(1000)
	successStore   = storage.NewSuccessStore(successLogFile, 500000)
	dirStatusStore = storage.NewDirStatusStore(dirStatusFile)

	// 上传队列 + Worker 池（internal/uploader）
	taskQueue  = uploader.NewQueue(100000)
	uploadPool *uploader.WorkerPool
	appCtx     context.Context
	appCancel  context.CancelFunc

	// ✨ 记录外部引擎活跃主播字典，用于对比下发开播/下播通知
	lastActiveMap   = make(map[string]bool)
	lastActiveMapMu sync.Mutex
)

var (
	triggerScanCh   = make(chan string, 1)   // 触发强制扫描的通道
	triggerReportCh = make(chan struct{}, 1) // 触发报告重置的通道
)

// triggerScan 触发强行跨越休眠阶段的强制目录扫描
func triggerScan(reason string) {
	select {
	case triggerScanCh <- reason:
	default:
	}
}

// triggerReportReset 触发邮件报告倒计时及生成逻辑的通道重置操作
func triggerReportReset() {
	select {
	case triggerReportCh <- struct{}{}:
	default:
	}
}

// fileTask 用于收集扫描阶段发现的文件及其大小，以便进行智能调度排序
type fileTask struct {
	path string
	size int64
}

// Task 定义了单个上传任务运行时的动态属性和状态
// 优化：内嵌专属读写锁，隔离并发争抢
type Task struct {
	Mu         sync.RWMutex
	ID         string
	Name       string
	Path       string
	Remote     string
	Size       int64
	Progress   int
	Speed      int64
	WorkerID   int
	Status     string
	RetryCount int
	CreatedAt  time.Time
	EndTime    time.Time
	Error      string
}

// HistoryRecord 上传历史条目（类型见 internal/storage）
type HistoryRecord = storage.HistoryRecord

// UploadRecord 成功统计条目
type UploadRecord = storage.UploadRecord

// TrendPoint 单日流量聚合
type TrendPoint = storage.TrendPoint

// 常量定义，包含安全的前置远端目录与本地磁盘化缓存文件
const (
	safeBaseDir    = "/home/_safe_uploads" // 远端安全目录
	successLogFile = "upload_success.json" // 本地成功日志，用于图表统计
	dirStatusFile  = "dir_status.json"     // 本地目录状态统计持久化文件
)

// saveConfigToFile 将当前配置原子落盘至 config.json
func saveConfigToFile() {
	if err := cfgStore.Save(); err != nil {
		log.Printf("[CONFIG][ERR] 无法保存配置文件 config.json: %v", err)
	}
}

// loadDirStatuses 启动时从本地磁盘恢复各个监控目录的累计上传数据，防止重启清空
func loadDirStatuses() {
	dirStatusStore.Load()
}

// markDirStatusDirty 标记内存中的目录状态已被更改，触发后台异步落盘
func markDirStatusDirty() {
	dirStatusStore.MarkDirty()
}

// flushDirStatuses 提取内存中的最新目录状态并执行物理层面的覆盖落盘
func flushDirStatuses() {
	dirStatusStore.Flush()
}

// dirStatusPersistLoop 驻留于后台的合并落盘守护协程，按固定心跳检查并持久化状态
func dirStatusPersistLoop() {
	stop := make(chan struct{})
	if appCtx != nil {
		go func() { <-appCtx.Done(); close(stop) }()
	}
	dirStatusStore.PersistLoop(5*time.Second, stop)
}

// manageWorkers 启动 internal/uploader Worker 池，并镜像计数到展示用原子变量
func manageWorkers() {
	appCtx, appCancel = context.WithCancel(context.Background())
	uploadPool = uploader.NewWorkerPool(taskQueue, func(ctx context.Context, path string) {
		atomic.AddInt64(&activeWorker, 1)
		defer atomic.AddInt64(&activeWorker, -1)
		handleFile(path)
	}, func() bool {
		runningMu.RLock()
		defer runningMu.RUnlock()
		return !running
	})
	uploadPool.Start(appCtx, func() int { return appCfg().Workers })

	// 将队列 pending 同步到 queueCount，兼容现有 status API
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-appCtx.Done():
				return
			case <-t.C:
				atomic.StoreInt64(&queueCount, taskQueue.Pending())
			}
		}
	}()
}

// pauseSystemOnFailure 当网络故障或远端拒绝频繁到达阈值时，触发系统自动保护熔断挂起功能，并通过微信向管理员发起强警告
func pauseSystemOnFailure(reason string) {
	runningMu.Lock()
	wasRunning := running
	running = false
	runningMu.Unlock()

	if wasRunning {
		log.Printf("[SYSTEM][AUTO-PAUSE] 🚨 触发安全熔断机制: %s", reason)
		// 向前端下发最高优先级的系统强弹窗警告
		SendAlert("error", "🛑 触发系统熔断保护", reason+"\n为防止本地任务大量报错，上传引擎已自动挂起！请检查远端服务器状态后，手动点击【启动扫描】恢复运行。")
		// 新增：向管理员微信推送重大中断警报
		sendWeChatNotify("异常通知", fmt.Sprintf("系统已触发自动保护熔断并挂起队列上传。异常原因：\n%s", reason))
	}
}

// main 程序的入口执行点，处理命令行传参，初始化并发模型与开启事件轮询死循环
func main() {
	// 解析命令行参数
	flag.StringVar(&dirs, "dirs", "", "扫描目录(逗号分隔)")
	flag.StringVar(&server, "server", "http://127.0.0.1:5244", "服务器")
	flag.IntVar(&workers, "workers", 3, "并发")
	flag.IntVar(&rateMB, "rate", 0, "手动限速 MB/s")
	flag.IntVar(&dayRateMB, "day-rate", 20, "白天限速 MB/s")
	flag.IntVar(&nightRateMB, "night-rate", 80, "夜晚限速 MB/s")
	flag.IntVar(&scanningInterval, "scan-interval", 30, "默认30min扫描一次")
	flag.IntVar(&reportMinutes, "report-minutes", 360, "邮件统计分钟")
	flag.IntVar(&webPort, "web-port", 8080, "Web API 端口")
	flag.StringVar(&liveConfigPath, "live-config", "/home/live/DouyinLiveRecorder/config/URL_config.ini", "录制配置文件路径")
	flag.StringVar(&recorderContainer, "recorder-container", "douyinliverecorder-app-1", "录制引擎Docker容器名")
	flag.StringVar(&recorderConfigPath, "recorder-config", "", "录制引擎主配置文件(config.ini)路径")
	flag.Parse()

	if dirs == "" {
		log.Fatal("必须指定 -dirs")
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	// 启用日志拦截器，将日志传回加密网页
	log.SetOutput(&logx.Interceptor{Original: os.Stdout, Store: appLogs})

	// 启动时读取本地上传成功记录缓存入内存，并重建增量聚合
	successStore.Load()

	// 启动时一次性加载哈希库到内存，后续 Exists/Save 全走内存
	hashDB = hashstore.New(hashFile)
	hashDB.Load()

	// 启动时加载硬盘目录累积统计数据
	loadDirStatuses()

	// 初始化系统参数：优先从 config.json 读取；如果文件不存在，则使用启动参数进行填充
	if err := cfgStore.LoadFromDisk(config.CLI{
		Dirs:               dirs,
		Server:             server,
		Workers:            workers,
		DayRate:            dayRateMB,
		NightRate:          nightRateMB,
		ScanInterval:       scanningInterval,
		ReportMinutes:      reportMinutes,
		LiveConfigPath:     liveConfigPath,
		RecorderContainer:  recorderContainer,
		RecorderConfigPath: recorderConfigPath,
	}); err != nil {
		log.Printf("[CONFIG][ERR] 加载配置失败: %v", err)
	}

	// 安全审计修复：允许通过 config.json 的 dashboardUser/dashboardPass 覆盖内置默认账号
	if appCfg().DashboardUser != "" {
		dashboardUsername = appCfg().DashboardUser
	}
	if appCfg().DashboardPass != "" {
		dashboardPassword = appCfg().DashboardPass
	}
	weakCreds := dashboardUsername == "admin" && dashboardPassword == "admin"
	if weakCreds {
		log.Println("[AUTH] 🚨 高危告警：控制台仍在使用默认弱口令 admin/admin，请立即在 config.json 中配置 dashboardUser 与 dashboardPass！")
	}

	// 在启动时尝试生成一次文件，确保 config.json 始终存在
	saveConfigToFile()

	addLog("info", "系统初始化完成，启动中...", "")

	// 启动并挂载具有商业级加密防护特征的 API 服务容器和各项异步常驻任务
	go StartWebServer(webPort)
	go queueStatusLoop()
	go reportLoop()
	go dirStatusPersistLoop()  // 挂载高性能异步合并落盘守护机制
	go successLogPersistLoop() // 挂载成功记录批量落盘守护机制（修复 Bug 5）
	go manageWorkers()         // 挂载全局异步的 Worker 上传线程池

	// SIGINT/SIGTERM：停止接收新任务并取消 Worker 池（在跑的 handleFile 会随 ctx 结束）
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("[SYSTEM] 收到退出信号 %v，正在优雅停机…", sig)
		runningMu.Lock()
		running = false
		runningMu.Unlock()
		if appCancel != nil {
			appCancel()
		}
		// 短暂等待 Worker 退出后强制结束
		time.Sleep(2 * time.Second)
		os.Exit(0)
	}()

	baseInterval := appCfg().ScanInterval
	currentDynamicInterval := baseInterval
	atomic.StoreInt64(&currentDynamicIntervalGlobal, int64(currentDynamicInterval))

	runningMu.RLock()
	if running {
		_ = login() // 获取远端 token
		activeCount := runOnce("auto", currentDynamicInterval)

		// 合并读取内置录制引擎的工作状态
		builtinRecordingCount := 0
		/* 若需要对接内置录制状态，可在此恢复代码
		for _, t := range GetBuiltinRecorderTasks() {
			if isBuiltinLiveStatus(t.Status) {
				builtinRecordingCount++
			}
		}
		*/
		totalActiveCount := int(activeCount) + builtinRecordingCount // 外置变动数 + 内置引擎正在录制数

		if totalActiveCount > 0 {
			// 如果有正在录制的文件，动态加快扫描频率
			currentDynamicInterval = baseInterval / (totalActiveCount + 1)
			if currentDynamicInterval < 3 {
				currentDynamicInterval = 3
			}
			atomic.StoreInt64(&currentDynamicIntervalGlobal, int64(currentDynamicInterval))
			log.Printf("[SCAN][DYNAMIC] 🔥 开机检测到 %d 个文件正在录制 (外部: %d, 内置: %d)，下次扫描已动态提速至 %d 分钟后", totalActiveCount, activeCount, builtinRecordingCount, currentDynamicInterval)
		}
	}
	runningMu.RUnlock()

	// 主扫描时间调度轮询死循环，利用管道与定时器响应系统心跳
	for {
		runningMu.RLock()
		isRunning := running
		runningMu.RUnlock()

		if isRunning {
			// 每次进入休眠前，精确计算并暴露下一次唤醒的时间戳给前端雷达
			targetTime := time.Now().Add(time.Duration(currentDynamicInterval) * time.Minute)
			atomic.StoreInt64(&nextScanTimeGlobal, targetTime.UnixMilli())

			select {
			case <-time.After(time.Until(targetTime)):
				_ = login()
				baseInterval = appCfg().ScanInterval

				activeCount := runOnce("auto", currentDynamicInterval)

				// 合并读取内置录制引擎的工作状态
				builtinRecordingCount := 0
				totalActiveCount := int(activeCount) + builtinRecordingCount

				if totalActiveCount > 0 {
					newInterval := baseInterval / (totalActiveCount + 1)
					if newInterval < 3 {
						newInterval = 3
					}
					if newInterval != currentDynamicInterval {
						log.Printf("[SCAN][DYNAMIC] 🔥 当前有 %d 个主播处于录制写入状态 (外部: %d, 内置: %d)，按比例调整下次扫描为 %d 分钟后", totalActiveCount, activeCount, builtinRecordingCount, newInterval)
					}
					currentDynamicInterval = newInterval
					atomic.StoreInt64(&currentDynamicIntervalGlobal, int64(currentDynamicInterval))
				} else {
					if currentDynamicInterval != baseInterval {
						log.Printf("[SCAN][DYNAMIC] 🟢 当前暂无录制任务，扫描间隔回归省电模式: %d 分钟", baseInterval)
					}
					currentDynamicInterval = baseInterval
					atomic.StoreInt64(&currentDynamicIntervalGlobal, int64(currentDynamicInterval))
				}

			case reason := <-triggerScanCh:
				_ = login()
				log.Printf("[SYSTEM] ⚡ 收到指令打断，执行扫描 (触发源: %s)", reason)

				baseInterval = appCfg().ScanInterval

				activeCount := runOnce(reason, currentDynamicInterval)

				builtinRecordingCount := 0
				totalActiveCount := int(activeCount) + builtinRecordingCount

				if totalActiveCount > 0 {
					newInterval := baseInterval / (totalActiveCount + 1)
					if newInterval < 3 {
						newInterval = 3
					}
					currentDynamicInterval = newInterval
					atomic.StoreInt64(&currentDynamicIntervalGlobal, int64(currentDynamicInterval))
				} else {
					currentDynamicInterval = baseInterval
					atomic.StoreInt64(&currentDynamicIntervalGlobal, int64(currentDynamicInterval))
				}
			}
		} else {
			// 系统挂起时，倒计时归零
			atomic.StoreInt64(&nextScanTimeGlobal, 0)
			select {
			case <-time.After(5 * time.Second):
			case <-triggerScanCh:
				log.Println("[SYSTEM] ⚡ 系统重新启动或热重载完成")
			}
		}
	}
}

// queueStatusLoop 定时收集并行阻塞与空转统计等运行期监控参数向日志推流展示
func queueStatusLoop() {
	start := time.Now()
	ticker := time.NewTicker(10 * time.Second)
	for range ticker.C {
		currentWorkers := appCfg().Workers

		log.Printf("[QUEUE][STATUS] 运行时长:%s | 等待任务:%d | 活动Worker:%d | 并发设定:%d",
			time.Since(start).Truncate(time.Second),
			atomic.LoadInt64(&queueCount),
			atomic.LoadInt64(&activeWorker),
			currentWorkers,
		)
	}
}

// runOnce 发起对本地全部监控目录树枝条叶的深度检索、合法过滤，并将任务下发给常驻异步任务池
func runOnce(triggerReason string, currentDynamicInterval int) int {
	atomic.StoreInt64(&nextScanTimeGlobal, -1) // -1 表示前端显示"正在同步目录状态..."

	if triggerReason == "start" || triggerReason == "rescan" {
		atomic.StoreInt32(&consecutiveFailures, 0)
	}

	currentWorkers := appCfg().Workers
	currentDirs := make([]string, len(appCfg().Dirs))
	copy(currentDirs, appCfg().Dirs)
	enableUpload := appCfg().EnableUpload // ✨ 获取全局上传开关

	runningMu.RLock()
	isRunning := running
	runningMu.RUnlock()

	if !isRunning {
		log.Println("[UPLOAD][PAUSED] 系统处于暂停状态，跳过本轮扫描")
		return 0
	}

	log.Printf("[SCAN][START] 🔍 启动目录探测，并发Workers:[%d] 目标路径:[%s]", currentWorkers, strings.Join(currentDirs, " | "))

	broadcastWS("scanStarted", map[string]interface{}{
		"time":     time.Now().UnixMilli(),
		"dirs":     currentDirs,
		"interval": currentDynamicInterval,
		"workers":  currentWorkers,
		"trigger":  triggerReason,
	})

	// 初始化目录统计状态 (保留已上传的数据，每次重头严谨计算 Pending 数以保证面板精准)
	for _, root := range currentDirs {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "." || root == "" {
			continue
		}
		if ds, exists := dirStatusStore.Get(root); exists {
			_ = ds
			ds.Mu.Lock()
			ds.PendingFiles = 0
			ds.TotalSize = 0
			ds.LastScanTime = time.Now().UnixMilli()
			ds.Mu.Unlock()
		} else {
			dirStatusStore.Put(root, &DirStatus{
				Path:         root,
				LastScanTime: time.Now().UnixMilli(),
			})
		}
	}

	var newlyAddedFiles int32 = 0

	// 扫描下沉到 internal/scanner：只产出候选，不直接上传
	// 上传关闭时：仍统计目录，但不产出可入队候选
	isQueuedFn := func(path string) bool { return taskQueue.Contains(path) }
	if !enableUpload {
		isQueuedFn = func(string) bool { return true }
	}
	var scanErrCount int32
	scanRes := scanner.Scan(context.Background(), scanner.Options{
		Dirs:     currentDirs,
		IsQueued: isQueuedFn,
		OnFile: func(root, path string, size int64) {
			// 目录统计：无论是否入队，合法文件都计入 Pending/TotalSize
			if ds, exists := dirStatusStore.Get(root); exists {
				_ = ds
				ds.Mu.Lock()
				ds.PendingFiles++
				ds.TotalSize += size
				ds.Mu.Unlock()
			}
		},
		OnZeroByte: func(path string) {
			log.Printf("[SCAN][CLEAN] 检测到遗留的 0 字节无效切片，已自动物理删除: %s", path)
		},
		OnError: func(path string, err error) {
			if err == nil || err == context.Canceled {
				return
			}
			n := atomic.AddInt32(&scanErrCount, 1)
			log.Printf("[SCAN][ERR] 访问路径出错 %s: %v", path, err)
			// 仅首次错误触发告警，避免整树权限拒绝时刷屏
			if n == 1 {
				SendAlert("warning", "目录扫描异常", "无法访问部分路径: "+err.Error())
				addLog("error", "文件遍历失败", err.Error())
			}
		},
		IsRunning: func() bool {
			runningMu.RLock()
			defer runningMu.RUnlock()
			return running
		},
	})

	activeRecordingCount := scanRes.Active
	collectedTasks := make([]fileTask, 0, len(scanRes.Candidates))
	for _, c := range scanRes.Candidates {
		collectedTasks = append(collectedTasks, fileTask{path: c.Path, size: c.Size})
	}

	// 智能调度核心-修改版：解决大文件全部扎堆导致通道严重拥堵的问题
	// 1. 先按大小降序排列
	sort.Slice(collectedTasks, func(i, j int) bool {
		return collectedTasks[i].size > collectedTasks[j].size
	})

	// 2. 双指针交替提取：按 [第二大, 最小, 第三大, 次小...] 穿插；
	//    绝对最大文件挪到序列末尾，避免开场 Worker 被超大文件长期占满
	if len(collectedTasks) > 1 {
		largest := collectedTasks[0]
		rest := collectedTasks[1:]
		var mixedTasks []fileTask
		left, right := 0, len(rest)-1
		for left <= right {
			mixedTasks = append(mixedTasks, rest[left])
			left++
			if left <= right {
				mixedTasks = append(mixedTasks, rest[right])
				right--
			}
		}
		collectedTasks = append(mixedTasks, largest)
	}

	// 将排序及混合完成后的任务抛入去重队列
	for _, t := range collectedTasks {
		if taskQueue.Enqueue(t.path) {
			atomic.AddInt64(&queueCount, 1)
			atomic.AddInt32(&newlyAddedFiles, 1)
		}
	}

	// 在扫描终点，将累加出的准确待处理量 + 内存记录的成功上传量 = 当下的总文件数
	dirStatusStore.Range(func(_ string, ds *DirStatus) bool {
		ds.Mu.Lock()
		ds.TotalFiles = ds.PendingFiles + ds.UploadedFiles
		ds.Mu.Unlock()
		return true
	})
	markDirStatusDirty() // 状态持久化落盘

	log.Printf("[SCAN][END] 🏁 本轮扫描完毕。发现新文件: %d 个 | 仍在录制中文件: %d 个", newlyAddedFiles, activeRecordingCount)

	broadcastWS("scanFinished", map[string]interface{}{
		"time":  time.Now().UnixMilli(),
		"added": newlyAddedFiles,
	})

	// 绝不等待工作线程，马上把控制权还给主流程触发雷达倒计时！
	return int(activeRecordingCount)
}

// cleanupFailedTasksByPath 根据给定路径定位并清理缓存队列中残留的失败态记录
func cleanupFailedTasksByPath(targetPath string) {
	// 利用无锁哈希表快速遍历并核销失败任务
	liveTasks.Range(func(key, value interface{}) bool {
		task := value.(*Task)
		task.Mu.RLock()
		p := task.Path
		st := task.Status
		task.Mu.RUnlock()

		if p == targetPath && st == "failed" {
			liveTasks.Delete(key)
			if _, loaded := queueFail.LoadAndDelete(key); loaded {
				atomic.AddInt64(&queueFailCount, -1)
			}
		}
		return true
	})
}

// handleFile 负责调度单一目标文件的生命周期，包括名称净洗、秒传对比、上报排队及执行下层上传操作
func handleFile(path string) {
	if convert.IsArtifact(path) {
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		log.Printf("[FILE][ERR] 无法获取文件状态 %s: %v", path, err)
		return
	}

	// ✨ 核心修复二：作为防线托底，拒绝 0 字节切片进入推流流程，避免 NaN 以及远端 500
	if info.Size() == 0 {
		log.Printf("[FILE][SKIP] 拦截到 0 字节死文件，阻断上传并执行清理: %s", path)
		os.Remove(path)
		return
	}

	// TS→MP4：上传前无损封装；失败则回退直接传原 TS
	var originalTS string
	doConvert := appCfg().ConvertMP4
	if doConvert && convert.IsTS(path) {
		log.Printf("[CONVERT] 🎬 开始 TS→MP4 封装: %s (%.2f MB)", filepath.Base(path), float64(info.Size())/1024/1024)
		if mp4Path, cerr := convert.TSToMP4(path, recorder.FFmpegBin()); cerr == nil {
			originalTS = path
			path = mp4Path
			if ni, nerr := os.Stat(path); nerr == nil {
				info = ni
			} else {
				log.Printf("[CONVERT] 转换后无法读取 MP4，回退原 TS: %v", nerr)
				path = originalTS
				originalTS = ""
				_ = os.Remove(mp4Path)
			}
		} else {
			log.Printf("[CONVERT] ⚠️ 转换失败，将直接上传原 TS: %v", cerr)
		}
	}

	root := detectRoot(path)
	if root == "" {
		log.Println("[SKIP][NO_ROOT_MATCH] 找不到匹配的根目录:", path)
		return
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		log.Println("[PATH][REL][ERR]", err, path)
		return
	}

	name := naming.CleanFileName(filepath.Base(rel))
	remote := naming.BuildRemotePath(safeBaseDir, filepath.Dir(rel), filepath.Base(rel))

	// 检测秒传机制 (Hash)
	hash := hashstore.FileHash(path)
	if hash != "" && hashDB.Exists(hash) {
		log.Println("[SKIP][HASH] 文件已存在于记录中 (秒传触发):", path)

		taskID := fmt.Sprintf("task-%d", time.Now().UnixNano())

		newTask := &Task{
			ID:        taskID,
			Name:      name,
			Path:      path,
			Size:      info.Size(),
			Progress:  100,
			Speed:     0,
			Status:    "success(秒传)",
			CreatedAt: time.Now(),
			EndTime:   time.Now(),
		}
		liveTasks.Store(taskID, newTask)

		queueSuccess.Store(taskID, struct{}{})
		atomic.AddInt64(&queueSuccessCount, 1)

		addHistoryRecord(path, remote, info.Size(), "success(秒传)", 0, "")

		// 一旦秒传判定成功，不仅记录增加，更要减扣其身处待处理列表的份额
		if ds, exists := dirStatusStore.Get(root); exists {
			_ = ds
			ds.Mu.Lock()
			if ds.PendingFiles > 0 {
				ds.PendingFiles--
			}
			ds.UploadedFiles++
			ds.UploadedSize += info.Size()
			ds.TotalFiles = ds.PendingFiles + ds.UploadedFiles
			ds.Mu.Unlock()
		}
		markDirStatusDirty()

		broadcastWS("taskDone", map[string]interface{}{
			"id":     taskID,
			"status": "success",
			"size":   info.Size(),
		})

		cleanupFailedTasksByPath(path)

		if err := os.Remove(path); err != nil {
			log.Printf("[FILE][CLEAN][ERR] 秒传触发，移除本地文件失败 %s: %v", path, err)
		}
		if originalTS != "" {
			if err := os.Remove(originalTS); err != nil {
				log.Printf("[FILE][CLEAN][ERR] 秒传触发，移除原 TS 失败 %s: %v", originalTS, err)
			}
		}
		return
	}

	// 开始执行远端上传
	// upload() 内部持有文件句柄（defer f.Close()），函数返回后句柄已关闭，此时再 Remove 安全
	if upload(path, remote, info.Size()) {
		hashDB.Save(hash)
		if err := os.Remove(path); err != nil {
			log.Printf("[FILE][CLEAN][ERR] 移除本地文件失败 %s: %v", path, err)
		}
		if originalTS != "" {
			if err := os.Remove(originalTS); err != nil {
				log.Printf("[FILE][CLEAN][ERR] 移除原 TS 失败 %s: %v", originalTS, err)
			}
		}
		recordSuccess(remote, name, info.Size())

		// 真实物理上传完毕后扣除待处理余量
		if ds, exists := dirStatusStore.Get(root); exists {
			_ = ds
			ds.Mu.Lock()
			if ds.PendingFiles > 0 {
				ds.PendingFiles--
			}
			ds.UploadedFiles++
			ds.UploadedSize += info.Size()
			ds.TotalFiles = ds.PendingFiles + ds.UploadedFiles
			ds.Mu.Unlock()
		}
		markDirStatusDirty()
	}
}

// upload 建立与远端 API 的长连接将流数据打包为 HTTP PUT 方法传输，并承载错误重试及异常鉴权上报
func upload(local, remotePath string, size int64) bool {
	f, err := os.Open(local)
	if err != nil {
		log.Printf("[UPLOAD][ERR] 无法打开文件 %s: %v", local, err)
		return false
	}
	defer f.Close()

	taskID := fmt.Sprintf("task-%d", time.Now().UnixNano())
	startTime := time.Now()
	pr := NewProgressReaderWithID(filepath.Base(remotePath), f, size, taskID)

	newTask := &Task{
		ID:        taskID,
		Name:      filepath.Base(remotePath),
		Path:      local,
		Size:      size,
		Progress:  0,
		Speed:     0,
		Status:    "uploading",
		CreatedAt: startTime,
	}
	liveTasks.Store(taskID, newTask)

	queueUploading.Store(taskID, struct{}{})
	atomic.AddInt64(&queueUploadingCount, 1)

	broadcastWS("uploadProgress", map[string]interface{}{
		"id":        taskID,
		"filename":  filepath.Base(remotePath),
		"path":      local,
		"size":      size,
		"uploaded":  0,
		"speed":     0,
		"status":    "uploading",
		"startTime": startTime.UnixMilli(),
	})

	if remoteClient == nil {
		_cfgSnap := appCfg()
		remoteClient = remote.NewOpenListClient(_cfgSnap.RemoteServer, _cfgSnap.RemoteUser, _cfgSnap.RemotePass, httpCli)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	putRes, err := remoteClient.Put(ctx, remotePath, pr, size)

	if err != nil {
		log.Printf("[UPLOAD][HTTP][ERR] %s -> %v", filepath.Base(local), err)
		// 发送通知
		SendAlert("error", "上传连接失败", "无法连接远端服务器: "+err.Error())

		if val, exists := liveTasks.Load(taskID); exists {
			task := val.(*Task)
			task.Mu.Lock()
			task.Status = "failed"
			task.Error = err.Error()
			task.EndTime = time.Now()
			task.Mu.Unlock()
		}

		if _, loaded := queueUploading.LoadAndDelete(taskID); loaded {
			atomic.AddInt64(&queueUploadingCount, -1)
		}
		queueFail.Store(taskID, struct{}{})
		atomic.AddInt64(&queueFailCount, 1)

		addHistoryRecord(local, remotePath, size, "failed", time.Since(startTime).Seconds(), err.Error())

		broadcastWS("taskDone", map[string]interface{}{
			"id":     taskID,
			"status": "fail",
			"error":  err.Error(),
		})

		// 判断网络不通或宕机级别的熔断
		fails := atomic.AddInt32(&consecutiveFailures, 1)
		if fails >= 30 {
			pauseSystemOnFailure(fmt.Sprintf("已连续 %d 次无法连接到远端服务器，网络可能断开或远端已宕机。", fails))
		}

		return false
	}

	// putRes.Code == 200 表示远端接受
	if putRes.OK() {
		if val, exists := liveTasks.Load(taskID); exists {
			task := val.(*Task)
			task.Mu.Lock()
			task.Status = "success"
			task.Progress = 100
			task.EndTime = time.Now()
			task.Mu.Unlock()
		}

		if _, loaded := queueUploading.LoadAndDelete(taskID); loaded {
			atomic.AddInt64(&queueUploadingCount, -1)
		}
		queueSuccess.Store(taskID, struct{}{})
		atomic.AddInt64(&queueSuccessCount, 1)

		addHistoryRecord(local, remotePath, size, "success", time.Since(startTime).Seconds(), "")

		broadcastWS("taskDone", map[string]interface{}{
			"id":       taskID,
			"status":   "success",
			"progress": 100,
			"size":     size,
		})

		cleanupFailedTasksByPath(local)

		// 一旦成功立刻清零失败熔断计数器，证明服务器健康
		atomic.StoreInt32(&consecutiveFailures, 0)
		return true

	} else {
		// 截获服务器的真实拦截明细输出到日志
		log.Printf("[UPLOAD][REMOTE][ERR] 远端服务器拒绝或异常，状态码: %d 详细报错: %s 文件: %s", putRes.Code, putRes.Message, filepath.Base(local))
		errMsg := fmt.Sprintf("远端拒绝 (Code: %d, 报错: %s)", putRes.Code, putRes.Message)

		// 将服务端真实错误暴露给用户的气泡系统
		SendAlert("error", "上传遭拒绝", fmt.Sprintf("文件: %s\n状态码: %d\n详细报错: %s", filepath.Base(local), putRes.Code, putRes.Message))

		if val, exists := liveTasks.Load(taskID); exists {
			task := val.(*Task)
			task.Mu.Lock()
			task.Status = "failed"
			task.Error = errMsg
			task.EndTime = time.Now()
			task.Mu.Unlock()
		}

		if _, loaded := queueUploading.LoadAndDelete(taskID); loaded {
			atomic.AddInt64(&queueUploadingCount, -1)
		}
		queueFail.Store(taskID, struct{}{})
		atomic.AddInt64(&queueFailCount, 1)

		addHistoryRecord(local, remotePath, size, "failed", time.Since(startTime).Seconds(), errMsg)

		broadcastWS("taskDone", map[string]interface{}{
			"id":     taskID,
			"status": "fail",
			"error":  errMsg,
		})

		// 判断 Token 失效或服务器磁盘已满的逻辑熔断
		fails := atomic.AddInt32(&consecutiveFailures, 1)
		if fails >= 30 {
			pauseSystemOnFailure(fmt.Sprintf("连续 %d 个文件被远端服务器拒绝接收 (状态码: %d，报错: %s)。", fails, putRes.Code, putRes.Message))
		}

		return false
	}
}

// addHistoryRecord 将最终确定状态的上传操作以标准格式记录至系统的长驻内存历史队列中
func addHistoryRecord(local, remote string, size int64, status string, duration float64, errorMsg string) {
	historyStore.Add(HistoryRecord{
		UploadTime: time.Now().Format("2006-01-02 15:04:05"),
		Name:       filepath.Base(remote),
		Size:       size,
		LocalPath:  local,
		Remote:     remote,
		Status:     status,
		Duration:   int(duration),
		ErrorMsg:   errorMsg,
	})
}

// ProgressReader 带速率限制和进度通知的自定义文件读取数据结构
type ProgressReader struct {
	name        string
	r           io.Reader
	total       int64
	read        int64
	last        time.Time
	start       time.Time
	taskID      string
	lastLogProg int
}

// NewProgressReaderWithID 封装系统的流处理组件，绑定文件与进程
func NewProgressReaderWithID(name string, r io.Reader, total int64, taskID string) *ProgressReader {
	return &ProgressReader{
		name:        name,
		r:           r,
		total:       total,
		start:       time.Now(),
		taskID:      taskID,
		lastLogProg: -1,
	}
}

// Read 实现核心 io.Reader 接口并劫持每一次小片数据读写用于上报进度与速率休眠拦截
// 性能提升：移除全局 liveTasksMu 的锁定，转为提取单任务内的细粒度 RWMutex
func (p *ProgressReader) Read(b []byte) (int, error) {
	startRead := time.Now()
	n, err := p.r.Read(b)
	p.read += int64(n)

	// 带宽限速逻辑
	rateMB := currentRate()
	if rateMB > 0 {
		rate := int64(rateMB) * 1024 * 1024
		expect := time.Duration(int64(time.Second) * int64(n) / rate)
		if d := time.Since(startRead); d < expect {
			time.Sleep(expect - d)
		}
	}

	// 每半秒更新一次进度并同步前端
	if time.Since(p.last) > 500*time.Millisecond {
		p.last = time.Now()

		// ✨ 核心修复三：彻底防范浮点数被 0 除所引发的 NaN (Not a Number) 以及随之而来的界面崩溃
		var progress int
		if p.total > 0 {
			progress = int(float64(p.read) * 100 / float64(p.total))
		} else {
			progress = 100 // 如果文件大小为 0，直判完成
		}

		elapsed := time.Since(p.start).Seconds()
		var speed int64
		if elapsed > 0.1 {
			speed = int64(float64(p.read) / elapsed)
		}

		step := progress / 10
		if step > p.lastLogProg {
			p.lastLogProg = step
			log.Printf("[UPLOAD][PROGRESS] 文件: %s -> 进度: %d%%", p.name, step*10)
		}

		// 精准定位任务进行原子级属性覆写，防止锁冲突
		if val, exists := liveTasks.Load(p.taskID); exists {
			task := val.(*Task)

			task.Mu.Lock()
			task.Progress = progress
			task.Speed = speed

			// 数据拷贝提取出安全区域，避开下流广播阻塞
			wsName := task.Name
			wsPath := task.Path
			wsSize := task.Size
			wsStatus := task.Status
			wsStartTime := task.CreatedAt.UnixMilli()
			task.Mu.Unlock()

			broadcastWS("uploadProgress", map[string]interface{}{
				"id":        p.taskID,
				"filename":  wsName,
				"path":      wsPath,
				"size":      wsSize,
				"uploaded":  p.read,
				"speed":     speed,
				"status":    wsStatus,
				"startTime": wsStartTime,
			})
		}
	}
	return n, err
}

// currentRate 根据预设在应用配置里的时间节点自动切换与判定所处小时数的限流值
func currentRate() int {
	_cfgSnap := appCfg()
	return ratelimit.Select(_cfgSnap.DayRate, _cfgSnap.NightRate, time.Now())
}

// updateStatsIncrementally 由 successStore 内部维护，保留空壳避免外部误调
func updateStatsIncrementally(rec UploadRecord) {
	successStore.Add(rec) // Add 会 applyStats；此函数仅为兼容旧调用点
}

// recordSuccess 专门记录最终通过网络被写入目标端存储系统的文件日志以供统计
func recordSuccess(remote, name string, size int64) {
	successStore.Add(UploadRecord{
		Time:     time.Now(),
		Streamer: naming.DetectStreamer(remote),
		Name:     name,
		Remote:   remote,
		Size:     size,
	})
}

// flushSuccessLog 将内存中的成功记录全量序列化后以原子替换方式写入磁盘
func flushSuccessLog() {
	successStore.Flush()
}

// successLogPersistLoop 后台守护协程：每 15 秒检查脏标记，批量合并落盘成功记录
func successLogPersistLoop() {
	stop := make(chan struct{})
	if appCtx != nil {
		go func() { <-appCtx.Done(); close(stop) }()
	}
	successStore.PersistLoop(15*time.Second, stop)
}

// reportLoop 长驻于后台的死循环机制，依靠时间计算判断向指定电子信箱推送数据的恰当时间
func reportLoop() {
	intervalMinutes := appCfg().EmailInterval
	// 1. 预先算出下一次应该发邮件的绝对时间点 (比如：现在是 10:00，间隔 6 小时，那 next 应该是 16:00)
	nextReportTime := time.Now().Add(time.Duration(intervalMinutes) * time.Minute)

	for {
		// 2. 核心：计算距离 16:00 还差多久？(比如还剩 5小时59分59秒)
		sleepDuration := time.Until(nextReportTime)

		if sleepDuration <= 0 {
			sendReport()

			intervalMinutes = appCfg().EmailInterval

			nextReportTime = time.Now().Add(time.Duration(intervalMinutes) * time.Minute)
			continue
		}

		select {
		case <-time.After(sleepDuration):
		case <-triggerReportCh:
			log.Printf("[SYSTEM] 📧 配置发生变更，但这不会打断原有的邮件倒计时，邮件仍将在 %v 后发送", time.Until(nextReportTime).Truncate(time.Second))
		}
	}
}

// sendReport 获取固定周期跨度内的所有成功提交资料，编排为精致富文本并交给邮件 SMTP 系统
func sendReport() {
	list := successStore.Snapshot()
	if len(list) == 0 {
		return
	}

	repMinutes := appCfg().EmailInterval

	// ⭐ 核心过滤：计算周期截止时间，只发送最近这个周期内（如6小时内）的记录
	cutoffTime := time.Now().Add(-time.Duration(repMinutes) * time.Minute)
	var recentList []UploadRecord
	for _, r := range list {
		if r.Time.After(cutoffTime) {
			recentList = append(recentList, r)
		}
	}

	if len(recentList) == 0 {
		log.Printf("[REPORT] 📦 过去 %d 分钟内无新上传成功记录，跳过本次邮件推送", repMinutes)
		return
	}

	group := map[string][]UploadRecord{}
	var totalBytes int64

	for _, r := range recentList {
		group[r.Streamer] = append(group[r.Streamer], r)
		totalBytes += r.Size
	}

	totalMB := float64(totalBytes) / 1024 / 1024
	now := time.Now().Format("2006-01-02 15:04")

	var html strings.Builder

	html.WriteString(`
<table width="100%" cellpadding="0" cellspacing="0" style="background:#f4f6f8;padding:24px;">
<tr><td align="center"><table width="760" cellpadding="0" cellspacing="0" style="background:#ffffff;border-radius:12px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Arial;">
`)

	html.WriteString(fmt.Sprintf(`
<tr><td style="padding:24px;border-bottom:1px solid #e5e7eb;">
<h2 style="margin:0;font-size:20px;color:#111827;">📦 上传成功报告</h2>
<p style="margin:6px 0 0;font-size:13px;color:#6b7280;">统计周期 %d 分钟 ｜ 生成时间 %s</p>
</td></tr>
`, repMinutes, now))

	html.WriteString(fmt.Sprintf(`
<tr><td style="padding:20px;">
<table width="100%%" cellpadding="12" cellspacing="0" style="background:#f8fafc;border-radius:10px;">
<tr>
<td><div style="font-size:12px;color:#6b7280;">新增文件数</div><div style="font-size:22px;color:#111827;"><b>%d</b></div></td>
<td><div style="font-size:12px;color:#6b7280;">消耗流量</div><div style="font-size:22px;color:#111827;"><b>%.2f MB</b></div></td>
<td><div style="font-size:12px;color:#6b7280;">涉及主播数</div><div style="font-size:22px;color:#111827;"><b>%d</b></div></td>
</tr>
</table></td></tr>
`, len(recentList), totalMB, len(group)))

	for streamer, files := range group {
		html.WriteString(fmt.Sprintf(`<tr><td style="padding:20px 20px 8px 20px;"><h3 style="margin:0;font-size:15px;color:#2563eb;">🎬 %s</h3></td></tr>
<tr><td style="padding:0 20px 20px 20px;"><table width="100%%" cellpadding="8" cellspacing="0" style="border-collapse:collapse;font-size:13px;">
<tr style="background:#f1f5f9;color:#374151;"><th align="left">时间</th><th align="left">文件名</th><th align="right">大小</th><th align="left">存储路径</th></tr>
`, streamer))

		for _, f := range files {
			html.WriteString(fmt.Sprintf(`<tr style="border-bottom:1px solid #e5e7eb;"><td style="color:#6b7280;">%s</td><td style="color:#111827;font-weight:500;">%s</td><td align="right">%.2f MB</td><td style="font-family:ui-monospace,Menlo,monospace;word-break:break-all;color:#374151;">%s</td></tr>`,
				f.Time.Format("01-02 15:04"), f.Name, float64(f.Size)/1024/1024, f.Remote,
			))
		}
		html.WriteString(`</table></td></tr>`)
	}

	html.WriteString(`<tr><td style="padding:16px 24px;border-top:1px dashed #e5e7eb;font-size:12px;color:#9ca3af;">本邮件由自动上传系统生成，请勿回复</td></tr></table></td></tr></table>`)

	log.Printf("[REPORT] 📤 正在发送本周期统计邮件，包含 %d 个文件记录", len(recentList))
	sendQQMail("📦 上传成功报告", html.String())
}

// sendQQMail 利用 SMTP 将带有授权码的主体信息发给外网腾讯服务器
func sendQQMail(subject, body string) {
	_cfgSnap := appCfg()
	mailFrom := _cfgSnap.MailFrom
	mailAuthCode := _cfgSnap.MailAuthCode
	mailTo := _cfgSnap.MailTo

	// 增加对邮箱配置缺失的安全判断
	if mailFrom == "" || mailAuthCode == "" || mailTo == "" {
		log.Printf("[REPORT][MAIL] ⚠️ 邮件参数未配置或不完整，自动跳过邮件发送")
		return
	}

	msg := []byte(
		"To: " + mailTo + "\r\n" +
			"From: " + mailFrom + "\r\n" +
			"Subject: " + subject + "\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/html; charset=UTF-8\r\n\r\n" +
			body,
	)
	auth := smtp.PlainAuth("", mailFrom, mailAuthCode, "smtp.qq.com")
	err := smtp.SendMail("smtp.qq.com:587", auth, mailFrom, []string{mailTo}, msg)
	if err != nil {
		log.Printf("[REPORT][MAIL][ERR] 邮件发送失败: %v", err)
	}
}

// login 携带后台配置内配置账号及加密体密码向存储总机做 HTTP POST 请求并提取令牌回传
func login() error {
	_cfgSnap := appCfg()
	if remoteClient == nil {
		remoteClient = remote.NewOpenListClient(_cfgSnap.RemoteServer, _cfgSnap.RemoteUser, _cfgSnap.RemotePass, httpCli)
	} else {
		remoteClient.SetCredentials(_cfgSnap.RemoteServer, _cfgSnap.RemoteUser, _cfgSnap.RemotePass)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return remoteClient.Login(ctx)
}

// detectRoot 提供针对底层物理目录映射的反推机制从而确定文件属主节点
func detectRoot(path string) string {
	return naming.DetectRoot(path, appCfg().Dirs)
}

// addLog 作为业务和展示系统隔离的桥梁，负责筛选后将指定等级事件装箱并经加密投递到浏览器
func addLog(level, message, errorMsg string) {
	if !appCfg().EnableLogs {
		return
	}
	appLogs.Add(level, message, errorMsg)
}

// getActiveStreamers 通过探测各目录内是否存在时间较新的文件，推断当前正在处于写入(活跃录制)状态的主播名单，并与上次比对触发微信开播/下播通知
func getActiveStreamers() []string {
	configuredDirs := appCfg().Dirs

	activeMap := make(map[string]bool)

	for _, dir := range configuredDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}

		// 优化：使用 WalkDir 替代 Walk，避免每个文件多一次 Lstat 系统调用
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if time.Since(info.ModTime()) < 3*time.Minute {
				rel, err := filepath.Rel(dir, path)
				if err == nil {
					parts := strings.Split(filepath.ToSlash(rel), "/")
					if len(parts) >= 2 {
						streamerName := parts[len(parts)-2]
						activeMap[streamerName] = true
					} else if len(parts) == 1 {
						name := strings.Split(parts[0], "_")[0]
						activeMap[name] = true
					}
				}
			}
			return nil
		})
	}

	var result []string
	for k := range activeMap {
		result = append(result, k)
	}

	// ✨ 核心防重排斥机制：提取当前处于活跃状态的内置引擎任务名单
	builtinNames := make(map[string]bool)
	for _, t := range GetBuiltinRecorderTasks() {
		if isBuiltinLiveStatus(t.Status) { // 只排斥确实在录制中的任务，防止干扰
			// 将特殊字符去除，匹配目录名可能发生的清洗化逻辑
			safeName := t.AnchorName
			invalidChars := []string{"\\", "/", ":", "*", "?", "\"", "<", ">", "|", "\r", "\n", "\t", "　"}
			for _, char := range invalidChars {
				safeName = strings.ReplaceAll(safeName, char, "")
			}
			safeName = strings.TrimSpace(safeName)
			safeName = strings.Trim(safeName, " ._-")
			if safeName == "" {
				safeName = t.RoomID
			}
			builtinNames[safeName] = true
			builtinNames[t.AnchorName] = true // 原名也存一份备用比对
		}
	}

	// 提取差异，推送微信开播和下播通知
	lastActiveMapMu.Lock()

	// 检测新开播
	for streamer := range activeMap {
		if !lastActiveMap[streamer] {
			// ✨ 【防碰撞】：如果这个主播已经在内置引擎的录制名单里，雷达保持静默
			if !builtinNames[streamer] {
				sendWeChatNotify("开播通知", fmt.Sprintf("检测到外部录制引擎中主播 [%s] 的文件夹有新数据写入，判断为开始录制！", streamer))
			}
		}
	}

	// 检测已下播
	for streamer := range lastActiveMap {
		if !activeMap[streamer] {
			// ✨ 【防碰撞】：同理，如果是内置引擎负责录制的，外部雷达不发下播通知
			if !builtinNames[streamer] {
				sendWeChatNotify("下播通知", fmt.Sprintf("检测到外部录制引擎中主播 [%s] 的文件夹已停止数据写入，判断为结束录制！", streamer))
			}
		}
	}

	// 更新缓存状态供下轮对比
	lastActiveMap = make(map[string]bool)
	for k, v := range activeMap {
		lastActiveMap[k] = v
	}
	lastActiveMapMu.Unlock()

	return result
}
