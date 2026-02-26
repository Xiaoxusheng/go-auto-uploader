package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var (
	//go:embed index.html
	indexHTML string

	// WebSocket 升级器配置，允许跨域
	wsUpgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	wsClients   = make(map[*websocket.Conn]bool)
	wsMutex     sync.Mutex
	wsBroadcast = make(chan WSMessage, 100)

	running   = false
	runningMu sync.RWMutex
	startTime time.Time

	dirStatuses   = make(map[string]*DirStatus)
	dirStatusesMu sync.RWMutex

	logs    = make([]*LogEntry, 0, 1000)
	logsMu  sync.RWMutex
	logChan = make(chan *LogEntry, 1000)
)

type WSMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

type DirStatus struct {
	Path          string `json:"path"`
	TotalFiles    int    `json:"totalFiles"`
	UploadedFiles int    `json:"uploadedFiles"`
	PendingFiles  int    `json:"pendingFiles"`
	TotalSize     int64  `json:"totalSize"`
	UploadedSize  int64  `json:"uploadedSize"`
	LastScanTime  int64  `json:"lastScanTime"`
}

type Config struct {
	ScanInterval       int      `json:"scanInterval"`
	Workers            int      `json:"workers"`
	DayRate            int      `json:"dayRate"`
	NightRate          int      `json:"nightRate"`
	EmailInterval      int      `json:"emailInterval"`
	Running            bool     `json:"running"`
	AutoRetry          bool     `json:"autoRetry"`
	MaxRetry           int      `json:"maxRetry"`
	EnableLogs         bool     `json:"enableLogs"`
	LogLevel           string   `json:"logLevel"`
	Dirs               []string `json:"dirs"`
	RemoteServer       string   `json:"remoteServer"`
	RemoteUser         string   `json:"remoteUser"`
	RemotePass         string   `json:"remotePass"`
	LiveConfigPath     string   `json:"liveConfigPath"`
	RecorderContainer  string   `json:"recorderContainer"`
	RecorderConfigPath string   `json:"recorderConfigPath"`
	// 新增的邮箱配置字段
	MailFrom     string `json:"mailFrom"`
	MailAuthCode string `json:"mailAuthCode"`
	MailTo       string `json:"mailTo"`
}

type Streamer struct {
	URL    string `json:"url"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

type LogEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

type APIResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

// 供图表使用的趋势数据结构
type TrendPoint struct {
	Date  string  `json:"date"`
	Size  float64 `json:"size"`  // 单位 GB
	Count int     `json:"count"` // 文件数量
}

type StreamerRank struct {
	Name string  `json:"name"`
	Size float64 `json:"size"` // 单位 GB
}

func StartWebServer(port int) {
	runningMu.Lock()
	// ⭐ 重启后从日志恢复成功任务计数
	restoreQueueCounts()
	running = true
	startTime = time.Now()
	runningMu.Unlock()

	mux := http.NewServeMux()

	// 核心路由与 API
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/v1/auth/login", handleLogin)
	mux.HandleFunc("/api/v1/auth/logout", handleLogout)
	mux.HandleFunc("/api/v1/status", handleStatus)
	mux.HandleFunc("/api/v1/tasks/live", handleLiveTasks)
	mux.HandleFunc("/api/v1/tasks/history", handleHistory)
	mux.HandleFunc("/api/v1/tasks/queue", handleQueue)

	// 远程操作与控制指令
	mux.HandleFunc("/api/v1/control/start", handleControlStart)
	mux.HandleFunc("/api/v1/control/pause", handleControlPause)
	mux.HandleFunc("/api/v1/control/stop", handleControlStop)
	mux.HandleFunc("/api/v1/control/relogin", handleControlRelogin)
	mux.HandleFunc("/api/v1/control/rescan", handleControlRescan)
	mux.HandleFunc("/api/v1/control/clear-fail-queue", handleControlClearFailQueue)
	mux.HandleFunc("/api/v1/control/retry-fail-queue", handleControlRetryFailQueue)
	mux.HandleFunc("/api/v1/control/clear-success-queue", handleControlClearSuccessQueue)

	mux.HandleFunc("/api/v1/dirs/status", handleDirsStatus)
	mux.HandleFunc("/api/v1/config", handleConfig)
	mux.HandleFunc("/api/v1/logs", handleLogs)
	mux.HandleFunc("/api/v1/logs/download", handleLogsDownload)

	// 录制引擎管理
	mux.HandleFunc("/api/v1/streamers", handleStreamers)
	mux.HandleFunc("/api/v1/streamers/active", handleActiveStreamers)
	mux.HandleFunc("/api/v1/recorder/status", handleRecorderStatus)
	mux.HandleFunc("/api/v1/recorder/control", handleRecorderControl)
	mux.HandleFunc("/api/v1/recorder/logs", handleRecorderLogs)
	mux.HandleFunc("/api/v1/cookies", handleCookies)

	// WebSocket 信道
	mux.HandleFunc("/ws/live", handleWebSocket)

	go wsBroadcastLoop()
	go logCollector()
	go wsDashboardBroadcaster()

	addr := fmt.Sprintf(":%d", port)
	log.Printf("[WEB] 控制台已就绪: http://127.0.0.1%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("[WEB] Server error: %v", err)
	}
}

// ⭐ 新增：向前端实时推送系统弹窗告警
// level 可选值: "info", "success", "warning", "error"
func SendAlert(level string, title string, message string) {
	broadcastWS("systemAlert", map[string]interface{}{
		"level":   level,
		"title":   title,
		"message": message,
		"time":    time.Now().Format("15:04:05"),
	})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if len(indexHTML) == 0 {
		http.Error(w, "index.html not found.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, indexHTML)
}

// 统计聚合计算函数：提取出逻辑，供 WebSocket 定时调用
func buildStatsTrendData() map[string]interface{} {
	data, err := os.ReadFile(successLogFile)
	if err != nil {
		return map[string]interface{}{"trend": []TrendPoint{}, "rank": []StreamerRank{}}
	}

	var list []UploadRecord
	json.Unmarshal(data, &list)

	// 初始化最近 7 天的空数据架子
	trendMap := make(map[string]*TrendPoint)
	now := time.Now()
	for i := 0; i < 7; i++ {
		d := now.AddDate(0, 0, -i).Format("01-02")
		trendMap[d] = &TrendPoint{Date: d, Size: 0, Count: 0}
	}

	rankMap := make(map[string]int64)

	// 遍历日志聚合并累加数据
	for _, rec := range list {
		day := rec.Time.Format("01-02")
		if tp, ok := trendMap[day]; ok {
			tp.Size += float64(rec.Size) / 1024 / 1024 / 1024 // 转换为 GB
			tp.Count++
		}
		rankMap[rec.Streamer] += rec.Size
	}

	// 将 Map 转换为有序 Slice，供 ECharts X 轴使用
	trendResult := make([]TrendPoint, 0)
	for i := 6; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("01-02")
		trendResult = append(trendResult, *trendMap[d])
	}

	// 计算排行榜
	rankResult := make([]StreamerRank, 0)
	for name, size := range rankMap {
		rankResult = append(rankResult, StreamerRank{
			Name: name,
			Size: float64(size) / 1024 / 1024 / 1024,
		})
	}

	sort.Slice(rankResult, func(i, j int) bool {
		return rankResult[i].Size > rankResult[j].Size
	})
	if len(rankResult) > 5 {
		rankResult = rankResult[:5]
	}

	return map[string]interface{}{
		"trend": trendResult,
		"rank":  rankResult,
	}
}

// WebSocket 消息推送协程
func wsBroadcastLoop() {
	for {
		msg := <-wsBroadcast
		wsMutex.Lock()
		for client := range wsClients {
			if err := client.WriteJSON(msg); err != nil {
				client.Close()
				delete(wsClients, client)
			}
		}
		wsMutex.Unlock()
	}
}

// 定时向前端广播实时状态指标
func wsDashboardBroadcaster() {
	ticker := time.NewTicker(2 * time.Second) // 频率保持为 2 秒
	defer ticker.Stop()

	for range ticker.C {
		wsMutex.Lock()
		clientCount := len(wsClients)
		wsMutex.Unlock()

		if clientCount > 0 {
			broadcastWS("systemStatus", buildStatusData())
			broadcastWS("queueStatus", buildQueueData())

			// 计算瞬时全站上传速率（供前端仪表盘）
			var totalSpeed int64 = 0
			liveTasksMu.RLock()
			for _, task := range liveTasks {
				if task.Status == "uploading" {
					totalSpeed += task.Speed
				}
			}
			liveTasksMu.RUnlock()

			broadcastWS("trafficMetrics", map[string]interface{}{
				"speed": totalSpeed, // 字节每秒
				"time":  time.Now().UnixMilli(),
			})

			// 新增：每次也通过 WebSocket 将历史聚合趋势推送给大屏
			broadcastWS("statsTrend", buildStatsTrendData())

			broadcastWS("recorderStatus", getRecorderStatus())
			broadcastWS("activeStreamers", getActiveStreamers())
			broadcastWS("streamersData", getStreamersData())
		}
	}
}

func broadcastWS(msgType string, payload interface{}) {
	select {
	case wsBroadcast <- WSMessage{Type: msgType, Payload: payload}:
	default:
	}
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	wsMutex.Lock()
	wsClients[conn] = true
	wsMutex.Unlock()

	for {
		var msg WSMessage
		err := conn.ReadJSON(&msg)
		if err != nil {
			break
		}
		if msg.Type == "ping" {
			wsMutex.Lock()
			_ = conn.WriteJSON(WSMessage{Type: "pong", Payload: msg.Payload})
			wsMutex.Unlock()
		}
	}

	wsMutex.Lock()
	delete(wsClients, conn)
	wsMutex.Unlock()
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		OTP      string `json:"otp"`
	}
	json.Unmarshal(body, &req)

	if req.Username != dashboardUsername || req.Password != dashboardPassword {
		sendJSONError(w, http.StatusUnauthorized, "Invalid credentials")
		return
	}
	expiry := time.Now().Add(24 * time.Hour).UnixMilli()
	if token == "" {
		token = "test-token-" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	sendJSONSuccess(w, map[string]interface{}{"token": token, "expiry": expiry})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	sendJSONSuccess(w, nil)
}

func buildStatusData() map[string]interface{} {
	runningMu.RLock()
	isRunning := running
	runningMu.RUnlock()

	appConfigMu.RLock()
	currentScanInterval := appConfig.ScanInterval
	currentDayRate := appConfig.DayRate
	currentNightRate := appConfig.NightRate
	currentWorkers := appConfig.Workers
	configuredDirs := appConfig.Dirs
	appConfigMu.RUnlock()

	dynInterval := atomic.LoadInt64(&currentDynamicIntervalGlobal)
	if dynInterval == 0 {
		dynInterval = int64(currentScanInterval)
	}
	nextScan := atomic.LoadInt64(&nextScanTimeGlobal)

	dirStatusesMu.RLock()
	dirs := make([]map[string]interface{}, 0)
	for _, dir := range configuredDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if status, exists := dirStatuses[dir]; exists {
			dirs = append(dirs, map[string]interface{}{
				"path":          status.Path,
				"totalFiles":    status.TotalFiles,
				"uploadedFiles": status.UploadedFiles,
				"pendingFiles":  status.PendingFiles,
				"totalSize":     status.TotalSize,
				"uploadedSize":  status.UploadedSize,
				"lastScanTime":  status.LastScanTime,
			})
		} else {
			dirs = append(dirs, map[string]interface{}{
				"path":          dir,
				"totalFiles":    0,
				"uploadedFiles": 0,
				"pendingFiles":  0,
				"totalSize":     0,
				"uploadedSize":  0,
				"lastScanTime":  time.Now().UnixMilli(),
			})
		}
	}
	dirStatusesMu.RUnlock()

	return map[string]interface{}{
		"running":          isRunning,
		"tokenValid":       token != "",
		"workers":          currentWorkers,
		"dirs":             dirs,
		"scanningInterval": currentScanInterval,
		"dynamicInterval":  dynInterval,
		"nextScanTime":     nextScan, // 倒计时锚点
		"rate":             currentRate(),
		"dayRate":          currentDayRate,
		"nightRate":        currentNightRate,
		"uptime":           int64(time.Since(startTime).Seconds()),
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	sendJSONSuccess(w, buildStatusData())
}

func buildQueueData() map[string]interface{} {
	queueMu.RLock()
	defer queueMu.RUnlock()

	return map[string]interface{}{
		"waiting":   atomic.LoadInt64(&queueCount),
		"uploading": len(queueUploading),
		"success":   len(queueSuccess),
		"failed":    len(queueFail),
		"retrying":  len(queueRetrying),
	}
}

func handleQueue(w http.ResponseWriter, r *http.Request) {
	sendJSONSuccess(w, buildQueueData())
}

func handleLiveTasks(w http.ResponseWriter, r *http.Request) {
	liveTasksMu.RLock()
	tasks := make([]map[string]interface{}, 0, len(liveTasks))
	cutoffTime := time.Now().Add(-30 * time.Minute)

	for _, task := range liveTasks {
		if task.CreatedAt.After(cutoffTime) {
			duration := 0
			if !task.EndTime.IsZero() {
				duration = int(task.EndTime.Sub(task.CreatedAt).Seconds())
			}

			tasks = append(tasks, map[string]interface{}{
				"id":        task.ID,
				"filename":  task.Name,
				"path":      task.Path,
				"size":      task.Size,
				"uploaded":  int64(float64(task.Size) * float64(task.Progress) / 100),
				"speed":     task.Speed,
				"status":    task.Status,
				"error":     task.Error,
				"startTime": task.CreatedAt.UnixMilli(),
				"duration":  duration,
			})
		}
	}
	liveTasksMu.RUnlock()
	sendJSONSuccess(w, tasks)
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page, _ := strconv.Atoi(query.Get("page"))
	limit, _ := strconv.Atoi(query.Get("limit"))
	status := query.Get("status")
	filename := query.Get("filename")

	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 50
	}

	historyMu.RLock()
	filtered := make([]*HistoryRecord, 0)
	for _, record := range history {
		if status != "" && !strings.Contains(record.Status, status) {
			continue
		}
		if filename != "" && !strings.Contains(strings.ToLower(record.Name), strings.ToLower(filename)) {
			continue
		}
		filtered = append(filtered, record)
	}

	total := len(filtered)
	start := (page - 1) * limit
	end := start + limit
	if start >= total {
		historyMu.RUnlock()
		sendJSONSuccess(w, map[string]interface{}{"items": []*HistoryRecord{}, "total": total})
		return
	}
	if end > total {
		end = total
	}

	result := filtered[start:end]
	historyMu.RUnlock()

	items := make([]map[string]interface{}, 0, len(result))
	for _, record := range result {
		items = append(items, map[string]interface{}{
			"id":       record.UploadTime,
			"filename": record.Name,
			"path":     record.LocalPath,
			"size":     record.Size,
			"uploaded": record.Size,
			"speed":    0,
			"status":   record.Status,
			"error":    record.ErrorMsg,
			"duration": record.Duration,
		})
	}

	sendJSONSuccess(w, map[string]interface{}{"items": items, "total": total})
}

func handleControlStart(w http.ResponseWriter, r *http.Request) {
	runningMu.Lock()
	running = true
	startTime = time.Now()
	runningMu.Unlock()

	log.Println("[CONTROL] 🚀 用户下发指令：启动系统，恢复扫描与上传任务")
	triggerScan("start")
	sendJSONSuccess(w, nil)
}

func handleControlPause(w http.ResponseWriter, r *http.Request) {
	runningMu.Lock()
	running = false
	runningMu.Unlock()

	log.Println("[CONTROL] ⏸️ 用户下发指令：暂停系统运行")
	sendJSONSuccess(w, nil)
}

func handleControlStop(w http.ResponseWriter, r *http.Request) {
	runningMu.Lock()
	running = false
	runningMu.Unlock()

	log.Println("[CONTROL] 🛑 用户下发指令：停止系统运行")
	sendJSONSuccess(w, nil)
}

func handleControlRelogin(w http.ResponseWriter, r *http.Request) {
	if err := login(); err != nil {
		log.Println("[CONTROL][ERR] 用户尝试刷新远端授权失败:", err)
		sendJSONError(w, http.StatusInternalServerError, "Relogin failed")
		return
	}
	log.Println("[CONTROL] 🔑 用户下发指令：远端授权凭证已成功刷新")
	sendJSONSuccess(w, nil)
}

func handleControlRescan(w http.ResponseWriter, r *http.Request) {
	log.Println("[CONTROL] 🔍 用户下发指令：手动触发深度目录重新扫描")
	triggerScan("rescan")
	sendJSONSuccess(w, map[string]interface{}{"message": "重新扫描已触发"})
}

func handleControlClearFailQueue(w http.ResponseWriter, r *http.Request) {
	queueMu.Lock()
	queueFail = make([]string, 0)
	queueMu.Unlock()
	log.Println("[CONTROL] 🧹 用户下发指令：已清空失败任务队列")
	sendJSONSuccess(w, nil)
}

func handleControlRetryFailQueue(w http.ResponseWriter, r *http.Request) {
	queueMu.Lock()
	for _, taskID := range queueFail {
		queueWaiting = append(queueWaiting, taskID)
	}
	queueFail = make([]string, 0)
	queueMu.Unlock()
	log.Println("[CONTROL] 🔄 用户下发指令：失败任务已全部压入等待队列准备重试")
	sendJSONSuccess(w, nil)
}

func handleControlClearSuccessQueue(w http.ResponseWriter, r *http.Request) {
	queueMu.Lock()
	queueSuccess = make([]string, 0)
	queueMu.Unlock()
	log.Println("[CONTROL] 🧹 用户下发指令：已清空成功任务队列展示历史")
	sendJSONSuccess(w, nil)
}

func handleDirsStatus(w http.ResponseWriter, r *http.Request) {
	appConfigMu.RLock()
	configuredDirs := appConfig.Dirs
	appConfigMu.RUnlock()

	dirStatusesMu.RLock()
	statuses := make([]*DirStatus, 0)
	for _, dir := range configuredDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if status, exists := dirStatuses[dir]; exists {
			statuses = append(statuses, status)
		} else {
			statuses = append(statuses, &DirStatus{
				Path:         dir,
				LastScanTime: time.Now().UnixMilli(),
			})
		}
	}
	dirStatusesMu.RUnlock()

	sendJSONSuccess(w, statuses)
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		appConfigMu.RLock()
		currentConfig := appConfig
		appConfigMu.RUnlock()
		sendJSONSuccess(w, currentConfig)
	case http.MethodPut:
		var newConfig Config
		if err := json.NewDecoder(r.Body).Decode(&newConfig); err != nil {
			sendJSONError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
		appConfigMu.Lock()
		appConfig = newConfig
		appConfigMu.Unlock()

		log.Printf("[CONTROL] ⚙️ 用户保存了新配置，目标扫描目录已变更为: [%s]", strings.Join(newConfig.Dirs, " | "))

		triggerScan("config-update")
		triggerReportReset()

		sendJSONSuccess(w, nil)
	default:
		sendJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func handleCookies(w http.ResponseWriter, r *http.Request) {
	appConfigMu.RLock()
	configPath := appConfig.RecorderConfigPath
	appConfigMu.RUnlock()

	if configPath == "" {
		sendJSONError(w, http.StatusBadRequest, "尚未配置录制引擎主配置文件路径 (config.ini)")
		return
	}

	if r.Method == http.MethodGet {
		data, err := os.ReadFile(configPath)
		if err != nil {
			sendJSONSuccess(w, map[string]string{"douyin": "", "kuaishou": "", "sooplive": "", "liveSavePath": ""})
			return
		}

		// 强行清理文件中的 BOM 隐藏字符，防止解析报错
		content := string(data)
		content = strings.ReplaceAll(content, "\ufeff", "")

		res := map[string]string{"douyin": "", "kuaishou": "", "sooplive": "", "liveSavePath": ""}
		lines := strings.Split(content, "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "douyin_cookie") {
				parts := strings.SplitN(trimmed, "=", 2)
				if len(parts) == 2 {
					res["douyin"] = strings.TrimSpace(parts[1])
				}
			} else if strings.HasPrefix(trimmed, "kuaishou_cookie") {
				parts := strings.SplitN(trimmed, "=", 2)
				if len(parts) == 2 {
					res["kuaishou"] = strings.TrimSpace(parts[1])
				}
			} else if strings.HasPrefix(trimmed, "sooplive_cookie") {
				parts := strings.SplitN(trimmed, "=", 2)
				if len(parts) == 2 {
					res["sooplive"] = strings.TrimSpace(parts[1])
				}
			} else if strings.HasPrefix(trimmed, "live_path") { // 解析引擎的保存路径
				parts := strings.SplitN(trimmed, "=", 2)
				if len(parts) == 2 {
					res["liveSavePath"] = strings.TrimSpace(parts[1])
				}
			}
		}

		sendJSONSuccess(w, res)
		return
	}

	if r.Method == http.MethodPut {
		var req struct {
			Douyin       string `json:"douyin"`
			Kuaishou     string `json:"kuaishou"`
			Sooplive     string `json:"sooplive"`
			LiveSavePath string `json:"liveSavePath"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			sendJSONError(w, http.StatusBadRequest, "Invalid JSON body")
			return
		}

		data, err := os.ReadFile(configPath)
		if err != nil {
			sendJSONError(w, http.StatusInternalServerError, "读取配置文件失败: "+err.Error())
			return
		}

		// 检查原文件是否有 BOM，如果有则记录下来，然后全局清洗掉它
		content := string(data)
		hasBOM := strings.HasPrefix(content, "\ufeff") || strings.Contains(content, "\ufeff")
		content = strings.ReplaceAll(content, "\ufeff", "")

		lines := strings.Split(content, "\n")
		douyinFound, kuaishouFound, soopliveFound, livePathFound := false, false, false, false

		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "douyin_cookie") {
				lines[i] = "douyin_cookie=" + req.Douyin
				douyinFound = true
			} else if strings.HasPrefix(trimmed, "kuaishou_cookie") {
				lines[i] = "kuaishou_cookie=" + req.Kuaishou
				kuaishouFound = true
			} else if strings.HasPrefix(trimmed, "sooplive_cookie") {
				lines[i] = "sooplive_cookie=" + req.Sooplive
				soopliveFound = true
			} else if strings.HasPrefix(trimmed, "live_path") { // 覆写物理路径
				lines[i] = "live_path=" + req.LiveSavePath
				livePathFound = true
			}
		}

		// 追加 Cookie 逻辑
		insertAfterCookie := func(key, val string) {
			for i, line := range lines {
				if strings.TrimSpace(line) == "[cookie]" {
					lines = append(lines[:i+1], append([]string{key + "=" + val}, lines[i+1:]...)...)
					return
				}
			}
			lines = append(lines, "", "[cookie]", key+"="+val)
		}

		// 追加 Base 配置逻辑
		insertAfterBase := func(key, val string) {
			for i, line := range lines {
				if strings.TrimSpace(line) == "[base]" {
					lines = append(lines[:i+1], append([]string{key + "=" + val}, lines[i+1:]...)...)
					return
				}
			}
			// 如果连 [base] 节点都没有，直接插在最前面
			lines = append([]string{"[base]", key + "=" + val, ""}, lines...)
		}

		if !douyinFound && req.Douyin != "" {
			insertAfterCookie("douyin_cookie", req.Douyin)
		}
		if !kuaishouFound && req.Kuaishou != "" {
			insertAfterCookie("kuaishou_cookie", req.Kuaishou)
		}
		if !soopliveFound && req.Sooplive != "" {
			insertAfterCookie("sooplive_cookie", req.Sooplive)
		}
		if !livePathFound && req.LiveSavePath != "" {
			insertAfterBase("live_path", req.LiveSavePath) // 注入路径配置
		}

		// 合并内容
		finalContent := strings.Join(lines, "\n")
		// 如果原文件有 BOM，我们安全地只把它加在绝对的第一行第一列
		if hasBOM {
			finalContent = "\ufeff" + finalContent
		}

		err = os.WriteFile(configPath, []byte(finalContent), 0644)
		if err != nil {
			log.Printf("[CONTROL][ERR] 无法写入配置文件 %s: %v", configPath, err)
			sendJSONError(w, http.StatusInternalServerError, "保存配置失败: "+err.Error())
			return
		}

		log.Printf("[CONTROL] 🍪 用户在网页端成功更新并自动修复了主配置文件编码 %s", configPath)
		sendJSONSuccess(w, nil)
		return
	}

	sendJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
}

func getStreamersData() []Streamer {
	appConfigMu.RLock()
	configPath := appConfig.LiveConfigPath
	appConfigMu.RUnlock()

	data, err := os.ReadFile(configPath)
	if err != nil {
		return []Streamer{}
	}

	content := string(data)
	content = strings.TrimPrefix(content, "\ufeff")

	lines := strings.Split(content, "\n")
	var streamers []Streamer
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "\ufeff")

		if line == "" {
			continue
		}

		line = strings.ReplaceAll(line, "，", ",")

		active := true
		if strings.HasPrefix(line, "#") {
			active = false
			line = strings.TrimPrefix(line, "#")
			line = strings.TrimPrefix(line, "\ufeff")
		}

		parts := strings.SplitN(line, ",", 2)
		url := strings.TrimSpace(parts[0])
		url = strings.ReplaceAll(url, "\ufeff", "")

		name := ""
		if len(parts) > 1 {
			name = strings.TrimSpace(parts[1])
			name = strings.TrimPrefix(name, "主播: ")
			name = strings.TrimPrefix(name, "主播:")
			name = strings.TrimPrefix(name, "主播：")
			name = strings.ReplaceAll(name, "\ufeff", "")
		}

		if strings.TrimSpace(name) == "" || name == "未命名" || name == "⏳ 等待自动获取..." {
			name = ""
		}

		streamers = append(streamers, Streamer{URL: url, Name: name, Active: active})
	}

	if streamers == nil {
		streamers = []Streamer{}
	}
	return streamers
}

func handleStreamers(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		sendJSONSuccess(w, getStreamersData())
		return
	}

	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		appConfigMu.RLock()
		configPath := appConfig.LiveConfigPath
		appConfigMu.RUnlock()

		var req []Streamer
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			sendJSONError(w, http.StatusBadRequest, "Invalid JSON body")
			return
		}

		var sb strings.Builder
		for _, s := range req {
			cleanURL := strings.ReplaceAll(s.URL, "\ufeff", "")
			cleanName := strings.ReplaceAll(s.Name, "\ufeff", "")

			if !s.Active {
				sb.WriteString("#")
			}

			sb.WriteString(cleanURL)

			if cleanName != "" && cleanName != "未命名" && cleanName != "⏳ 等待自动获取..." && !strings.Contains(cleanName, "等待引擎抓取") {
				sb.WriteString(",主播: " + cleanName)
			}

			sb.WriteString("\n")
		}

		err := os.WriteFile(configPath, []byte(sb.String()), 0644)
		if err != nil {
			log.Printf("[CONTROL][ERR] 无法写入录制配置文件 %s: %v", configPath, err)
			sendJSONError(w, http.StatusInternalServerError, "保存配置文件失败")
			return
		}

		log.Printf("[CONTROL] 🎥 用户更新了直播监控名单，共 %d 条记录已写入物理文件", len(req))

		broadcastWS("streamersData", getStreamersData())
		sendJSONSuccess(w, nil)
		return
	}

	sendJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
}

func getRecorderStatus() string {
	appConfigMu.RLock()
	container := appConfig.RecorderContainer
	appConfigMu.RUnlock()

	if container == "" {
		return "未配置"
	}
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", container).CombinedOutput()
	if err != nil {
		return "离线/异常"
	}
	return strings.TrimSpace(string(out))
}

func handleRecorderStatus(w http.ResponseWriter, r *http.Request) {
	sendJSONSuccess(w, getRecorderStatus())
}

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

		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
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
	return result
}

func handleActiveStreamers(w http.ResponseWriter, r *http.Request) {
	sendJSONSuccess(w, getActiveStreamers())
}

func handleRecorderControl(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Query().Get("action")
	if action != "start" && action != "stop" && action != "restart" {
		sendJSONError(w, http.StatusBadRequest, "非法的控制指令")
		return
	}

	appConfigMu.RLock()
	container := appConfig.RecorderContainer
	appConfigMu.RUnlock()

	log.Printf("[DOCKER] 用户请求执行容器控制: docker %s %s", action, container)
	out, err := exec.Command("docker", action, container).CombinedOutput()

	if err != nil {
		log.Printf("[DOCKER][ERR] 执行失败: %s", string(out))
		sendJSONError(w, http.StatusInternalServerError, "操作失败: "+string(out))
		return
	}

	sendJSONSuccess(w, "操作成功执行")
}

func handleRecorderLogs(w http.ResponseWriter, r *http.Request) {
	appConfigMu.RLock()
	container := appConfig.RecorderContainer
	appConfigMu.RUnlock()

	out, err := exec.Command("docker", "logs", "--tail", "100", container).CombinedOutput()
	if err != nil {
		sendJSONError(w, http.StatusInternalServerError, "获取日志失败: "+string(out))
		return
	}
	sendJSONSuccess(w, string(out))
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	level := query.Get("level")
	keyword := query.Get("keyword")
	page, _ := strconv.Atoi(query.Get("page"))
	limit, _ := strconv.Atoi(query.Get("limit"))

	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 50
	}

	logsMu.RLock()
	filtered := make([]*LogEntry, 0)
	for _, entry := range logs {
		if level != "" && entry.Level != level {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(entry.Message), strings.ToLower(keyword)) {
			continue
		}
		filtered = append(filtered, entry)
	}
	logsMu.RUnlock()

	total := len(filtered)

	end := total - (page-1)*limit
	if end <= 0 {
		sendJSONSuccess(w, map[string]interface{}{"items": []*LogEntry{}, "total": total})
		return
	}

	start := end - limit
	if start < 0 {
		start = 0
	}

	result := filtered[start:end]
	sendJSONSuccess(w, map[string]interface{}{"items": result, "total": total})
}

// ⭐ 恢复函数
func restoreQueueCounts() {
	data, err := os.ReadFile(successLogFile)
	if err != nil {
		return
	}
	var list []UploadRecord
	json.Unmarshal(data, &list)

	// 将历史成功记录的数量同步到 queueSuccess
	queueMu.Lock()
	// 假设我们只恢复成功计数，因为等待和上传中的任务在重启后会重新扫描生成
	for i := 0; i < len(list); i++ {
		queueSuccess = append(queueSuccess, fmt.Sprintf("hist-%d", i))
	}
	queueMu.Unlock()
}

func handleLogsDownload(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	level := query.Get("level")
	keyword := query.Get("keyword")
	exportLimit, _ := strconv.Atoi(query.Get("limit"))

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=logs-%s.log", time.Now().Format("20060102-150405")))

	logsMu.RLock()
	filtered := make([]*LogEntry, 0)
	for _, entry := range logs {
		if level != "" && entry.Level != level {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(entry.Message), strings.ToLower(keyword)) {
			continue
		}
		filtered = append(filtered, entry)
	}
	logsMu.RUnlock()

	if exportLimit > 0 && len(filtered) > exportLimit {
		filtered = filtered[len(filtered)-exportLimit:]
	}

	for _, entry := range filtered {
		fmt.Fprintf(w, "[%s] [%s] %s\n", entry.Time, entry.Level, entry.Message)
		if entry.Error != "" {
			fmt.Fprintf(w, "  Error: %s\n", entry.Error)
		}
	}
	log.Printf("[CONTROL] 📥 用户导出了 %d 条系统日志\n", len(filtered))
}

func logCollector() {
	for entry := range logChan {
		logsMu.Lock()
		logs = append(logs, entry)
		if len(logs) > 1000 {
			logs = logs[1:]
		}
		logsMu.Unlock()

		broadcastWS("newLog", entry)
	}
}

func sendJSONSuccess(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(APIResponse{Code: 200, Message: "success", Data: data})
}

func sendJSONError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(APIResponse{Code: statusCode, Message: message})
}
