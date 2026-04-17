package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

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
	dashboardPassword = "admin"

	// 修复 Bug 7：将远端服务器 Token 与 Dashboard 登录 Token 分开
	// token 原先被 login() 和 handleLogin() 同时写入，两者语义不同会互相覆盖
	token            string // 远端 Alist 服务器的 Bearer Token（用于文件上传鉴权）
	tokenMu          sync.Mutex
	dashboardToken   string // Dashboard 本地会话 Token（用于前端登录验证）
	dashboardTokenMu sync.Mutex

	// 优化点：为 httpCli 配置连接池，复用空闲连接，极大减少频繁新建 TCP 请求带来的内存和 CPU 消耗
	httpCli = &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			MaxIdleConns:        100,              // 最大空闲连接数
			MaxIdleConnsPerHost: 20,               // 每个 host 最大空闲连接
			IdleConnTimeout:     90 * time.Second, // 空闲连接保活时间
		},
	}

	hashFile = "uploaded_hash.db" // 已经上传的文件哈希记录（秒传用）

	queueCount   int64 // 等待队列数量
	activeWorker int64 // 当前活跃的 Worker 数量

	// 暴露给前端的动态扫描周期与倒计时时间戳
	currentDynamicIntervalGlobal int64
	nextScanTimeGlobal           int64

	// 熔断保护机制：连续失败次数计数器
	consecutiveFailures int32
)

// 动态应用配置
var (
	appConfig   Config
	appConfigMu sync.RWMutex
)

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

	history   = make([]*HistoryRecord, 0)
	historyMu sync.RWMutex

	// 优化点：专门用于保护 successRecords 内存数据和 json 文件并发读写的互斥锁
	successLogMu sync.Mutex
	// 优化点：在内存中常驻成功记录，消除前端轮询及 Worker 追加数据时对磁盘和 GC 产生的巨大压力
	successRecords []UploadRecord

	// 优化点：将图表统计从每次 O(N) 全量遍历 50W 条数据优化为 O(1) 增量聚合维护
	trendStats sync.Map // key: date (MM-DD), value: *TrendPoint
	rankStats  sync.Map // key: streamer string, value: *atomic.Int64

	// 哈希表优化：移除全局锁，换用高并发 sync.Map 承载海量哈希验证
	hashCache  sync.Map
	hashFileMu sync.Mutex // 专为哈希值追记落盘提供的 I/O 锁

	// 高性能合并落盘脏标记，采用无锁原子操作防止并发瓶颈
	dirStatusDirty int32

	// 修复 Bug 5：成功记录的合并落盘脏标记，避免每次上传完都全量重写 50 万条 JSON
	successLogDirty int32

	// 【彻底解耦扫描与上传】新增的全局异步 Worker 池核心组件
	globalTaskCh  = make(chan string, 100000) // 十万级超大缓冲，保证扫描引擎永不阻塞
	enqueuedFiles sync.Map                    // 任务防重防漏护盾
	workerCount   int32                       // 当前实际运行的 Worker 数量

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

// HistoryRecord 定义了长期沉淀入库的历史文件上传记录
type HistoryRecord struct {
	UploadTime string `json:"uploadTime"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	LocalPath  string `json:"localPath"`
	Remote     string `json:"remote"`
	Status     string `json:"status"`
	Duration   int    `json:"duration"`
	ErrorMsg   string `json:"errorMsg"`
}

// 常量定义，包含安全的前置远端目录与本地磁盘化缓存文件
const (
	safeBaseDir    = "/_safe_uploads"      // 远端安全目录
	successLogFile = "upload_success.json" // 本地成功日志，用于图表统计
	dirStatusFile  = "dir_status.json"     // 本地目录状态统计持久化文件
)

// logInterceptor 拦截系统内部各类标准打印流，并实现同步推送到前端 WebSocket 展示
type logInterceptor struct {
	original io.Writer
}

// Write 实现接口拦截标准日志，并组装投递给面板日志监控系统
func (l *logInterceptor) Write(p []byte) (n int, err error) {
	n, err = l.original.Write(p)
	msg := strings.TrimSpace(string(p))

	parts := strings.SplitN(msg, " ", 3)
	if len(parts) >= 3 && strings.Contains(parts[0], "/") && strings.Contains(parts[1], ":") {
		msg = parts[2]
	}

	level := "info"
	lowerMsg := strings.ToLower(msg)
	if strings.Contains(lowerMsg, "[err]") || strings.Contains(lowerMsg, "error") || strings.Contains(lowerMsg, "fail") {
		level = "error"
	} else if strings.Contains(lowerMsg, "[warn]") || strings.Contains(lowerMsg, "warning") {
		level = "warn"
	}

	addLog(level, msg, "")
	return n, err
}

// saveConfigToFile 将当前内存中的应用运行时设置持久化落盘至 config.json 文件
// 注意：调用方必须在调用前自行加锁读取副本，此函数不再内部加锁，避免重入死锁
func saveConfigToFile() {
	appConfigMu.RLock()
	cfgCopy := appConfig
	appConfigMu.RUnlock()

	// 使用带缩进的 json 格式，便于用户直接查看或修改文件内容
	data, err := json.MarshalIndent(cfgCopy, "", "  ")
	if err == nil {
		os.WriteFile("config.json", data, 0644)
	} else {
		log.Printf("[CONFIG][ERR] 无法保存配置文件 config.json: %v", err)
	}
}

// loadDirStatuses 启动时从本地磁盘恢复各个监控目录的累计上传数据，防止重启清空
func loadDirStatuses() {
	data, err := os.ReadFile(dirStatusFile)
	if err == nil {
		var tempMap map[string]*DirStatus
		if err := json.Unmarshal(data, &tempMap); err == nil {
			for k, v := range tempMap {
				// 转入高并发 sync.Map 容器
				dirStatuses.Store(k, v)
			}
		}
	}
}

// markDirStatusDirty 标记内存中的目录状态已被更改，触发后台异步落盘
func markDirStatusDirty() {
	atomic.StoreInt32(&dirStatusDirty, 1)
}

// flushDirStatuses 提取内存中的最新目录状态并执行物理层面的覆盖落盘
func flushDirStatuses() {
	tempMap := make(map[string]*DirStatus)
	dirStatuses.Range(func(key, value interface{}) bool {
		ds := value.(*DirStatus)
		ds.Mu.RLock()
		// 复制出状态快照用于落盘
		tempMap[key.(string)] = &DirStatus{
			Path:          ds.Path,
			TotalFiles:    ds.TotalFiles,
			UploadedFiles: ds.UploadedFiles,
			PendingFiles:  ds.PendingFiles,
			TotalSize:     ds.TotalSize,
			UploadedSize:  ds.UploadedSize,
			LastScanTime:  ds.LastScanTime,
		}
		ds.Mu.RUnlock()
		return true
	})

	data, err := json.MarshalIndent(tempMap, "", "  ")
	if err == nil {
		targetDir := filepath.Dir(dirStatusFile)
		f, err := os.CreateTemp(targetDir, "dir_status_*.json")
		if err != nil {
			return
		}
		tmpName := f.Name()
		f.Write(data)
		f.Close()
		os.Rename(tmpName, dirStatusFile) // 原子替换，避免写入中途宕机导致损坏
	}
}

// dirStatusPersistLoop 驻留于后台的合并落盘守护协程，按固定心跳检查并持久化状态
func dirStatusPersistLoop() {
	ticker := time.NewTicker(5 * time.Second) // 5 秒合并写一次，化解 I/O 阻塞瓶颈
	for range ticker.C {
		// CAS 原子操作：如果当前为脏数据(1)，则置为干净(0)并执行实际的写盘操作
		if atomic.CompareAndSwapInt32(&dirStatusDirty, 1, 0) {
			flushDirStatuses()
		}
	}
}

// manageWorkers 全局常驻的动态 Worker 线程池调度器
func manageWorkers() {
	for {
		appConfigMu.RLock()
		desired := appConfig.Workers
		appConfigMu.RUnlock()

		current := atomic.LoadInt32(&workerCount)
		for current < int32(desired) {
			// 先原子递增并拿到本次新建 Worker 的唯一 ID，再启动 goroutine
			// 修复 Bug：原先 int(current)+1 依赖循环变量快照，current++ 后 workerID 会跳号
			newID := int(atomic.AddInt32(&workerCount, 1))

			go func(workerID int) {
				for path := range globalTaskCh {
					runningMu.RLock()
					isRunning := running
					runningMu.RUnlock()

					// 若系统处于暂停状态，将任务丢弃并抹除内存标记，交由下一次扫描重新拾取
					if !isRunning {
						enqueuedFiles.Delete(path)
						atomic.AddInt64(&queueCount, -1)
						continue
					}

					atomic.AddInt64(&queueCount, -1)
					atomic.AddInt64(&activeWorker, 1)

					start := time.Now()
					handleFile(path) // 实际的耗时上传操作在这里发生

					log.Printf("[UPLOAD][DONE][W%d] 文件:%s 耗时:%s", workerID, filepath.Base(path), time.Since(start).Truncate(time.Millisecond))

					// 上传结束（无论成功或失败），必须清除防重标记，允许未来复检
					enqueuedFiles.Delete(path)
					atomic.AddInt64(&activeWorker, -1)
				}
				atomic.AddInt32(&workerCount, -1)
			}(newID)

			current++
		}
		// 每 2 秒巡检一次 Worker 数量，确保存活
		time.Sleep(2 * time.Second)
	}
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
	flag.StringVar(&server, "server", "https://wustwust.cn:8081", "服务器")
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
	log.SetOutput(&logInterceptor{original: os.Stdout})

	// 优化点：启动时读取一次本地上传成功记录缓存入内存，并依此构建高性能的增量聚合面板所需数据源
	if data, err := os.ReadFile(successLogFile); err == nil {
		json.Unmarshal(data, &successRecords)
		for _, rec := range successRecords {
			updateStatsIncrementally(rec) // 将已存的数据恢复到高并发增量池
		}
	} else {
		successRecords = make([]UploadRecord, 0)
	}

	// 启动时一次性加载哈希库到内存，后续 hashExists/saveHash 全走内存
	loadHashCache()

	// 启动时加载硬盘目录累积统计数据
	loadDirStatuses()

	// 初始化系统参数：优先从 config.json 读取；如果文件不存在，则使用启动参数进行填充
	appConfigMu.Lock()
	data, err := os.ReadFile("config.json")
	if err == nil {
		// 成功读取到文件则反序列化覆盖
		json.Unmarshal(data, &appConfig)
	} else {
		// 文件不存在，写入默认及命令行参数
		appConfig.ScanInterval = scanningInterval
		appConfig.Workers = workers
		appConfig.DayRate = dayRateMB
		appConfig.NightRate = nightRateMB
		appConfig.EmailInterval = reportMinutes
		appConfig.Dirs = strings.Split(dirs, ",")
		appConfig.EnableLogs = true
		appConfig.RemoteServer = server
		appConfig.RemoteUser = "admin"
		appConfig.RemotePass = "LilKmxNF"
		appConfig.LiveConfigPath = liveConfigPath
		appConfig.RecorderContainer = recorderContainer
		appConfig.RecorderConfigPath = recorderConfigPath
		appConfig.EnableEncryption = true // 默认开启通信加密

		// 写入默认的邮件配置参数（在此预设原有的硬编码值）
		appConfig.MailFrom = "your_email@qq.com"
		appConfig.MailAuthCode = "your_auth_code"
		appConfig.MailTo = "receive_email@qq.com"
	}
	appConfigMu.Unlock()

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

	appConfigMu.RLock()
	baseInterval := appConfig.ScanInterval
	appConfigMu.RUnlock()
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
			if t.Status == "录制中" {
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
				appConfigMu.RLock()
				baseInterval = appConfig.ScanInterval
				appConfigMu.RUnlock()

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

				appConfigMu.RLock()
				baseInterval = appConfig.ScanInterval
				appConfigMu.RUnlock()

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
		appConfigMu.RLock()
		currentWorkers := appConfig.Workers
		appConfigMu.RUnlock()

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

	appConfigMu.RLock()
	currentWorkers := appConfig.Workers
	currentDirs := make([]string, len(appConfig.Dirs))
	copy(currentDirs, appConfig.Dirs)
	appConfigMu.RUnlock()

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
		if val, exists := dirStatuses.Load(root); exists {
			ds := val.(*DirStatus)
			ds.Mu.Lock()
			ds.PendingFiles = 0
			ds.TotalSize = 0
			ds.LastScanTime = time.Now().UnixMilli()
			ds.Mu.Unlock()
		} else {
			dirStatuses.Store(root, &DirStatus{
				Path:         root,
				LastScanTime: time.Now().UnixMilli(),
			})
		}
	}

	var newlyAddedFiles int32 = 0
	var activeRecordingCount int32 = 0
	var scanWg sync.WaitGroup

	// 用于大文件排队调度的任务缓冲池与互斥锁
	var collectedTasks []fileTask
	var collectedMu sync.Mutex

	// 并发遍历多个目标根目录以加速数据检索和任务投递
	for _, root := range currentDirs {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "." || root == "" {
			continue
		}

		scanWg.Add(1)
		go func(scanRoot string) {
			defer scanWg.Done()

			err := filepath.WalkDir(scanRoot, func(path string, d os.DirEntry, err error) error {
				runningMu.RLock()
				isRunning := running
				runningMu.RUnlock()

				if !isRunning {
					return fmt.Errorf("scan canceled by user")
				}
				if err != nil {
					log.Printf("[SCAN][ERR] 访问路径出错 %s: %v", path, err)
					return nil
				}
				if d.IsDir() {
					return nil
				}

				info, err := d.Info()
				if err != nil {
					return nil
				}

				if time.Since(info.ModTime()) < 2*time.Minute {
					atomic.AddInt32(&activeRecordingCount, 1)
					return nil
				}

				// ✨ 核心修复一：在扫描阶段直接拦截并静默销毁 0 字节废弃文件，拔除根源污染
				if info.Size() == 0 {
					log.Printf("[SCAN][CLEAN] 检测到遗留的 0 字节无效切片，已自动物理删除: %s", path)
					os.Remove(path)
					return nil
				}

				// 每找到一个合法的磁盘文件，视为当前待处理文件
				if val, exists := dirStatuses.Load(scanRoot); exists {
					ds := val.(*DirStatus)
					ds.Mu.Lock()
					ds.PendingFiles++
					ds.TotalSize += info.Size()
					ds.Mu.Unlock()
				}

				// 利用内存锁阻挡已在排队或上一轮尚未处理完成的文件，避免冗余收集
				if _, loaded := enqueuedFiles.Load(path); !loaded {
					collectedMu.Lock()
					collectedTasks = append(collectedTasks, fileTask{path: path, size: info.Size()})
					collectedMu.Unlock()
				}
				return nil
			})

			if err != nil && err.Error() != "scan canceled by user" {
				SendAlert("warning", "目录扫描异常", "无法访问部分路径: "+err.Error())
				addLog("error", "文件遍历失败", err.Error())
			}
		}(root)
	}

	// 阻塞挂起直至所有平行目录的检索分析下发动作完成
	scanWg.Wait()

	// 智能调度核心-修改版：解决大文件全部扎堆导致通道严重拥堵的问题
	// 1. 依然先按大小降序排列
	sort.Slice(collectedTasks, func(i, j int) bool {
		return collectedTasks[i].size > collectedTasks[j].size
	})

	// 2. 双指针交替提取：按 [最大, 最小, 第二大, 第二小...] 的顺序重组序列，实现负载穿插均衡化
	var mixedTasks []fileTask
	left, right := 0, len(collectedTasks)-1
	for left <= right {
		mixedTasks = append(mixedTasks, collectedTasks[left])
		left++
		if left <= right {
			mixedTasks = append(mixedTasks, collectedTasks[right])
			right--
		}
	}
	collectedTasks = mixedTasks // 覆写回主切片供下发推流

	// 将排序及混合完成后的任务真正抛入全局异步大容量缓冲通道中
	for _, t := range collectedTasks {
		if _, loaded := enqueuedFiles.LoadOrStore(t.path, true); !loaded {
			atomic.AddInt64(&queueCount, 1)
			atomic.AddInt32(&newlyAddedFiles, 1)

			select {
			case globalTaskCh <- t.path:
			default:
				enqueuedFiles.Delete(t.path)
				atomic.AddInt64(&queueCount, -1)
				atomic.AddInt32(&newlyAddedFiles, -1)
			}
		}
	}

	// 在扫描终点，将累加出的准确待处理量 + 内存记录的成功上传量 = 当下的总文件数
	dirStatuses.Range(func(key, value interface{}) bool {
		ds := value.(*DirStatus)
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

	dir := filepath.Dir(rel)

	// 过滤文件夹路径中的非法前缀，防止生成类似 `_safe_uploads/-可奈` 这种导致 500 的非法云端目录
	dirParts := strings.Split(filepath.ToSlash(dir), "/")
	for i, part := range dirParts {
		cleanPart := strings.TrimLeft(part, "-_.")
		if cleanPart == "" {
			cleanPart = "streamer_dir"
		}
		dirParts[i] = cleanPart
	}
	cleanDir := strings.Join(dirParts, "/")

	name := cleanFileName(filepath.Base(rel))
	remote := filepath.ToSlash(filepath.Join(safeBaseDir, cleanDir, name))

	// 检测秒传机制 (Hash)
	hash := fileHash(path)
	if hash != "" && hashExists(hash) {
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
		if val, exists := dirStatuses.Load(root); exists {
			ds := val.(*DirStatus)
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
		return
	}

	// 开始执行远端上传
	// upload() 内部持有文件句柄（defer f.Close()），函数返回后句柄已关闭，此时再 Remove 安全
	if upload(path, remote, info.Size()) {
		saveHash(hash)
		if err := os.Remove(path); err != nil {
			log.Printf("[FILE][CLEAN][ERR] 移除本地文件失败 %s: %v", path, err)
		}
		recordSuccess(remote, name, info.Size())

		// 真实物理上传完毕后扣除待处理余量
		if val, exists := dirStatuses.Load(root); exists {
			ds := val.(*DirStatus)
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
func upload(local, remote string, size int64) bool {
	f, err := os.Open(local)
	if err != nil {
		log.Printf("[UPLOAD][ERR] 无法打开文件 %s: %v", local, err)
		return false
	}
	defer f.Close()

	taskID := fmt.Sprintf("task-%d", time.Now().UnixNano())
	startTime := time.Now()
	pr := NewProgressReaderWithID(filepath.Base(remote), f, size, taskID)

	newTask := &Task{
		ID:        taskID,
		Name:      filepath.Base(remote),
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
		"filename":  filepath.Base(remote),
		"path":      local,
		"size":      size,
		"uploaded":  0,
		"speed":     0,
		"status":    "uploading",
		"startTime": startTime.UnixMilli(),
	})

	appConfigMu.RLock()
	targetServer := appConfig.RemoteServer
	appConfigMu.RUnlock()

	req, _ := http.NewRequest("PUT", targetServer+"/api/fs/put", pr)
	req.ContentLength = size
	req.Header.Set("File-Path", remote)
	req.Header.Set("Content-Type", "application/octet-stream")

	tokenMu.Lock()
	req.Header.Set("Authorization", token)
	tokenMu.Unlock()

	resp, err := httpCli.Do(req)

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

		addHistoryRecord(local, remote, size, "failed", time.Since(startTime).Seconds(), err.Error())

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
	defer resp.Body.Close()

	// 解析出服务器真实返回的 Message，拒绝吃掉任何服务端返回的详细报错
	var r struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&r); decodeErr != nil {
		r.Code = resp.StatusCode
		r.Message = "解析远端响应体失败或非标准化 JSON 格式"
	} else if r.Code == 0 {
		r.Code = resp.StatusCode
	}

	if r.Code == 200 {
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

		addHistoryRecord(local, remote, size, "success", time.Since(startTime).Seconds(), "")

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
		log.Printf("[UPLOAD][REMOTE][ERR] 远端服务器拒绝或异常，状态码: %d 详细报错: %s 文件: %s", r.Code, r.Message, filepath.Base(local))
		errMsg := fmt.Sprintf("远端拒绝 (Code: %d, 报错: %s)", r.Code, r.Message)

		// 将服务端真实错误暴露给用户的气泡系统
		SendAlert("error", "上传遭拒绝", fmt.Sprintf("文件: %s\n状态码: %d\n详细报错: %s", filepath.Base(local), r.Code, r.Message))

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

		addHistoryRecord(local, remote, size, "failed", time.Since(startTime).Seconds(), errMsg)

		broadcastWS("taskDone", map[string]interface{}{
			"id":     taskID,
			"status": "fail",
			"error":  errMsg,
		})

		// 判断 Token 失效或服务器磁盘已满的逻辑熔断
		fails := atomic.AddInt32(&consecutiveFailures, 1)
		if fails >= 30 {
			pauseSystemOnFailure(fmt.Sprintf("连续 %d 个文件被远端服务器拒绝接收 (状态码: %d，报错: %s)。", fails, r.Code, r.Message))
		}

		return false
	}
}

// addHistoryRecord 将最终确定状态的上传操作以标准格式记录至系统的长驻内存历史队列中
func addHistoryRecord(local, remote string, size int64, status string, duration float64, errorMsg string) {
	record := HistoryRecord{
		UploadTime: time.Now().Format("2006-01-02 15:04:05"),
		Name:       filepath.Base(remote),
		Size:       size,
		LocalPath:  local,
		Remote:     remote,
		Status:     status,
		Duration:   int(duration),
		ErrorMsg:   errorMsg,
	}

	historyMu.Lock()
	history = append(history, &record)
	if len(history) > 1000 {
		history = history[1:]
	}
	historyMu.Unlock()
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
	appConfigMu.RLock()
	defer appConfigMu.RUnlock()

	h := time.Now().Hour()
	if h >= 8 && h < 23 {
		return appConfig.DayRate
	}
	return appConfig.NightRate
}

// cleanFileName 对文件命中可能包含表情符、敏感词或导致 500 异常的不规则符号进行剔除修剪
// 优化：对扩展名也做合法性验证，防止含非 ASCII 字符的扩展名污染上传路径
func cleanFileName(name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)

	// 校验扩展名：只允许字母和数字，非法字符一律清除（如中文扩展名、含空格的扩展名等）
	cleanExt := ext
	if ext != "" {
		var eb strings.Builder
		eb.WriteRune('.') // 保留前导点
		for _, r := range strings.TrimPrefix(ext, ".") {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				eb.WriteRune(r)
			}
		}
		cleanExt = eb.String()
		if cleanExt == "." {
			cleanExt = "" // 扩展名全部是非法字符，直接丢弃
		}
	}

	var b strings.Builder
	lastDash := false

	for _, r := range base {
		if (r >= 0x4E00 && r <= 0x9FFF) || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}

	res := strings.Trim(b.String(), "-")
	if res == "" {
		res = "file"
	}
	return res + cleanExt
}

type UploadRecord struct {
	Time     time.Time
	Streamer string
	Name     string
	Remote   string
	Size     int64
}

// TrendPoint 为增量统计专属配备的高性能细粒度锁定数据结构
type TrendPoint struct {
	Mu    sync.Mutex
	Date  string  `json:"date"`
	Size  float64 `json:"size"`
	Count int     `json:"count"`
}

// updateStatsIncrementally 并发安全的图表源数据累加器，消除了过去 O(N) O(500,000) 的遍历消耗
func updateStatsIncrementally(rec UploadRecord) {
	day := rec.Time.Format("01-02")
	// 获取或初始化当天的 TrendPoint
	tVal, _ := trendStats.LoadOrStore(day, &TrendPoint{Date: day})
	tp := tVal.(*TrendPoint)

	tp.Mu.Lock()
	tp.Size += float64(rec.Size) / 1024 / 1024 / 1024
	tp.Count++
	tp.Mu.Unlock()

	// 更新主播流量排行 Rank
	rVal, _ := rankStats.LoadOrStore(rec.Streamer, &atomic.Int64{})
	rInt := rVal.(*atomic.Int64)
	rInt.Add(rec.Size)
}

// recordSuccess 专门记录最终通过网络被写入目标端存储系统的文件日志以供统计
func recordSuccess(remote, name string, size int64) {
	rec := UploadRecord{
		Time:     time.Now(),
		Streamer: detectStreamer(remote),
		Name:     name,
		Remote:   remote,
		Size:     size,
	}

	// 立刻提交至高性能的增量内存统计池
	updateStatsIncrementally(rec)

	successLogMu.Lock()
	defer successLogMu.Unlock()

	// 优化点：直接向常驻内存的 Slice 追加数据，完全摆脱 `json.Unmarshal(data, &list)` 的 CPU/GC 消耗
	successRecords = append(successRecords, rec)

	// 优化点：限制最多保留 500000 条历史记录，防止内存无限膨胀
	if len(successRecords) > 500000 {
		successRecords = successRecords[len(successRecords)-500000:]
	}

	// 修复 Bug 5：不再每次追加都全量重写整个 JSON 文件（50 万条时 IO 和 GC 压力极大）
	// 改为打脏标记，由 successLogPersistLoop 每 15 秒批量合并落盘一次
	atomic.StoreInt32(&successLogDirty, 1)
}

// flushSuccessLog 将内存中的成功记录全量序列化后以原子替换方式写入磁盘
func flushSuccessLog() {
	successLogMu.Lock()
	snapshot := make([]UploadRecord, len(successRecords))
	copy(snapshot, successRecords)
	successLogMu.Unlock()

	targetDir := filepath.Dir(successLogFile)
	f, err := os.CreateTemp(targetDir, "upload_success_*.json")
	if err != nil {
		log.Printf("[SUCCESS_LOG][ERR] 创建临时日志文件失败: %v", err)
		return
	}
	tmpName := f.Name()
	enc := json.NewEncoder(f)
	encErr := enc.Encode(snapshot)
	f.Close()
	if encErr != nil {
		log.Printf("[SUCCESS_LOG][ERR] 序列化日志失败: %v", encErr)
		os.Remove(tmpName)
		return
	}
	// 原子替换，防止写一半时进程崩溃导致文件损坏
	if err := os.Rename(tmpName, successLogFile); err != nil {
		log.Printf("[SUCCESS_LOG][ERR] 写入日志失败: %v", err)
		os.Remove(tmpName)
	}
}

// successLogPersistLoop 后台守护协程：每 15 秒检查脏标记，批量合并落盘成功记录
func successLogPersistLoop() {
	ticker := time.NewTicker(15 * time.Second)
	for range ticker.C {
		if atomic.CompareAndSwapInt32(&successLogDirty, 1, 0) {
			flushSuccessLog()
		}
	}
}

// reportLoop 长驻于后台的死循环机制，依靠时间计算判断向指定电子信箱推送数据的恰当时间
func reportLoop() {
	appConfigMu.RLock()
	intervalMinutes := appConfig.EmailInterval
	appConfigMu.RUnlock()

	nextReportTime := time.Now().Add(time.Duration(intervalMinutes) * time.Minute)

	for {
		sleepDuration := time.Until(nextReportTime)

		if sleepDuration <= 0 {
			sendReport()

			appConfigMu.RLock()
			intervalMinutes = appConfig.EmailInterval
			appConfigMu.RUnlock()

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
	successLogMu.Lock()
	if len(successRecords) == 0 {
		successLogMu.Unlock()
		return
	}
	// 优化点：直接复制内存副本来处理，脱离磁盘读取
	list := make([]UploadRecord, len(successRecords))
	copy(list, successRecords)
	successLogMu.Unlock()

	appConfigMu.RLock()
	repMinutes := appConfig.EmailInterval
	appConfigMu.RUnlock()

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
	appConfigMu.RLock()
	mailFrom := appConfig.MailFrom
	mailAuthCode := appConfig.MailAuthCode
	mailTo := appConfig.MailTo
	appConfigMu.RUnlock()

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
	appConfigMu.RLock()
	targetServer := appConfig.RemoteServer
	usr := appConfig.RemoteUser
	pwd := appConfig.RemotePass
	appConfigMu.RUnlock()

	body := fmt.Sprintf("Username=%s&Password=%s", usr, pwd)
	req, _ := http.NewRequest("POST", targetServer+"/api/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpCli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var r struct {
		Code int
		Data struct{ Token string }
	}
	json.NewDecoder(resp.Body).Decode(&r)
	if r.Code != 200 {
		return fmt.Errorf("login failed with code %d", r.Code)
	}

	tokenMu.Lock()
	token = r.Data.Token
	tokenMu.Unlock()
	return nil
}

// fileHash 读取文件二进制流将其哈希压缩为无碰撞的 SHA-256 签名用于唯一身份核对
func fileHash(p string) string {
	f, err := os.Open(p)
	if err != nil {
		log.Printf("[HASH][ERR] 无法打开文件计算哈希 %s: %v", p, err)
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		log.Printf("[HASH][ERR] 读取文件计算哈希失败 %s: %v", p, err)
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// loadHashCache 启动时一次性加载哈希库到内存
func loadHashCache() {
	data, err := os.ReadFile(hashFile)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			hashCache.Store(line, struct{}{})
		}
	}
	// 利用 Range 快计缓存总数
	count := 0
	hashCache.Range(func(_, _ interface{}) bool { count++; return true })
	log.Printf("[HASH] 已从磁盘加载 %d 条哈希记录到高并发无锁哈希表中", count)
}

// hashExists 直接查内存哈希集合，O(1) 精确匹配，彻底消灭全文件读写锁瓶颈
func hashExists(h string) bool {
	if h == "" {
		return false
	}
	_, ok := hashCache.Load(h)
	return ok
}

// saveHash 向库内追加全新的防重复哈希值字符串，并同步更新内存缓存
func saveHash(h string) {
	if h == "" {
		return
	}

	// 先写内存缓存，保证下次 hashExists 毫无延迟即可见
	hashCache.Store(h, struct{}{})

	// 追加写入必须锁住 File I/O 以防数据覆盖或坏块
	hashFileMu.Lock()
	defer hashFileMu.Unlock()
	f, err := os.OpenFile(hashFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[HASH][ERR] 打开哈希库文件失败: %v", err)
		return
	}
	defer f.Close()
	f.WriteString(h + "\n")
}

// detectRoot 提供针对底层物理目录映射的反推机制从而确定文件属主节点
func detectRoot(path string) string {
	path = filepath.Clean(path)

	appConfigMu.RLock()
	currentDirs := make([]string, len(appConfig.Dirs))
	copy(currentDirs, appConfig.Dirs)
	appConfigMu.RUnlock()

	for _, d := range currentDirs {
		root := filepath.Clean(strings.TrimSpace(d))
		rel, err := filepath.Rel(root, path)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return root
		}
	}
	return ""
}

// detectStreamer 切割并归类服务器返回的远程文件树节点获得流数据所属的主播用户名
func detectStreamer(remote string) string {
	parts := strings.Split(remote, "/")
	if len(parts) > 2 {
		return parts[2]
	}
	return "未知"
}

// addLog 作为业务和展示系统隔离的桥梁，负责筛选后将指定等级事件装箱并经加密投递到浏览器
func addLog(level, message, errorMsg string) {
	appConfigMu.RLock()
	enabled := appConfig.EnableLogs
	appConfigMu.RUnlock()

	if !enabled {
		return
	}

	entry := &LogEntry{
		Time:    time.Now().Format(time.DateTime),
		Level:   level,
		Message: message,
		Error:   errorMsg,
	}

	select {
	case logChan <- entry:
	default:
	}
}

// getActiveStreamers 通过探测各目录内是否存在时间较新的文件，推断当前正在处于写入(活跃录制)状态的主播名单，并与上次比对触发微信开播/下播通知
func getActiveStreamers() []string {
	appConfigMu.RLock()
	configuredDirs := appConfig.Dirs
	appConfigMu.RUnlock()

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
		if t.Status == "录制中" { // 只排斥确实在录制中的任务，防止干扰
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
