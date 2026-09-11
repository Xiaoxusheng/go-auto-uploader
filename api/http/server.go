// Package httpapi 控制台 HTTP 服务：路由装配 + 依赖注入的 Handler。
package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"upload/internal/app"
	"upload/internal/auth"
	"upload/internal/cryptox"
	"upload/internal/recorder"
	"upload/internal/storage"
)

const (
	authSessionTTL = 24 * time.Hour
	maxKeyPoolSize = 4096
)

// Options 启动注入项。
type Options struct {
	IndexHTML string
	// Extra 挂载内置录制等扩展路由
	Extra func(mux *http.ServeMux)
}

// Server 控制台服务实例。
type Server struct {
	indexHTML string
	extra     func(mux *http.ServeMux)

	wsUpgrader   websocket.Upgrader
	sessionKeys  *cryptox.SessionStore
	rsaKeyPair   *cryptox.RSAKeyPair
	authSessions *auth.SessionStore
	docker       *recorder.DockerController
	cachedDisk   int64
	cachedFFMem  int64
	sysStatsMu   sync.RWMutex
	maxKeyPoolSz int
}

// New 创建服务。
func New(opts Options) *Server {
	return &Server{
		indexHTML: opts.IndexHTML,
		extra:     opts.Extra,
		wsUpgrader: websocket.Upgrader{
			CheckOrigin:     func(r *http.Request) bool { return true },
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
		},
		sessionKeys:  cryptox.NewSessionStore(maxKeyPoolSize),
		authSessions: auth.NewSessionStore(),
		docker: &recorder.DockerController{
			ContainerNameFn: func() string { return app.AppCfg().RecorderContainer },
		},
	}
}

// AuthSessions 暴露会话库（测试用）。
func (s *Server) AuthSessions() *auth.SessionStore { return s.authSessions }

// IssueToken 签发登录令牌。
func (s *Server) IssueToken() string { return s.authSessions.Issue() }

// Middleware 鉴权中间件。
func (s *Server) Middleware(next http.Handler) http.Handler {
	return auth.Middleware(s.authSessions, next)
}

// Start 启动 HTTP 服务（阻塞）。
func (s *Server) Start(port int) error {
	kp, err := cryptox.GenerateRSAKeyPair()
	if err != nil {
		return fmt.Errorf("生成 RSA 密钥失败: %w", err)
	}
	s.rsaKeyPair = kp
	log.Println("[SEC] 🛡️ 商业级动态 RSA+AES 混合加密中心已初始化")

	restoreQueueCounts()
	app.SetRunning(true)
	app.SetStartTime(time.Now())

	mux := http.NewServeMux()
	s.Register(mux)

	go s.sysStatsCollector()
	go wsBroadcastLoop()
	go logCollector()
	go s.wsDashboardBroadcaster()

	addr := fmt.Sprintf(":%d", port)
	log.Printf("[WEB] 控制台已就绪: http://127.0.0.1%s", addr)
	return http.ListenAndServe(addr, s.Middleware(mux))
}

// Register 挂载全部业务路由。
func (s *Server) Register(mux *http.ServeMux) {
	Register(mux, Routes{
		Index: s.handleIndex, Login: s.handleLogin, Logout: s.handleLogout,
		PubKey: s.handleGetPubKey, Exchange: s.handleExchangeKey,
		Status: s.handleStatus, LiveTasks: s.handleLiveTasks, History: s.handleHistory, Queue: s.handleQueue,
		CtlStart: s.handleControlStart, CtlPause: s.handleControlPause, CtlStop: s.handleControlStop,
		CtlRelogin: s.handleControlRelogin, CtlRescan: s.handleControlRescan,
		CtlClearFail: s.handleControlClearFailQueue, CtlRetryFail: s.handleControlRetryFailQueue,
		CtlClearSuccess: s.handleControlClearSuccessQueue,
		DirsStatus:      s.handleDirsStatus, Config: s.handleConfig,
		Logs: s.handleLogs, LogsDownload: s.handleLogsDownload,
		Streamers: s.handleStreamers, ActiveStreamers: s.handleActiveStreamers, Cookies: s.handleCookies,
		RecorderStatus: s.handleRecorderStatus, RecorderControl: s.handleRecorderControl, RecorderLogs: s.handleRecorderLogs,
		WebSocket: s.handleWebSocket,
		Extra:     s.extra,
	})
}

// ---------- crypto helpers ----------

func (s *Server) sessionKey(r *http.Request) ([]byte, error) {
	return s.sessionKeys.SessionKeyFromRequest(r)
}

func encryptPayload(plaintext []byte, key []byte) (string, error) {
	return cryptox.Encrypt(plaintext, key)
}

func decryptPayload(cryptoText string, key []byte) ([]byte, error) {
	return cryptox.Decrypt(cryptoText, key)
}

func (s *Server) ensureRSAKeyPair() *cryptox.RSAKeyPair {
	if s.rsaKeyPair != nil {
		return s.rsaKeyPair
	}
	kp, err := cryptox.GenerateRSAKeyPair()
	if err != nil {
		log.Printf("[SEC] 生成 RSA 密钥失败: %v", err)
		return nil
	}
	s.rsaKeyPair = kp
	return kp
}

func (s *Server) parseEncryptedRequest(r *http.Request, target interface{}) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	if !app.AppCfg().EnableEncryption {
		return json.Unmarshal(body, target)
	}
	var encReq cryptox.Envelope
	if err := json.Unmarshal(body, &encReq); err != nil {
		return fmt.Errorf("拦截器告警：强制要求使用商业级加密格式通信")
	}
	if encReq.Encrypted == "" {
		return fmt.Errorf("拦截器告警：加密载荷缺失")
	}
	key, err := s.sessionKey(r)
	if err != nil {
		return fmt.Errorf("Session Invalid: %v", err)
	}
	decrypted, err := decryptPayload(encReq.Encrypted, key)
	if err != nil {
		return fmt.Errorf("动态安全网关解密失败: %v", err)
	}
	return json.Unmarshal(decrypted, target)
}

type apiResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

func (s *Server) sendJSONSuccess(w http.ResponseWriter, r *http.Request, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	raw, _ := json.Marshal(apiResponse{Code: 200, Message: "success", Data: data})
	if !app.AppCfg().EnableEncryption {
		_, _ = w.Write(raw)
		return
	}
	key, err := s.sessionKey(r)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"message":"Session Invalid"}`))
		return
	}
	enc, err := encryptPayload(raw, key)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"message":"动态加密服务异常"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(cryptox.Envelope{Encrypted: enc})
}

func (s *Server) sendJSONError(w http.ResponseWriter, r *http.Request, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	raw, _ := json.Marshal(apiResponse{Code: statusCode, Message: message})
	if !app.AppCfg().EnableEncryption {
		_, _ = w.Write(raw)
		return
	}
	key, err := s.sessionKey(r)
	if err != nil {
		_, _ = w.Write([]byte(fmt.Sprintf(`{"code":%d,"message":"%s"}`, statusCode, message)))
		return
	}
	enc, err := encryptPayload(raw, key)
	if err != nil {
		_, _ = w.Write([]byte(fmt.Sprintf(`{"code":%d,"message":"%s"}`, statusCode, message)))
		return
	}
	_ = json.NewEncoder(w).Encode(cryptox.Envelope{Encrypted: enc})
}

// ---------- sys stats ----------

func getDiskFreeSpaceStd(pathStr string) int64 {
	if pathStr == "" {
		pathStr = "."
	}
	absPath, err := filepath.Abs(pathStr)
	if err != nil {
		absPath = pathStr
	}
	if runtime.GOOS == "windows" {
		vol := filepath.VolumeName(absPath)
		if vol == "" {
			vol = "C:"
		}
		out, err := exec.Command("wmic", "logicaldisk", "where", fmt.Sprintf("DeviceID='%s'", vol), "get", "FreeSpace").Output()
		if err == nil {
			lines := strings.Split(string(out), "\n")
			if len(lines) >= 2 {
				if freeBytes, err := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64); err == nil {
					return freeBytes
				}
			}
		}
		return 0
	}
	out, err := exec.Command("df", "-k", absPath).Output()
	if err == nil {
		lines := strings.Split(string(out), "\n")
		if len(lines) >= 2 {
			fields := strings.Fields(lines[1])
			if len(fields) >= 4 {
				if freeKb, err := strconv.ParseInt(fields[3], 10, 64); err == nil {
					return freeKb * 1024
				}
			}
		}
	}
	return 0
}

func getFFmpegMemoryStd() int64 {
	var totalMem int64
	if runtime.GOOS == "windows" {
		out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq ffmpeg.exe", "/FO", "CSV", "/NH").Output()
		if err != nil {
			return 0
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.Contains(line, "INFO:") {
				continue
			}
			parts := strings.Split(line, "\",\"")
			if len(parts) >= 5 {
				memStr := strings.NewReplacer(`"`, "", ",", "", " K", "", " KB", "").Replace(parts[4])
				if memKb, err := strconv.ParseInt(strings.TrimSpace(memStr), 10, 64); err == nil {
					totalMem += memKb * 1024
				}
			}
		}
		return totalMem
	}
	out, err := exec.Command("ps", "-eo", "comm,rss").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.Contains(strings.ToLower(fields[0]), "ffmpeg") {
			if memKb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				totalMem += memKb * 1024
			}
		}
	}
	return totalMem
}

func (s *Server) sysStatsCollector() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	update := func() {
		dirs := app.AppCfg().Dirs
		target := "."
		if len(dirs) > 0 && dirs[0] != "" {
			target = dirs[0]
		}
		df := getDiskFreeSpaceStd(target)
		mem := getFFmpegMemoryStd()
		s.sysStatsMu.Lock()
		s.cachedDisk = df
		s.cachedFFMem = mem
		s.sysStatsMu.Unlock()
	}
	update()
	for range ticker.C {
		update()
	}
}

// ---------- ws loops ----------

func wsBroadcastLoop() {
	stop := make(chan struct{})
	if app.AppCtx != nil {
		go func() { <-app.AppCtx.Done(); close(stop) }()
	}
	if app.WSHub != nil {
		app.WSHub.Run(stop)
	}
}

func logCollector() {
	for entry := range app.AppLogs.Chan() {
		app.AppLogs.Append(entry)
		app.BroadcastWS("newLog", entry)
	}
}

type trendPointRes struct {
	Date  string  `json:"date"`
	Size  float64 `json:"size"`
	Count int     `json:"count"`
}

type streamerRank struct {
	Name string  `json:"name"`
	Size float64 `json:"size"`
}

func buildStatsTrendData() map[string]interface{} {
	now := time.Now()
	trendByDate := map[string]storage.TrendPointDTO{}
	for _, tp := range app.SuccessStore.TrendSnapshot() {
		trendByDate[tp.Date] = tp
	}
	trendResult := make([]trendPointRes, 0)
	for i := 6; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("01-02")
		if tp, exists := trendByDate[d]; exists {
			trendResult = append(trendResult, trendPointRes{Date: tp.Date, Size: tp.Size, Count: tp.Count})
		} else {
			trendResult = append(trendResult, trendPointRes{Date: d})
		}
	}
	rankResult := make([]streamerRank, 0)
	for _, p := range app.SuccessStore.RankTop(5) {
		rankResult = append(rankResult, streamerRank{
			Name: p[0].(string),
			Size: float64(p[1].(int64)) / 1024 / 1024 / 1024,
		})
	}
	return map[string]interface{}{"trend": trendResult, "rank": rankResult}
}

func (s *Server) wsDashboardBroadcaster() {
	fast := time.NewTicker(2 * time.Second)
	slow := time.NewTicker(10 * time.Second)
	defer fast.Stop()
	defer slow.Stop()

	cachedStreamers := getStreamersData()
	cachedActive := app.GetActiveStreamers()
	cachedTrend := buildStatsTrendData()
	cachedRec := s.docker.Status()

	for {
		select {
		case <-fast.C:
			if app.WSHub == nil || app.WSHub.ClientCount() == 0 {
				continue
			}
			app.BroadcastWS("systemStatus", s.buildStatusData())
			app.BroadcastWS("queueStatus", buildQueueData())
			var totalSpeed int64
			app.LiveTasks.Range(func(_, value interface{}) bool {
				task := value.(*app.Task)
				task.Mu.RLock()
				if task.Status == "uploading" {
					totalSpeed += task.Speed
				}
				task.Mu.RUnlock()
				return true
			})
			app.BroadcastWS("trafficMetrics", map[string]interface{}{"speed": totalSpeed, "time": time.Now().UnixMilli()})
			app.BroadcastWS("statsTrend", cachedTrend)
			app.BroadcastWS("recorderStatus", cachedRec)
			app.BroadcastWS("activeStreamers", cachedActive)
			app.BroadcastWS("streamersData", cachedStreamers)
			app.BroadcastWS("builtinTasks", recorder.Tasks())
		case <-slow.C:
			cachedStreamers = getStreamersData()
			cachedActive = app.GetActiveStreamers()
			cachedTrend = buildStatsTrendData()
			cachedRec = s.docker.Status()
		}
	}
}

// ---------- status builders ----------

func (s *Server) buildStatusData() map[string]interface{} {
	cfg := app.AppCfg()
	tokenValid := app.RemoteCli != nil && app.RemoteCli.Token() != ""
	dyn := atomic.LoadInt64(&app.DynInterval)
	if dyn == 0 {
		dyn = int64(cfg.ScanInterval)
	}
	dirs := make([]map[string]interface{}, 0)
	for _, dir := range cfg.Dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if status, exists := app.DirStatusStore.Get(dir); exists {
			status.Mu.RLock()
			dirs = append(dirs, map[string]interface{}{
				"path": status.Path, "totalFiles": status.TotalFiles, "uploadedFiles": status.UploadedFiles,
				"pendingFiles": status.PendingFiles, "totalSize": status.TotalSize,
				"uploadedSize": status.UploadedSize, "lastScanTime": status.LastScanTime,
			})
			status.Mu.RUnlock()
		} else {
			dirs = append(dirs, map[string]interface{}{"path": dir, "lastScanTime": time.Now().UnixMilli()})
		}
	}
	s.sysStatsMu.RLock()
	diskFree, ffmpegMem := s.cachedDisk, s.cachedFFMem
	s.sysStatsMu.RUnlock()
	return map[string]interface{}{
		"running": app.IsRunning(), "tokenValid": tokenValid, "workers": cfg.Workers, "dirs": dirs,
		"scanningInterval": cfg.ScanInterval, "dynamicInterval": dyn,
		"nextScanTime": atomic.LoadInt64(&app.NextScanUnix),
		"rate":         app.CurrentRate(), "dayRate": cfg.DayRate, "nightRate": cfg.NightRate,
		"uptime": app.UptimeSeconds(), "diskFree": diskFree, "ffmpegMem": ffmpegMem,
	}
}

func buildQueueData() map[string]interface{} {
	return map[string]interface{}{
		"waiting":   atomic.LoadInt64(&app.QueueCount),
		"uploading": atomic.LoadInt64(&app.QueueUploadingCount),
		"success":   atomic.LoadInt64(&app.QueueSuccessCount),
		"failed":    atomic.LoadInt64(&app.QueueFailCount),
		"retrying":  atomic.LoadInt64(&app.QueueRetryingCount),
	}
}

func restoreQueueCounts() {
	waiting := app.TaskQueue.Pending()
	var uploading, success, failed, retrying int64
	app.QueueUploading.Range(func(_, _ interface{}) bool { uploading++; return true })
	app.QueueSuccess.Range(func(_, _ interface{}) bool { success++; return true })
	app.QueueFail.Range(func(_, _ interface{}) bool { failed++; return true })
	app.QueueRetrying.Range(func(_, _ interface{}) bool { retrying++; return true })
	atomic.StoreInt64(&app.QueueCount, waiting)
	atomic.StoreInt64(&app.QueueUploadingCount, uploading)
	atomic.StoreInt64(&app.QueueSuccessCount, success)
	atomic.StoreInt64(&app.QueueFailCount, failed)
	atomic.StoreInt64(&app.QueueRetryingCount, retrying)
}

// ParseEncryptedRequest 解析加密请求（供 main hooks 包装）。
func (s *Server) ParseEncryptedRequest(r *http.Request, target interface{}) error {
	return s.parseEncryptedRequest(r, target)
}

// SendJSONSuccess 成功响应（供 hooks）。
func (s *Server) SendJSONSuccess(w http.ResponseWriter, r *http.Request, data interface{}) {
	s.sendJSONSuccess(w, r, data)
}

// SendJSONError 错误响应（供 hooks）。
func (s *Server) SendJSONError(w http.ResponseWriter, r *http.Request, statusCode int, message string) {
	s.sendJSONError(w, r, statusCode, message)
}

// BuildStatusData 构建系统状态大宽表（bots/测试用）。
func (s *Server) BuildStatusData() map[string]interface{} { return s.buildStatusData() }

// BuildQueueData 队列计数快照。
func BuildQueueData() map[string]interface{} { return buildQueueData() }

// DiskFreeSpaceStd 导出磁盘剩余空间探测（测试/工具）。
func DiskFreeSpaceStd(path string) int64 { return getDiskFreeSpaceStd(path) }

// EncryptPayload / DecryptPayload / RestoreQueueCounts 导出供测试。
func EncryptPayload(plaintext []byte, key []byte) (string, error) {
	return encryptPayload(plaintext, key)
}

func DecryptPayload(cryptoText string, key []byte) ([]byte, error) {
	return decryptPayload(cryptoText, key)
}

func RestoreQueueCounts() { restoreQueueCounts() }
