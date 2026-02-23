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

	wsUpgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	wsClients   = make(map[*websocket.Conn]bool)
	wsMutex     sync.Mutex
	wsBroadcast = make(chan WSMessage, 100)

	running      = false
	runningMu    sync.RWMutex
	startTime    time.Time
	todayStats   = TodayStats{Success: 0, Fail: 0}
	todayStatsMu sync.RWMutex

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

type Queue struct {
	Waiting   []*Task `json:"waiting"`
	Uploading []*Task `json:"uploading"`
	Success   []*Task `json:"success"`
	Fail      []*Task `json:"fail"`
	Retrying  []*Task `json:"retrying"`
}

type TodayStats struct {
	Success int `json:"success"`
	Fail    int `json:"fail"`
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
	ScanInterval      int      `json:"scanInterval"`
	Workers           int      `json:"workers"`
	DayRate           int      `json:"dayRate"`
	NightRate         int      `json:"nightRate"`
	EmailInterval     int      `json:"emailInterval"`
	Running           bool     `json:"running"`
	AutoRetry         bool     `json:"autoRetry"`
	MaxRetry          int      `json:"maxRetry"`
	EnableLogs        bool     `json:"enableLogs"`
	LogLevel          string   `json:"logLevel"`
	Dirs              []string `json:"dirs"`
	RemoteServer      string   `json:"remoteServer"`
	RemoteUser        string   `json:"remoteUser"`
	RemotePass        string   `json:"remotePass"`
	LiveConfigPath    string   `json:"liveConfigPath"`
	RecorderContainer string   `json:"recorderContainer"`
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

func StartWebServer(port int) {
	runningMu.Lock()
	running = true
	startTime = time.Now()
	runningMu.Unlock()

	mux := http.NewServeMux()

	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/v1/auth/login", handleLogin)
	mux.HandleFunc("/api/v1/auth/logout", handleLogout)
	mux.HandleFunc("/api/v1/status", handleStatus)
	mux.HandleFunc("/api/v1/tasks/live", handleLiveTasks)
	mux.HandleFunc("/api/v1/tasks/history", handleHistory)
	mux.HandleFunc("/api/v1/tasks/queue", handleQueue)

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

	mux.HandleFunc("/api/v1/streamers", handleStreamers)
	mux.HandleFunc("/api/v1/streamers/active", handleActiveStreamers)

	mux.HandleFunc("/api/v1/recorder/status", handleRecorderStatus)
	mux.HandleFunc("/api/v1/recorder/control", handleRecorderControl)
	mux.HandleFunc("/api/v1/recorder/logs", handleRecorderLogs)

	mux.HandleFunc("/ws/live", handleWebSocket)

	go wsBroadcastLoop()
	go logCollector()
	go wsDashboardBroadcaster() // ⭐ 新增：启动基于 WebSocket 的仪表盘状态广播引擎

	addr := fmt.Sprintf(":%d", port)
	log.Printf("[WEB] 控制台已就绪: http://127.0.0.1%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("[WEB] Server error: %v", err)
	}
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

// ⭐ 核心创新：每 3 秒通过 WebSocket 自动推流状态，替代前端的 HTTP 轮询
func wsDashboardBroadcaster() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		wsMutex.Lock()
		clientCount := len(wsClients)
		wsMutex.Unlock()

		// 只有当有前端页面打开时，才进行计算并推送，极致节省性能
		if clientCount > 0 {
			broadcastWS("systemStatus", buildStatusData())
			broadcastWS("queueStatus", buildQueueData())
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

// ⭐ 提取组装状态的数据工厂
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
		"rate":             currentRate(),
		"dayRate":          currentDayRate,
		"nightRate":        currentNightRate,
		"uptime":           int64(time.Since(startTime).Seconds()),
	}
}

// 保留 HTTP 接口兼容首次访问
func handleStatus(w http.ResponseWriter, r *http.Request) {
	sendJSONSuccess(w, buildStatusData())
}

// ⭐ 提取组装队列的数据工厂
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

// 保留 HTTP 接口兼容首次访问
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

func handleStreamers(w http.ResponseWriter, r *http.Request) {
	appConfigMu.RLock()
	configPath := appConfig.LiveConfigPath
	appConfigMu.RUnlock()

	if r.Method == http.MethodGet {
		data, err := os.ReadFile(configPath)
		if err != nil {
			sendJSONSuccess(w, []Streamer{})
			return
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
				name = strings.ReplaceAll(name, "\ufeff", "")
			}

			if name == "未命名" || name == "⏳ 等待自动获取..." {
				name = ""
			}

			streamers = append(streamers, Streamer{URL: url, Name: name, Active: active})
		}
		sendJSONSuccess(w, streamers)
		return
	}

	if r.Method == http.MethodPut {
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
		sendJSONSuccess(w, nil)
		return
	}

	sendJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
}

func handleActiveStreamers(w http.ResponseWriter, r *http.Request) {
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

	sendJSONSuccess(w, result)
}

func handleRecorderStatus(w http.ResponseWriter, r *http.Request) {
	appConfigMu.RLock()
	container := appConfig.RecorderContainer
	appConfigMu.RUnlock()

	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", container).CombinedOutput()
	if err != nil {
		sendJSONError(w, http.StatusInternalServerError, "无法获取状态或容器不存在")
		return
	}
	status := strings.TrimSpace(string(out))
	sendJSONSuccess(w, status)
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
