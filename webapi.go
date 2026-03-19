package main

import (
	"crypto/aes"
	"crypto/cipher"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
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
	"github.com/shirou/gopsutil/v3/disk"    // 新增：用于探测系统磁盘余量
	"github.com/shirou/gopsutil/v3/process" // 新增：用于探测底层录制进程的物理内存
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

	wsClients   = make(map[*WSClient]bool)
	wsMutex     sync.RWMutex
	wsBroadcast = make(chan WSMessage, 100)

	running   = false
	runningMu sync.RWMutex
	startTime time.Time

	dirStatuses   = make(map[string]*DirStatus)
	dirStatusesMu sync.RWMutex

	logs    = make([]*LogEntry, 0, 1000)
	logsMu  sync.RWMutex
	logChan = make(chan *LogEntry, 1000)

	// ==========================================
	// 动态密钥交换中心 (RSA + AES PFS 完美前向保密)
	// ==========================================
	rsaPrivateKey      *rsa.PrivateKey
	rsaPublicKeyBase64 string
	sessionKeys        = make(map[string][]byte) // 存放每个客户端独有的 AES 密钥
	sessionKeysMu      sync.RWMutex
)

// WSClient 增加专属的 AES 密钥字段，实现独立加密广播
type WSClient struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	AESKey []byte
}

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
	MailFrom           string   `json:"mailFrom"`
	MailAuthCode       string   `json:"mailAuthCode"`
	MailTo             string   `json:"mailTo"`
	EnableEncryption   bool     `json:"enableEncryption"` // 决定是否开启通信层的数据安全加密
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

type TrendPoint struct {
	Date  string  `json:"date"`
	Size  float64 `json:"size"`
	Count int     `json:"count"`
}

type StreamerRank struct {
	Name string  `json:"name"`
	Size float64 `json:"size"`
}

type EncryptedRequest struct {
	Encrypted string `json:"encrypted"`
}

// ==========================================
// 核心加解密与 Session 管理逻辑
// ==========================================

// getSessionKey 从请求头或 URL Query 中提取客户端的动态分配 AES 密钥
func getSessionKey(r *http.Request) ([]byte, error) {
	sid := r.Header.Get("X-Session-Id")
	if sid == "" {
		sid = r.URL.Query().Get("session_id")
	}
	if sid == "" {
		return nil, fmt.Errorf("missing session id")
	}
	sessionKeysMu.RLock()
	key, ok := sessionKeys[sid]
	sessionKeysMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("invalid or expired session id")
	}
	return key, nil
}

// encryptPayload 使用客户端专属的动态 AES 密钥进行载荷加密
func encryptPayload(plaintext []byte, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aesGCM.NonceSize())
	if _, err = io.ReadFull(cryptorand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := aesGCM.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decryptPayload 使用客户端专属的动态 AES 密钥进行载荷解密
func decryptPayload(cryptoText string, key []byte) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(cryptoText)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := aesGCM.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("密文格式被破坏或长度不足")
	}
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return aesGCM.Open(nil, nonce, ciphertext, nil)
}

// parseEncryptedRequest 拦截密文请求，并使用动态分配的密钥将其还原为实际业务结构体，支持降级回明文解析
func parseEncryptedRequest(r *http.Request, target interface{}) error {
	appConfigMu.RLock()
	encEnabled := appConfig.EnableEncryption
	appConfigMu.RUnlock()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil // 允许空载荷的 GET 请求通过
	}

	// 如果服务端关闭了加密，直接走标准 JSON 解析逻辑即可
	if !encEnabled {
		return json.Unmarshal(body, target)
	}

	var encReq EncryptedRequest
	if err := json.Unmarshal(body, &encReq); err != nil {
		return fmt.Errorf("拦截器告警：强制要求使用商业级加密格式通信")
	}
	if encReq.Encrypted == "" {
		return fmt.Errorf("拦截器告警：加密载荷缺失")
	}

	key, err := getSessionKey(r)
	if err != nil {
		return fmt.Errorf("Session Invalid: %v", err)
	}

	decryptedData, err := decryptPayload(encReq.Encrypted, key)
	if err != nil {
		return fmt.Errorf("动态安全网关解密失败: %v", err)
	}

	return json.Unmarshal(decryptedData, target)
}

// sendJSONSuccess 统一成功响应，并依据配置决定是否使用对应客户端的临时 AES 密钥对结果进行端到端加密
func sendJSONSuccess(w http.ResponseWriter, r *http.Request, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	resp := APIResponse{Code: 200, Message: "success", Data: data}
	rawJSON, _ := json.Marshal(resp)

	appConfigMu.RLock()
	encEnabled := appConfig.EnableEncryption
	appConfigMu.RUnlock()

	// 降级为明文直接响应
	if !encEnabled {
		w.Write(rawJSON)
		return
	}

	key, err := getSessionKey(r)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":401,"message":"Session Invalid"}`))
		return
	}

	encryptedStr, err := encryptPayload(rawJSON, key)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"code":500,"message":"动态加密服务异常"}`))
		return
	}
	json.NewEncoder(w).Encode(EncryptedRequest{Encrypted: encryptedStr})
}

// sendJSONError 统一错误响应，使用客户端临时 AES 密钥下发加密报错信息
func sendJSONError(w http.ResponseWriter, r *http.Request, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	resp := APIResponse{Code: statusCode, Message: message}
	rawJSON, _ := json.Marshal(resp)

	appConfigMu.RLock()
	encEnabled := appConfig.EnableEncryption
	appConfigMu.RUnlock()

	if !encEnabled {
		w.Write(rawJSON)
		return
	}

	key, err := getSessionKey(r)
	if err != nil {
		w.Write([]byte(fmt.Sprintf(`{"code":%d,"message":"%s"}`, statusCode, message))) // 降级策略
		return
	}

	encryptedStr, err := encryptPayload(rawJSON, key)
	if err != nil {
		w.Write([]byte(fmt.Sprintf(`{"code":%d,"message":"%s"}`, statusCode, message))) // 降级策略
		return
	}
	json.NewEncoder(w).Encode(EncryptedRequest{Encrypted: encryptedStr})
}

// ==========================================
// 密钥交换前置 API
// ==========================================

// handleGetPubKey 前端获取系统的随机 RSA 公钥，并下发当前系统的安全开关状态
func handleGetPubKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	appConfigMu.RLock()
	encEnabled := appConfig.EnableEncryption
	appConfigMu.RUnlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"code": 200,
		"data": map[string]interface{}{
			"pubkey":  rsaPublicKeyBase64,
			"enabled": encEnabled,
		},
	})
}

// handleExchangeKey 前端发送用 RSA 加密过的临时 AES 密钥，后端接管并分发 SessionID
func handleExchangeKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EncKey string `json:"enc_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}

	ciphertext, err := base64.StdEncoding.DecodeString(req.EncKey)
	if err != nil {
		http.Error(w, "invalid base64", 400)
		return
	}

	// 使用 RSA-OAEP 对前端传来的专属 AES 密钥进行解密
	aesKey, err := rsa.DecryptOAEP(sha256.New(), cryptorand.Reader, rsaPrivateKey, ciphertext, nil)
	if err != nil || len(aesKey) != 32 {
		http.Error(w, "decryption failed", 400)
		return
	}

	sessionID := fmt.Sprintf("sess-%d-%d", time.Now().UnixNano(), rand.Intn(1000000))
	sessionKeysMu.Lock()
	sessionKeys[sessionID] = aesKey
	sessionKeysMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"code": 200,
		"data": map[string]string{"session_id": sessionID},
	})
}

// StartWebServer 启动动态安全的 Web 服务器，挂载所有路由端点
func StartWebServer(port int) {
	// 系统启动时动态生成 RSA-2048 密钥对，彻底抛弃硬编码密钥
	priv, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		log.Fatalf("[SEC] 生成 RSA 密钥失败: %v", err)
	}
	rsaPrivateKey = priv
	pubASN1, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		log.Fatalf("[SEC] 导出公钥失败: %v", err)
	}
	rsaPublicKeyBase64 = base64.StdEncoding.EncodeToString(pubASN1)
	log.Println("[SEC] 🛡️ 商业级动态 RSA+AES 混合加密中心已初始化")

	runningMu.Lock()
	restoreQueueCounts()
	running = true
	startTime = time.Now()
	runningMu.Unlock()

	mux := http.NewServeMux()

	// 安全握手前置路由
	mux.HandleFunc("/api/v1/sec/pubkey", handleGetPubKey)
	mux.HandleFunc("/api/v1/sec/exchange", handleExchangeKey)

	// 核心业务路由
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
	mux.HandleFunc("/api/v1/cookies", handleCookies)

	InitBuiltinRecorder(mux)

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

// SendAlert 向前端发送系统弹窗级别的警告通知
func SendAlert(level string, title string, message string) {
	broadcastWS("systemAlert", map[string]interface{}{
		"level":   level,
		"title":   title,
		"message": message,
		"time":    time.Now().Format("15:04:05"),
	})
}

// handleIndex 处理根路径请求，返回前端页面基础结构
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

// buildStatsTrendData 构建统计趋势图表的数据，利用内存缓存加速访问
func buildStatsTrendData() map[string]interface{} {
	successLogMu.Lock()
	list := make([]UploadRecord, len(successRecords))
	copy(list, successRecords)
	successLogMu.Unlock()

	trendMap := make(map[string]*TrendPoint)
	now := time.Now()
	for i := 0; i < 7; i++ {
		d := now.AddDate(0, 0, -i).Format("01-02")
		trendMap[d] = &TrendPoint{Date: d, Size: 0, Count: 0}
	}

	rankMap := make(map[string]int64)

	for _, rec := range list {
		day := rec.Time.Format("01-02")
		if tp, ok := trendMap[day]; ok {
			tp.Size += float64(rec.Size) / 1024 / 1024 / 1024
			tp.Count++
		}
		rankMap[rec.Streamer] += rec.Size
	}

	trendResult := make([]TrendPoint, 0)
	for i := 6; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("01-02")
		trendResult = append(trendResult, *trendMap[d])
	}

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

// wsBroadcastLoop 处理 WebSocket 广播队列，实时向所有连接客户端下发报文(支持明密文切换)
func wsBroadcastLoop() {
	for {
		msg := <-wsBroadcast
		rawBytes, _ := json.Marshal(msg)

		appConfigMu.RLock()
		encEnabled := appConfig.EnableEncryption
		appConfigMu.RUnlock()

		wsMutex.RLock()
		clientsCopy := make([]*WSClient, 0, len(wsClients))
		for client := range wsClients {
			clientsCopy = append(clientsCopy, client)
		}
		wsMutex.RUnlock()

		for _, client := range clientsCopy {
			client.mu.Lock()
			client.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))

			var err error
			// ✨ 核心：根据后台安全配置判断是否进行端到端隔离加密传输
			if encEnabled && client.AESKey != nil {
				encryptedPayload, _ := encryptPayload(rawBytes, client.AESKey)
				err = client.conn.WriteJSON(EncryptedRequest{Encrypted: encryptedPayload})
			} else {
				err = client.conn.WriteJSON(msg)
			}

			client.mu.Unlock()

			if err != nil {
				client.conn.Close()
				wsMutex.Lock()
				delete(wsClients, client)
				wsMutex.Unlock()
			}
		}
	}
}

// wsDashboardBroadcaster 定时向面板广播系统实时状态数据、系统负载及图表
func wsDashboardBroadcaster() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		wsMutex.RLock()
		clientCount := len(wsClients)
		wsMutex.RUnlock()

		if clientCount > 0 {
			broadcastWS("systemStatus", buildStatusData())
			broadcastWS("queueStatus", buildQueueData())

			var totalSpeed int64 = 0
			liveTasksMu.RLock()
			for _, task := range liveTasks {
				if task.Status == "uploading" {
					totalSpeed += task.Speed
				}
			}
			liveTasksMu.RUnlock()

			broadcastWS("trafficMetrics", map[string]interface{}{
				"speed": totalSpeed,
				"time":  time.Now().UnixMilli(),
			})

			broadcastWS("statsTrend", buildStatsTrendData())

			broadcastWS("recorderStatus", getRecorderStatus())
			broadcastWS("activeStreamers", getActiveStreamers())
			broadcastWS("streamersData", getStreamersData())

			broadcastWS("builtinTasks", GetBuiltinRecorderTasks())
		}
	}
}

// broadcastWS 将指定类型的消息压入广播队列，待加密发送给各个前端页面
func broadcastWS(msgType string, payload interface{}) {
	select {
	case wsBroadcast <- WSMessage{Type: msgType, Payload: payload}:
	default:
	}
}

// handleWebSocket 处理 WebSocket 升级及接收逻辑，增加明密文双模控制支持
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	appConfigMu.RLock()
	encEnabled := appConfig.EnableEncryption
	appConfigMu.RUnlock()

	var key []byte
	var err error

	// 若开启加密，要求具备合法的 SessionID 以获得协商好的密钥
	if encEnabled {
		key, err = getSessionKey(r)
		if err != nil {
			http.Error(w, "Unauthorized Session", 401)
			return
		}
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	client := &WSClient{conn: conn, AESKey: key}

	wsMutex.Lock()
	wsClients[client] = true
	wsMutex.Unlock()

	defer func() {
		client.conn.Close()
		wsMutex.Lock()
		delete(wsClients, client)
		wsMutex.Unlock()
	}()

	for {
		var msg WSMessage

		if encEnabled {
			var encMsg EncryptedRequest
			err := client.conn.ReadJSON(&encMsg)
			if err != nil {
				break
			}

			// 商业级网络入口处立刻使用客户端专属密钥解密报文
			decryptedBytes, err := decryptPayload(encMsg.Encrypted, client.AESKey)
			if err != nil {
				log.Printf("[WS][ERR] WebSocket 密文非法或解密失败: %v", err)
				continue
			}

			if err := json.Unmarshal(decryptedBytes, &msg); err != nil {
				continue
			}
		} else {
			// 明文降级处理
			err := client.conn.ReadJSON(&msg)
			if err != nil {
				break
			}
		}

		if msg.Type == "ping" {
			pongRaw, _ := json.Marshal(WSMessage{Type: "pong", Payload: msg.Payload})

			client.mu.Lock()
			client.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))

			if encEnabled && client.AESKey != nil {
				pongEnc, _ := encryptPayload(pongRaw, client.AESKey)
				_ = client.conn.WriteJSON(EncryptedRequest{Encrypted: pongEnc})
			} else {
				_ = client.conn.WriteMessage(websocket.TextMessage, pongRaw)
			}
			client.mu.Unlock()
		}
	}
}

// handleLogin 处理登录请求，从解密体中验证凭据并下发 Token
func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSONError(w, r, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		OTP      string `json:"otp"`
	}
	if err := parseEncryptedRequest(r, &req); err != nil {
		sendJSONError(w, r, http.StatusBadRequest, "无法解析加密的凭据载荷")
		return
	}

	if req.Username != dashboardUsername || req.Password != dashboardPassword {
		sendJSONError(w, r, http.StatusUnauthorized, "Invalid credentials")
		return
	}
	expiry := time.Now().Add(24 * time.Hour).UnixMilli()
	if token == "" {
		token = "test-token-" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	sendJSONSuccess(w, r, map[string]interface{}{"token": token, "expiry": expiry})
}

// handleLogout 处理注销请求，清除登录状态
func handleLogout(w http.ResponseWriter, r *http.Request) {
	sendJSONSuccess(w, r, nil)
}

// buildStatusData 构建系统运行参数与目录状态聚合的大宽表
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

	// ===== 核心修改：增加内存与磁盘余量采集 =====
	var diskFree int64 = 0
	var ffmpegMem int64 = 0

	// 探测磁盘可用空间 (默认检查当前系统所在目录，若配置了多目录则以第一个监控目录所在磁盘为准)
	targetDir := "."
	if len(configuredDirs) > 0 && configuredDirs[0] != "" {
		targetDir = configuredDirs[0]
	}
	if usage, err := disk.Usage(targetDir); err == nil {
		diskFree = int64(usage.Free)
	}

	// 探测录制进程 (ffmpeg) 占用的物理内存 RSS
	if procs, err := process.Processes(); err == nil {
		for _, p := range procs {
			if name, err := p.Name(); err == nil && strings.Contains(strings.ToLower(name), "ffmpeg") {
				if memInfo, err := p.MemoryInfo(); err == nil {
					ffmpegMem += int64(memInfo.RSS)
				}
			}
		}
	}
	// ===========================================

	return map[string]interface{}{
		"running":          isRunning,
		"tokenValid":       token != "",
		"workers":          currentWorkers,
		"dirs":             dirs,
		"scanningInterval": currentScanInterval,
		"dynamicInterval":  dynInterval,
		"nextScanTime":     nextScan,
		"rate":             currentRate(),
		"dayRate":          currentDayRate,
		"nightRate":        currentNightRate,
		"uptime":           int64(time.Since(startTime).Seconds()),
		"diskFree":         diskFree,  // 新增磁盘剩余字段
		"ffmpegMem":        ffmpegMem, // 新增内存占用字段
	}
}

// handleStatus 返回当前的系统聚合运行状态
func handleStatus(w http.ResponseWriter, r *http.Request) {
	sendJSONSuccess(w, r, buildStatusData())
}

// buildQueueData 统计并构建各个任务队列的数字分布
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

// handleQueue 返回当前各个任务队列的长度
func handleQueue(w http.ResponseWriter, r *http.Request) {
	sendJSONSuccess(w, r, buildQueueData())
}

// handleLiveTasks 响应查询实时进行中的上传任务
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
	sendJSONSuccess(w, r, tasks)
}

// handleHistory 响应查询带分页与筛选条件的上传历史记录表
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
		sendJSONSuccess(w, r, map[string]interface{}{"items": []*HistoryRecord{}, "total": total})
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

	sendJSONSuccess(w, r, map[string]interface{}{"items": items, "total": total})
}

// handleControlStart 响应用户启动系统的控制指令
func handleControlStart(w http.ResponseWriter, r *http.Request) {
	runningMu.Lock()
	running = true
	startTime = time.Now()
	runningMu.Unlock()

	log.Println("[CONTROL] 🚀 用户下发指令：启动系统，恢复扫描与上传任务")
	triggerScan("start")
	sendJSONSuccess(w, r, nil)
}

// handleControlPause 响应用户暂停系统的控制指令
func handleControlPause(w http.ResponseWriter, r *http.Request) {
	runningMu.Lock()
	running = false
	runningMu.Unlock()

	log.Println("[CONTROL] ⏸️ 用户下发指令：暂停系统运行")
	sendJSONSuccess(w, r, nil)
}

// handleControlStop 响应用户停止系统的控制指令
func handleControlStop(w http.ResponseWriter, r *http.Request) {
	runningMu.Lock()
	running = false
	runningMu.Unlock()

	log.Println("[CONTROL] 🛑 用户下发指令：停止系统运行")
	sendJSONSuccess(w, r, nil)
}

// handleControlRelogin 响应用户强制刷新远端 Token 的指令
func handleControlRelogin(w http.ResponseWriter, r *http.Request) {
	if err := login(); err != nil {
		log.Println("[CONTROL][ERR] 用户尝试刷新远端授权失败:", err)
		sendJSONError(w, r, http.StatusInternalServerError, "Relogin failed")
		return
	}
	log.Println("[CONTROL] 🔑 用户下发指令：远端授权凭证已成功刷新")
	sendJSONSuccess(w, r, nil)
}

// handleControlRescan 响应用户强制触发全量目录扫描的指令
func handleControlRescan(w http.ResponseWriter, r *http.Request) {
	log.Println("[CONTROL] 🔍 用户下发指令：手动触发深度目录重新扫描")
	triggerScan("rescan")
	sendJSONSuccess(w, r, map[string]interface{}{"message": "重新扫描已触发"})
}

// handleControlClearFailQueue 响应用户清空失败队列的指令
func handleControlClearFailQueue(w http.ResponseWriter, r *http.Request) {
	queueMu.Lock()
	queueFail = make([]string, 0)
	queueMu.Unlock()
	log.Println("[CONTROL] 🧹 用户下发指令：已清空失败任务队列")
	sendJSONSuccess(w, r, nil)
}

// handleControlRetryFailQueue 响应用户重试全部失败任务的指令
func handleControlRetryFailQueue(w http.ResponseWriter, r *http.Request) {
	queueMu.Lock()
	for _, taskID := range queueFail {
		queueWaiting = append(queueWaiting, taskID)
	}
	queueFail = make([]string, 0)
	queueMu.Unlock()
	log.Println("[CONTROL] 🔄 用户下发指令：失败任务已全部压入等待队列准备重试")

	// 核心修复：向引擎发送重扫指令，让 Worker 真正去把硬盘里残留的失败文件捡起来
	triggerScan("rescan")

	sendJSONSuccess(w, r, nil)
}

// handleControlClearSuccessQueue 响应用户清空成功展示历史的指令
func handleControlClearSuccessQueue(w http.ResponseWriter, r *http.Request) {
	queueMu.Lock()
	queueSuccess = make([]string, 0)
	queueMu.Unlock()
	log.Println("[CONTROL] 🧹 用户下发指令：已清空成功任务队列展示历史")
	sendJSONSuccess(w, r, nil)
}

// handleDirsStatus 返回所有受监控目录及其内部文件的统计快照
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

	sendJSONSuccess(w, r, statuses)
}

// handleConfig 处理读取(GET)或全量写入(PUT)系统核心配置项的请求
func handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		appConfigMu.RLock()
		currentConfig := appConfig
		appConfigMu.RUnlock()
		sendJSONSuccess(w, r, currentConfig)
	case http.MethodPut:
		var newConfig Config
		if err := parseEncryptedRequest(r, &newConfig); err != nil {
			sendJSONError(w, r, http.StatusBadRequest, "非法配置实体或解密异常")
			return
		}
		appConfigMu.Lock()
		appConfig = newConfig
		appConfigMu.Unlock()

		saveConfigToFile()

		log.Printf("[CONTROL] ⚙️ 用户保存了新配置，目标扫描目录已变更为: [%s]，加密模式: %v", strings.Join(newConfig.Dirs, " | "), newConfig.EnableEncryption)

		triggerScan("config-update")
		triggerReportReset()

		sendJSONSuccess(w, r, nil)
	default:
		sendJSONError(w, r, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// handleCookies 处理读取或修改外部录制引擎中 Cookie 等凭据文件的请求
func handleCookies(w http.ResponseWriter, r *http.Request) {
	appConfigMu.RLock()
	configPath := appConfig.RecorderConfigPath
	appConfigMu.RUnlock()

	if configPath == "" {
		sendJSONError(w, r, http.StatusBadRequest, "尚未配置录制引擎主配置文件路径 (config.ini)")
		return
	}

	if r.Method == http.MethodGet {
		data, err := os.ReadFile(configPath)
		if err != nil {
			sendJSONSuccess(w, r, map[string]string{"douyin": "", "kuaishou": "", "sooplive": "", "liveSavePath": ""})
			return
		}

		res := map[string]string{"douyin": "", "kuaishou": "", "sooplive": "", "liveSavePath": ""}
		lines := strings.Split(string(data), "\n")
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
			} else if strings.HasPrefix(trimmed, "live_path") {
				parts := strings.SplitN(trimmed, "=", 2)
				if len(parts) == 2 {
					res["liveSavePath"] = strings.TrimSpace(parts[1])
				}
			}
		}

		sendJSONSuccess(w, r, res)
		return
	}

	if r.Method == http.MethodPut {
		var req struct {
			Douyin       string `json:"douyin"`
			Kuaishou     string `json:"kuaishou"`
			Sooplive     string `json:"sooplive"`
			LiveSavePath string `json:"liveSavePath"`
		}
		if err := parseEncryptedRequest(r, &req); err != nil {
			sendJSONError(w, r, http.StatusBadRequest, "加密层校验失败")
			return
		}

		data, err := os.ReadFile(configPath)
		if err != nil {
			sendJSONError(w, r, http.StatusInternalServerError, "读取配置文件失败: "+err.Error())
			return
		}

		lines := strings.Split(string(data), "\n")
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
			} else if strings.HasPrefix(trimmed, "live_path") {
				lines[i] = "live_path=" + req.LiveSavePath
				livePathFound = true
			}
		}

		insertAfterCookie := func(key, val string) {
			for i, line := range lines {
				if strings.TrimSpace(line) == "[cookie]" {
					lines = append(lines[:i+1], append([]string{key + "=" + val}, lines[i+1:]...)...)
					return
				}
			}
			lines = append(lines, "", "[cookie]", key+"="+val)
		}

		insertAfterBase := func(key, val string) {
			for i, line := range lines {
				if strings.TrimSpace(line) == "[base]" {
					lines = append(lines[:i+1], append([]string{key + "=" + val}, lines[i+1:]...)...)
					return
				}
			}
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
			insertAfterBase("live_path", req.LiveSavePath)
		}

		err = os.WriteFile(configPath, []byte(strings.Join(lines, "\n")), 0644)
		if err != nil {
			log.Printf("[CONTROL][ERR] 无法写入配置文件 %s: %v", configPath, err)
			sendJSONError(w, r, http.StatusInternalServerError, "保存配置失败: "+err.Error())
			return
		}

		log.Printf("[CONTROL] 🍪 用户在网页端成功更新了主配置文件 %s", configPath)
		sendJSONSuccess(w, r, nil)
		return
	}

	sendJSONError(w, r, http.StatusMethodNotAllowed, "Method not allowed")
}

// getStreamersData 读取并解析直播录制名单文件内容，还原为结构体数组
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

// handleStreamers 处理读取(GET)或全量更新(PUT/POST)直播监控名单的请求
func handleStreamers(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		sendJSONSuccess(w, r, getStreamersData())
		return
	}

	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		appConfigMu.RLock()
		configPath := appConfig.LiveConfigPath
		appConfigMu.RUnlock()

		var req []Streamer
		if err := parseEncryptedRequest(r, &req); err != nil {
			sendJSONError(w, r, http.StatusBadRequest, "解析密文体异常")
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
			sendJSONError(w, r, http.StatusInternalServerError, "保存配置文件失败")
			return
		}

		log.Printf("[CONTROL] 🎥 用户更新了直播监控名单，共 %d 条记录已写入物理文件", len(req))

		broadcastWS("streamersData", getStreamersData())
		sendJSONSuccess(w, r, nil)
		return
	}

	sendJSONError(w, r, http.StatusMethodNotAllowed, "Method not allowed")
}

// getRecorderStatus 通过调用底层 Shell 获取外部 Docker 录制引擎的运行状态
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

// handleRecorderStatus 响应查询外部 Docker 引擎健康与运行状态的请求
func handleRecorderStatus(w http.ResponseWriter, r *http.Request) {
	sendJSONSuccess(w, r, getRecorderStatus())
}

// getActiveStreamers 通过探测各目录内是否存在时间较新的文件，推断当前正在处于写入(活跃录制)状态的主播名单
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

// handleActiveStreamers 响应查询正在活跃录制的主播列表接口
func handleActiveStreamers(w http.ResponseWriter, r *http.Request) {
	sendJSONSuccess(w, r, getActiveStreamers())
}

// handleRecorderControl 执行对宿主机底层外部 Docker 容器的启动、停止、重启指令操作
func handleRecorderControl(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Query().Get("action")
	if action != "start" && action != "stop" && action != "restart" {
		sendJSONError(w, r, http.StatusBadRequest, "非法的控制指令")
		return
	}

	appConfigMu.RLock()
	container := appConfig.RecorderContainer
	appConfigMu.RUnlock()

	log.Printf("[DOCKER] 用户请求执行容器控制: docker %s %s", action, container)
	out, err := exec.Command("docker", action, container).CombinedOutput()

	if err != nil {
		log.Printf("[DOCKER][ERR] 执行失败: %s", string(out))
		sendJSONError(w, r, http.StatusInternalServerError, "操作失败: "+string(out))
		return
	}

	sendJSONSuccess(w, r, "操作成功执行")
}

// handleRecorderLogs 调用 Docker 指令拉取外部容器尾部的 100 行日志供调试使用
func handleRecorderLogs(w http.ResponseWriter, r *http.Request) {
	appConfigMu.RLock()
	container := appConfig.RecorderContainer
	appConfigMu.RUnlock()

	out, err := exec.Command("docker", "logs", "--tail", "100", container).CombinedOutput()
	if err != nil {
		sendJSONError(w, r, http.StatusInternalServerError, "获取日志失败: "+string(out))
		return
	}
	sendJSONSuccess(w, r, string(out))
}

// handleLogs 提供带分页参数、等级筛选以及关键字搜索的应用层日志查询视图接口
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
		sendJSONSuccess(w, r, map[string]interface{}{"items": []*LogEntry{}, "total": total})
		return
	}

	start := end - limit
	if start < 0 {
		start = 0
	}

	result := filtered[start:end]
	sendJSONSuccess(w, r, map[string]interface{}{"items": result, "total": total})
}

// restoreQueueCounts 系统启动时通过对内存内日志的复用来还原历史排队数，彻底杜绝 I/O 开销
func restoreQueueCounts() {
	successLogMu.Lock()
	listLen := len(successRecords)
	successLogMu.Unlock()

	queueMu.Lock()
	for i := 0; i < listLen; i++ {
		queueSuccess = append(queueSuccess, fmt.Sprintf("hist-%d", i))
	}
	queueMu.Unlock()
}

// handleLogsDownload 组装导出的文本系统运行日志，并判断是否需要使用 Base64+AES 进行加密导出
func handleLogsDownload(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	level := query.Get("level")
	keyword := query.Get("keyword")
	exportLimit, _ := strconv.Atoi(query.Get("limit"))

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=logs-%s.txt", time.Now().Format("20060102-150405")))

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

	var sb strings.Builder
	for _, entry := range filtered {
		sb.WriteString(fmt.Sprintf("[%s] [%s] %s\n", entry.Time, entry.Level, entry.Message))
		if entry.Error != "" {
			sb.WriteString(fmt.Sprintf("  Error: %s\n", entry.Error))
		}
	}

	appConfigMu.RLock()
	encEnabled := appConfig.EnableEncryption
	appConfigMu.RUnlock()

	if !encEnabled {
		w.Write([]byte(sb.String()))
		log.Printf("[CONTROL] 📥 用户导出了 %d 条明文系统日志", len(filtered))
		return
	}

	// 安全获取当前请求客户端的私有 AES 密钥
	key, err := getSessionKey(r)
	if err != nil {
		http.Error(w, "Unauthorized Session", 401)
		return
	}

	encryptedLogData, _ := encryptPayload([]byte(sb.String()), key)
	w.Write([]byte(encryptedLogData))

	log.Printf("[CONTROL] 📥 用户导出了 %d 条系统日志，为确保安全已在文件层全链路加密处理\n", len(filtered))
}

// logCollector 后台独立长时运行协程，将 Channel 内部抛出的各级系统日志合并缓存后广播至 WebSocket 流
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
