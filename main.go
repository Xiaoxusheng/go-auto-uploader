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

	token   string
	tokenMu sync.Mutex

	httpCli = &http.Client{Timeout: 0}

	hashFile = "uploaded_hash.db" // 已经上传的文件哈希记录（秒传用）

	queueCount   int64 // 等待队列数量
	activeWorker int64 // 当前活跃的 Worker 数量

	// 暴露给前端的动态扫描周期与倒计时时间戳
	currentDynamicIntervalGlobal int64
	nextScanTimeGlobal           int64
)

// 动态应用配置
var (
	appConfig   Config
	appConfigMu sync.RWMutex
)

// 任务队列和历史记录状态
var (
	liveTasks   = make(map[string]*Task)
	liveTasksMu sync.RWMutex

	queueWaiting   = make([]string, 0)
	queueUploading = make([]string, 0)
	queueSuccess   = make([]string, 0)
	queueFail      = make([]string, 0)
	queueRetrying  = make([]string, 0)
	queueMu        sync.RWMutex

	history   = make([]*HistoryRecord, 0)
	historyMu sync.RWMutex
)

var (
	triggerScanCh   = make(chan string, 1)   // 触发强制扫描的通道
	triggerReportCh = make(chan struct{}, 1) // 触发报告重置的通道
)

// 触发目录扫描
func triggerScan(reason string) {
	select {
	case triggerScanCh <- reason:
	default:
	}
}

// 触发报告发送通道重置
func triggerReportReset() {
	select {
	case triggerReportCh <- struct{}{}:
	default:
	}
}

// Task 定义了单个上传任务的属性
type Task struct {
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

// HistoryRecord 定义了历史上传记录
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

const (
	safeBaseDir    = "/_safe_uploads"      // 远端安全目录
	successLogFile = "upload_success.json" // 本地成功日志，用于图表统计

	// 邮件相关配置
	mailFrom     = "2673893724@qq.com"
	mailAuthCode = "koeekvlajhtsdije"
	mailTo       = "3096407768@qq.com"
)

// logInterceptor 拦截标准日志并同步给前端 WebSocket
type logInterceptor struct {
	original io.Writer
}

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
	// 启用日志拦截器，将日志传回网页
	log.SetOutput(&logInterceptor{original: os.Stdout})

	// 初始化系统参数
	appConfigMu.Lock()
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
	appConfigMu.Unlock()

	addLog("info", "系统初始化完成，启动中...", "")

	// 启动 WebAPI 服务器与后台常驻任务
	go StartWebServer(webPort)
	go queueStatusLoop()
	go reportLoop()

	appConfigMu.RLock()
	baseInterval := appConfig.ScanInterval
	appConfigMu.RUnlock()
	currentDynamicInterval := baseInterval
	atomic.StoreInt64(&currentDynamicIntervalGlobal, int64(currentDynamicInterval))

	runningMu.RLock()
	if running {
		_ = login() // 获取远端 token
		activeCount := runOnce("auto", currentDynamicInterval)
		if activeCount > 0 {
			// 如果有正在录制的文件，动态加快扫描频率
			currentDynamicInterval = baseInterval / (activeCount + 1)
			if currentDynamicInterval < 3 {
				currentDynamicInterval = 3
			}
			atomic.StoreInt64(&currentDynamicIntervalGlobal, int64(currentDynamicInterval))
			log.Printf("[SCAN][DYNAMIC] 🔥 开机检测到 %d 个文件正在录制，下次扫描已动态提速至 %d 分钟后", activeCount, currentDynamicInterval)
		}
	}
	runningMu.RUnlock()

	// 主扫描轮询
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

				if activeCount > 0 {
					newInterval := baseInterval / (activeCount + 1)
					if newInterval < 3 {
						newInterval = 3
					}
					if newInterval != currentDynamicInterval {
						log.Printf("[SCAN][DYNAMIC] 🔥 当前有 %d 个主播处于录制写入状态，按比例调整下次扫描为 %d 分钟后", activeCount, newInterval)
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
				// ⭐ 恢复了被删掉的打断日志
				_ = login()
				log.Printf("[SYSTEM] ⚡ 收到指令打断，执行扫描 (触发源: %s)", reason)

				appConfigMu.RLock()
				baseInterval = appConfig.ScanInterval
				appConfigMu.RUnlock()

				activeCount := runOnce(reason, currentDynamicInterval)

				if activeCount > 0 {
					newInterval := baseInterval / (activeCount + 1)
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
				log.Println("[SYSTEM] ⚡ 系统热重载完成")
			}
		}
	}
}

// 打印队列状态日志
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

// 执行单次扫描与任务分发
func runOnce(triggerReason string, currentDynamicInterval int) int {
	atomic.StoreInt64(&nextScanTimeGlobal, -1) // -1 表示前端显示"正在扫描中"

	appConfigMu.RLock()
	currentWorkers := appConfig.Workers
	currentDirs := make([]string, len(appConfig.Dirs))
	copy(currentDirs, appConfig.Dirs)
	appConfigMu.RUnlock()

	taskCh := make(chan string, currentWorkers*2)
	var wg sync.WaitGroup

	runningMu.RLock()
	isRunning := running
	runningMu.RUnlock()

	if !isRunning {
		// ⭐ 恢复了被删掉的暂停提示日志
		log.Println("[UPLOAD][PAUSED] 系统处于暂停状态，跳过本轮扫描")
		return 0
	}

	// ⭐ 恢复了被删掉的扫描启动日志
	log.Printf("[SCAN][START] 🔍 启动目录探测，并发Workers:[%d] 目标路径:[%s]", currentWorkers, strings.Join(currentDirs, " | "))

	// 广播给前端
	broadcastWS("scanStarted", map[string]interface{}{
		"time":     time.Now().UnixMilli(),
		"dirs":     currentDirs,
		"interval": currentDynamicInterval,
		"workers":  currentWorkers,
		"trigger":  triggerReason,
	})

	// 启动 Worker 池
	for i := 0; i < currentWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for path := range taskCh {
				runningMu.RLock()
				isRunning := running
				runningMu.RUnlock()

				if !isRunning {
					log.Printf("[UPLOAD][W%d] 接收到暂停指令，终止任务处理", id)
					return
				}

				atomic.AddInt64(&queueCount, -1)
				atomic.AddInt64(&activeWorker, 1)

				start := time.Now()
				handleFile(path) // 处理文件上传

				// ⭐ 恢复了被删掉的任务完成耗时日志
				log.Printf("[UPLOAD][DONE][W%d] 文件:%s 耗时:%s",
					id, filepath.Base(path), time.Since(start).Truncate(time.Millisecond))

				atomic.AddInt64(&activeWorker, -1)
			}
		}(i + 1)
	}

	// 初始化目录统计状态
	dirStatusesMu.Lock()
	for _, root := range currentDirs {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "." || root == "" {
			continue
		}
		if ds, exists := dirStatuses[root]; exists {
			ds.PendingFiles = 0
			ds.TotalFiles = 0
			ds.TotalSize = 0
			ds.LastScanTime = time.Now().UnixMilli()
		} else {
			dirStatuses[root] = &DirStatus{
				Path:         root,
				LastScanTime: time.Now().UnixMilli(),
			}
		}
	}
	dirStatusesMu.Unlock()

	var newlyAddedFiles int32 = 0
	var activeRecordingCount int32 = 0

	// 遍历并投递任务
	for _, root := range currentDirs {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "." || root == "" {
			continue
		}
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
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
			if info.IsDir() {
				return nil
			}

			// 忽略最后修改时间在 2 分钟内的文件（判定为正在写入）
			if time.Since(info.ModTime()) < 2*time.Minute {
				atomic.AddInt32(&activeRecordingCount, 1)
				return nil
			}

			dirStatusesMu.Lock()
			if ds, exists := dirStatuses[root]; exists {
				ds.PendingFiles++
				ds.TotalFiles++
				ds.TotalSize += info.Size()
			}
			dirStatusesMu.Unlock()

			atomic.AddInt64(&queueCount, 1)
			atomic.AddInt32(&newlyAddedFiles, 1)
			taskCh <- path
			return nil
		})
		if err != nil && err.Error() != "scan canceled by user" {
			// 如果目录扫描报错，发送前端强提醒
			SendAlert("warning", "目录扫描异常", "无法访问部分路径: "+err.Error())
			addLog("error", "文件遍历失败", err.Error())
		}
	}

	close(taskCh)
	wg.Wait() // 等待所有 Worker 完成

	// ⭐ 恢复了被删掉的扫描结束日志
	log.Printf("[SCAN][END] 🏁 本轮扫描完毕。发现新文件: %d 个 | 仍在录制中文件: %d 个", newlyAddedFiles, activeRecordingCount)

	broadcastWS("scanFinished", map[string]interface{}{
		"time":  time.Now().UnixMilli(),
		"added": newlyAddedFiles,
	})

	return int(activeRecordingCount)
}

func cleanupFailedTasksByPath(targetPath string) {
	liveTasksMu.Lock()
	defer liveTasksMu.Unlock()

	queueMu.Lock()
	defer queueMu.Unlock()

	var toDelete []string
	for id, task := range liveTasks {
		if task.Path == targetPath && task.Status == "failed" {
			toDelete = append(toDelete, id)
			delete(liveTasks, id)
		}
	}

	for _, id := range toDelete {
		queueFail = removeTask(queueFail, id)
	}
}

// 处理单个文件的上传逻辑
func handleFile(path string) {
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("[FILE][ERR] 无法获取文件状态 %s: %v", path, err)
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
	name := cleanFileName(filepath.Base(rel))
	remote := filepath.ToSlash(filepath.Join(safeBaseDir, dir, name))

	// 检测秒传机制 (Hash)
	hash := fileHash(path)
	if hashExists(hash) {
		// ⭐ 恢复了被删掉的秒传日志
		log.Println("[SKIP][HASH] 文件已存在于记录中 (秒传触发):", path)

		taskID := fmt.Sprintf("task-%d", time.Now().UnixNano())

		liveTasksMu.Lock()
		liveTasks[taskID] = &Task{
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
		liveTasksMu.Unlock()

		queueMu.Lock()
		queueSuccess = append(queueSuccess, taskID)
		queueMu.Unlock()

		addHistoryRecord(path, remote, info.Size(), "success(秒传)", 0, "")

		dirStatusesMu.Lock()
		if ds, exists := dirStatuses[root]; exists {
			if ds.PendingFiles > 0 {
				ds.PendingFiles--
			}
			ds.UploadedFiles++
			ds.UploadedSize += info.Size()
		}
		dirStatusesMu.Unlock()

		broadcastWS("taskDone", map[string]interface{}{
			"id":     taskID,
			"status": "success",
			"size":   info.Size(),
		})

		cleanupFailedTasksByPath(path)
		return
	}

	// 开始执行远端上传
	if upload(path, remote, info.Size()) {
		saveHash(hash)
		if err := os.Remove(path); err != nil {
			log.Printf("[FILE][CLEAN][ERR] 移除本地文件失败 %s: %v", path, err)
		}
		recordSuccess(remote, name, info.Size())

		dirStatusesMu.Lock()
		if ds, exists := dirStatuses[root]; exists {
			if ds.PendingFiles > 0 {
				ds.PendingFiles--
			}
			ds.UploadedFiles++
			ds.UploadedSize += info.Size()
		}
		dirStatusesMu.Unlock()
	}
}

// 执行 HTTP PUT 上传
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

	liveTasksMu.Lock()
	liveTasks[taskID] = &Task{
		ID:        taskID,
		Name:      filepath.Base(remote),
		Path:      local,
		Size:      size,
		Progress:  0,
		Speed:     0,
		Status:    "uploading",
		CreatedAt: startTime,
	}
	liveTasksMu.Unlock()

	queueMu.Lock()
	queueUploading = append(queueUploading, taskID)
	queueMu.Unlock()

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

		liveTasksMu.Lock()
		if task, exists := liveTasks[taskID]; exists {
			task.Status = "failed"
			task.Error = err.Error()
			task.EndTime = time.Now()
		}
		liveTasksMu.Unlock()

		queueMu.Lock()
		queueUploading = removeTask(queueUploading, taskID)
		queueFail = append(queueFail, taskID)
		queueMu.Unlock()

		addHistoryRecord(local, remote, size, "failed", time.Since(startTime).Seconds(), err.Error())

		broadcastWS("taskDone", map[string]interface{}{
			"id":     taskID,
			"status": "fail",
			"error":  err.Error(),
		})
		return false
	}
	defer resp.Body.Close()

	var r struct{ Code int }
	if decodeErr := json.NewDecoder(resp.Body).Decode(&r); decodeErr != nil {
		r.Code = resp.StatusCode
	}

	if r.Code == 200 {
		liveTasksMu.Lock()
		if task, exists := liveTasks[taskID]; exists {
			task.Status = "success"
			task.Progress = 100
			task.EndTime = time.Now()
		}
		liveTasksMu.Unlock()

		queueMu.Lock()
		queueUploading = removeTask(queueUploading, taskID)
		queueSuccess = append(queueSuccess, taskID)
		queueMu.Unlock()

		addHistoryRecord(local, remote, size, "success", time.Since(startTime).Seconds(), "")

		broadcastWS("taskDone", map[string]interface{}{
			"id":       taskID,
			"status":   "success",
			"progress": 100,
			"size":     size,
		})

		cleanupFailedTasksByPath(local)
		return true

	} else {
		log.Printf("[UPLOAD][REMOTE][ERR] 远端服务器拒绝或异常，状态码: %d 文件: %s", r.Code, filepath.Base(local))
		errMsg := fmt.Sprintf("远端拒绝接收 (Code: %d)", r.Code)

		// Token 失效发送通知
		SendAlert("error", "上传遭拒绝", fmt.Sprintf("文件: %s\n原因: 远端返回 Code %d，可能 Token 已过期或配置错误。", filepath.Base(local), r.Code))

		liveTasksMu.Lock()
		if task, exists := liveTasks[taskID]; exists {
			task.Status = "failed"
			task.Error = errMsg
			task.EndTime = time.Now()
		}
		liveTasksMu.Unlock()

		queueMu.Lock()
		queueUploading = removeTask(queueUploading, taskID)
		queueFail = append(queueFail, taskID)
		queueMu.Unlock()

		addHistoryRecord(local, remote, size, "failed", time.Since(startTime).Seconds(), errMsg)

		broadcastWS("taskDone", map[string]interface{}{
			"id":     taskID,
			"status": "fail",
			"error":  errMsg,
		})

		return false
	}
}

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

func removeTask(tasks []string, taskID string) []string {
	for i, t := range tasks {
		if t == taskID {
			return append(tasks[:i], tasks[i+1:]...)
		}
	}
	return tasks
}

// ProgressReader 带速率限制和进度通知的流读取器
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
		progress := int(float64(p.read) * 100 / float64(p.total))
		speed := int64(float64(p.read) / float64(time.Since(p.start).Seconds()))

		step := progress / 10
		if step > p.lastLogProg {
			p.lastLogProg = step
			log.Printf("[UPLOAD][PROGRESS] 文件: %s -> 进度: %d%%", p.name, step*10)
		}

		liveTasksMu.Lock()
		if task, exists := liveTasks[p.taskID]; exists {
			task.Progress = progress
			task.Speed = speed
			broadcastWS("uploadProgress", map[string]interface{}{
				"id":        p.taskID,
				"filename":  task.Name,
				"path":      task.Path,
				"size":      task.Size,
				"uploaded":  p.read,
				"speed":     speed,
				"status":    "uploading",
				"startTime": task.CreatedAt.UnixMilli(),
			})
		}
		liveTasksMu.Unlock()
	}
	return n, err
}

// 根据时间动态计算限速
func currentRate() int {
	appConfigMu.RLock()
	defer appConfigMu.RUnlock()

	h := time.Now().Hour()
	if h >= 8 && h < 23 {
		return appConfig.DayRate
	}
	return appConfig.NightRate
}

// ⭐ 恢复了被删掉的完善版特殊字符清理逻辑
func cleanFileName(name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)

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
	return res + ext
}

type UploadRecord struct {
	Time     time.Time
	Streamer string
	Name     string
	Remote   string
	Size     int64
}

// 记录成功上传日志，用于大屏统计展示
func recordSuccess(remote, name string, size int64) {
	var list []UploadRecord
	data, _ := os.ReadFile(successLogFile)
	json.Unmarshal(data, &list)

	list = append(list, UploadRecord{
		Time:     time.Now(),
		Streamer: detectStreamer(remote),
		Name:     name,
		Remote:   remote,
		Size:     size,
	})

	b, _ := json.MarshalIndent(list, "", "  ")
	if err := os.WriteFile(successLogFile, b, 0644); err != nil {
		log.Printf("[CLEAN][SUCCESS_LOG][ERR] 写入日志失败: %v", err)
	}
}

// 邮件发送循环
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

// ⭐ 恢复了完整的 HTML 邮件发送格式逻辑
func sendReport() {
	data, _ := os.ReadFile(successLogFile)
	var list []UploadRecord
	if json.Unmarshal(data, &list) != nil || len(list) == 0 {
		return
	}

	group := map[string][]UploadRecord{}
	var totalBytes int64

	for _, r := range list {
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

	appConfigMu.RLock()
	repMinutes := appConfig.EmailInterval
	appConfigMu.RUnlock()

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
<td><div style="font-size:12px;color:#6b7280;">文件数量</div><div style="font-size:22px;color:#111827;"><b>%d</b></div></td>
<td><div style="font-size:12px;color:#6b7280;">消耗流量</div><div style="font-size:22px;color:#111827;"><b>%.2f MB</b></div></td>
<td><div style="font-size:12px;color:#6b7280;">主播数量</div><div style="font-size:22px;color:#111827;"><b>%d</b></div></td>
</tr>
</table></td></tr>
`, len(list), totalMB, len(group)))

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

	log.Printf("[REPORT] 📤 正在发送本周期统计邮件，包含 %d 个文件记录", len(list))
	sendQQMail("📦 上传成功报告", html.String())
}

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

func fileHash(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashExists(h string) bool {
	data, _ := os.ReadFile(hashFile)
	return strings.Contains(string(data), h)
}

func saveHash(h string) {
	f, _ := os.OpenFile(hashFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer f.Close()
	f.WriteString(h + "\n")
}

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

func detectStreamer(remote string) string {
	parts := strings.Split(remote, "/")
	if len(parts) > 2 {
		return parts[2]
	}
	return "未知"
}

func sendQQMail(subject, body string) {
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
		log.Printf("[REPORT][MAIL][ERR] 发送失败: %v", err)
	}
}

// ⭐ 这里是触发记录的地方，只要 enableLogs 开关打开就会投递进 WebSocket
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
