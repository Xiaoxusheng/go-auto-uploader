package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"upload/internal/app"
	"upload/internal/config"
	"upload/internal/logx"
	"upload/internal/storage"
	"upload/internal/ws"
)

// Streamer 录制主播配置。
type Streamer struct {
	URL    string `json:"url"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if len(s.indexHTML) == 0 {
		http.Error(w, "index.html not found.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, s.indexHTML)
}

func (s *Server) handleGetPubKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	pub := ""
	if kp := s.ensureRSAKeyPair(); kp != nil {
		pub = kp.PublicBase64
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"code": 200,
		"data": map[string]interface{}{"pubkey": pub, "enabled": app.AppCfg().EnableEncryption},
	})
}

func (s *Server) handleExchangeKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EncKey string `json:"enc_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	kp := s.ensureRSAKeyPair()
	if kp == nil {
		http.Error(w, "crypto not ready", 500)
		return
	}
	aesKey, err := kp.UnwrapAESKey(req.EncKey)
	if err != nil {
		http.Error(w, "decryption failed", 400)
		return
	}
	sessionID := fmt.Sprintf("sess-%d-%d", time.Now().UnixNano(), time.Now().UnixNano()%1000000)
	if err := s.sessionKeys.Put(sessionID, aesKey); err != nil {
		http.Error(w, "too many sessions", http.StatusTooManyRequests)
		return
	}
	time.AfterFunc(24*time.Hour, func() { s.sessionKeys.Delete(sessionID) })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"code": 200,
		"data": map[string]string{"session_id": sessionID},
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.sendJSONError(w, r, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		OTP      string `json:"otp"`
	}
	if err := s.parseEncryptedRequest(r, &req); err != nil {
		s.sendJSONError(w, r, http.StatusBadRequest, "无法解析加密的凭据载荷")
		return
	}
	if until, locked := s.authSessions.CheckLocked(); locked {
		s.sendJSONError(w, r, http.StatusTooManyRequests, "失败次数过多，账户已临时锁定，请稍后再试（至 "+until.Format("15:04:05")+"）")
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(app.DashUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(req.Password), []byte(app.DashPass)) == 1
	if !userOK || !passOK {
		s.authSessions.RecordLoginFailure()
		s.sendJSONError(w, r, http.StatusUnauthorized, "Invalid credentials")
		return
	}
	s.authSessions.ResetLoginFailures()
	token := s.authSessions.Issue()
	if token == "" {
		s.sendJSONError(w, r, http.StatusInternalServerError, "令牌签发失败")
		return
	}
	s.sendJSONSuccess(w, r, map[string]interface{}{"token": token, "expiry": time.Now().Add(authSessionTTL).UnixMilli()})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.authSessions.Revoke(authTokenFromRequest(r))
	s.sendJSONSuccess(w, r, nil)
}

func authTokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.sendJSONSuccess(w, r, s.buildStatusData())
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	s.sendJSONSuccess(w, r, buildQueueData())
}

func (s *Server) handleLiveTasks(w http.ResponseWriter, r *http.Request) {
	tasks := make([]map[string]interface{}, 0)
	cutoff := time.Now().Add(-30 * time.Minute)
	app.LiveTasks.Range(func(_, value interface{}) bool {
		task := value.(*app.Task)
		task.Mu.RLock()
		if task.CreatedAt.After(cutoff) {
			duration := 0
			if !task.EndTime.IsZero() {
				duration = int(task.EndTime.Sub(task.CreatedAt).Seconds())
			}
			tasks = append(tasks, map[string]interface{}{
				"id": task.ID, "filename": task.Name, "path": task.Path, "size": task.Size,
				"uploaded": int64(float64(task.Size) * float64(task.Progress) / 100),
				"speed":    task.Speed, "status": task.Status, "error": task.Error,
				"startTime": task.CreatedAt.UnixMilli(), "duration": duration,
			})
		}
		task.Mu.RUnlock()
		return true
	})
	s.sendJSONSuccess(w, r, tasks)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	status := q.Get("status")
	filename := q.Get("filename")
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 50
	}
	filtered := make([]*storage.HistoryRecord, 0)
	for _, record := range app.HistoryStore.Snapshot() {
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
	if start >= total {
		s.sendJSONSuccess(w, r, map[string]interface{}{"items": []map[string]interface{}{}, "total": total})
		return
	}
	end := start + limit
	if end > total {
		end = total
	}
	items := make([]map[string]interface{}, 0, end-start)
	for _, record := range filtered[start:end] {
		items = append(items, map[string]interface{}{
			"id": record.UploadTime, "filename": record.Name, "path": record.LocalPath,
			"size": record.Size, "uploaded": record.Size, "speed": 0,
			"status": record.Status, "error": record.ErrorMsg, "duration": record.Duration,
		})
	}
	s.sendJSONSuccess(w, r, map[string]interface{}{"items": items, "total": total})
}

func (s *Server) handleControlStart(w http.ResponseWriter, r *http.Request) {
	app.SetRunning(true)
	app.SetStartTime(time.Now())
	log.Println("[CONTROL] 🚀 用户下发指令：启动系统，恢复扫描与上传任务")
	app.TriggerScan("start")
	s.sendJSONSuccess(w, r, nil)
}

func (s *Server) handleControlPause(w http.ResponseWriter, r *http.Request) {
	app.SetRunning(false)
	log.Println("[CONTROL] ⏸️ 用户下发指令：暂停系统运行")
	app.SendWeChatNotify("系统暂停上传通知", "管理员已通过控制台下发指令，系统目前已暂停文件上传。")
	s.sendJSONSuccess(w, r, nil)
}

func (s *Server) handleControlStop(w http.ResponseWriter, r *http.Request) {
	app.SetRunning(false)
	log.Println("[CONTROL] 🛑 用户下发指令：停止系统运行")
	app.SendWeChatNotify("系统停止上传通知", "管理员已通过控制台下发指令，系统目前已完全停止一切上传活动。")
	s.sendJSONSuccess(w, r, nil)
}

func (s *Server) handleControlRelogin(w http.ResponseWriter, r *http.Request) {
	if err := app.Login(); err != nil {
		log.Println("[CONTROL][ERR] 用户尝试刷新远端授权失败:", err)
		s.sendJSONError(w, r, http.StatusInternalServerError, "Relogin failed")
		return
	}
	log.Println("[CONTROL] 🔑 用户下发指令：远端授权凭证已成功刷新")
	s.sendJSONSuccess(w, r, nil)
}

func (s *Server) handleControlRescan(w http.ResponseWriter, r *http.Request) {
	log.Println("[CONTROL] 🔍 用户下发指令：手动触发深度目录重新扫描")
	app.TriggerScan("rescan")
	s.sendJSONSuccess(w, r, map[string]interface{}{"message": "重新扫描已触发"})
}

func (s *Server) handleControlClearFailQueue(w http.ResponseWriter, r *http.Request) {
	app.QueueFail = sync.Map{}
	atomic.StoreInt64(&app.QueueFailCount, 0)
	log.Println("[CONTROL] 🧹 用户下发指令：已清空失败任务队列")
	s.sendJSONSuccess(w, r, nil)
}

func (s *Server) handleControlRetryFailQueue(w http.ResponseWriter, r *http.Request) {
	n := atomic.LoadInt64(&app.QueueFailCount)
	atomic.AddInt64(&app.QueueCount, n)
	app.QueueFail = sync.Map{}
	atomic.StoreInt64(&app.QueueFailCount, 0)
	log.Println("[CONTROL] 🔄 用户下发指令：失败任务已全部压入等待队列准备重试")
	app.TriggerScan("rescan")
	s.sendJSONSuccess(w, r, nil)
}

func (s *Server) handleControlClearSuccessQueue(w http.ResponseWriter, r *http.Request) {
	app.QueueSuccess = sync.Map{}
	atomic.StoreInt64(&app.QueueSuccessCount, 0)
	log.Println("[CONTROL] 🧹 用户下发指令：已清空成功任务队列展示历史")
	s.sendJSONSuccess(w, r, nil)
}

func (s *Server) handleDirsStatus(w http.ResponseWriter, r *http.Request) {
	statuses := make([]*storage.DirStatus, 0)
	for _, dir := range app.AppCfg().Dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if ds, exists := app.DirStatusStore.Get(dir); exists {
			ds.Mu.RLock()
			clone := &storage.DirStatus{
				Path: ds.Path, TotalFiles: ds.TotalFiles, UploadedFiles: ds.UploadedFiles,
				PendingFiles: ds.PendingFiles, TotalSize: ds.TotalSize,
				UploadedSize: ds.UploadedSize, LastScanTime: ds.LastScanTime,
			}
			ds.Mu.RUnlock()
			statuses = append(statuses, clone)
		} else {
			statuses = append(statuses, &storage.DirStatus{Path: dir, LastScanTime: time.Now().UnixMilli()})
		}
	}
	s.sendJSONSuccess(w, r, statuses)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := app.AppCfg()
		cfg.DashboardUser = ""
		cfg.DashboardPass = ""
		s.sendJSONSuccess(w, r, cfg)
	case http.MethodPut:
		var newConfig config.Config
		if err := s.parseEncryptedRequest(r, &newConfig); err != nil {
			s.sendJSONError(w, r, http.StatusBadRequest, "非法配置实体或解密异常")
			return
		}
		prev := app.AppCfg()
		newConfig.DashboardUser = prev.DashboardUser
		newConfig.DashboardPass = prev.DashboardPass
		app.CfgStore.Replace(newConfig)
		app.SaveConfigToFile()
		log.Printf("[CONTROL] ⚙️ 用户保存了新配置，目标扫描目录已变更为: [%s]，加密模式: %v，上传开关: %v，TS转MP4: %v",
			strings.Join(newConfig.Dirs, " | "), newConfig.EnableEncryption, newConfig.EnableUpload, newConfig.ConvertMP4)
		app.TriggerScan("config-update")
		app.TriggerReportReset()
		s.sendJSONSuccess(w, r, nil)
	default:
		s.sendJSONError(w, r, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (s *Server) handleCookies(w http.ResponseWriter, r *http.Request) {
	configPath := app.AppCfg().RecorderConfigPath
	if configPath == "" {
		s.sendJSONError(w, r, http.StatusBadRequest, "尚未配置录制引擎主配置文件路径 (config.ini)")
		return
	}
	if r.Method == http.MethodGet {
		data, err := os.ReadFile(configPath)
		if err != nil {
			s.sendJSONSuccess(w, r, map[string]string{"douyin": "", "kuaishou": "", "sooplive": "", "liveSavePath": ""})
			return
		}
		res := map[string]string{"douyin": "", "kuaishou": "", "sooplive": "", "liveSavePath": ""}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			for _, key := range []string{"douyin_cookie", "kuaishou_cookie", "sooplive_cookie", "live_path"} {
				if strings.HasPrefix(trimmed, key) {
					parts := strings.SplitN(trimmed, "=", 2)
					if len(parts) == 2 {
						field := map[string]string{
							"douyin_cookie": "douyin", "kuaishou_cookie": "kuaishou",
							"sooplive_cookie": "sooplive", "live_path": "liveSavePath",
						}[key]
						res[field] = strings.TrimSpace(parts[1])
					}
				}
			}
		}
		s.sendJSONSuccess(w, r, res)
		return
	}
	if r.Method == http.MethodPut {
		var req struct {
			Douyin       string `json:"douyin"`
			Kuaishou     string `json:"kuaishou"`
			Sooplive     string `json:"sooplive"`
			LiveSavePath string `json:"liveSavePath"`
		}
		if err := s.parseEncryptedRequest(r, &req); err != nil {
			s.sendJSONError(w, r, http.StatusBadRequest, "加密层校验失败")
			return
		}
		data, err := os.ReadFile(configPath)
		if err != nil {
			s.sendJSONError(w, r, http.StatusInternalServerError, "读取配置文件失败: "+err.Error())
			return
		}
		lines := strings.Split(string(data), "\n")
		found := map[string]bool{}
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(trimmed, "douyin_cookie"):
				lines[i] = "douyin_cookie=" + req.Douyin
				found["douyin"] = true
			case strings.HasPrefix(trimmed, "kuaishou_cookie"):
				lines[i] = "kuaishou_cookie=" + req.Kuaishou
				found["kuaishou"] = true
			case strings.HasPrefix(trimmed, "sooplive_cookie"):
				lines[i] = "sooplive_cookie=" + req.Sooplive
				found["sooplive"] = true
			case strings.HasPrefix(trimmed, "live_path"):
				lines[i] = "live_path=" + req.LiveSavePath
				found["live"] = true
			}
		}
		insertAfter := func(section, key, val string) {
			for i, line := range lines {
				if strings.TrimSpace(line) == section {
					lines = append(lines[:i+1], append([]string{key + "=" + val}, lines[i+1:]...)...)
					return
				}
			}
			if section == "[base]" {
				lines = append([]string{"[base]", key + "=" + val, ""}, lines...)
			} else {
				lines = append(lines, "", section, key+"="+val)
			}
		}
		if !found["douyin"] && req.Douyin != "" {
			insertAfter("[cookie]", "douyin_cookie", req.Douyin)
		}
		if !found["kuaishou"] && req.Kuaishou != "" {
			insertAfter("[cookie]", "kuaishou_cookie", req.Kuaishou)
		}
		if !found["sooplive"] && req.Sooplive != "" {
			insertAfter("[cookie]", "sooplive_cookie", req.Sooplive)
		}
		if !found["live"] && req.LiveSavePath != "" {
			insertAfter("[base]", "live_path", req.LiveSavePath)
		}
		if err := os.WriteFile(configPath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
			log.Printf("[CONTROL][ERR] 无法写入配置文件 %s: %v", configPath, err)
			s.sendJSONError(w, r, http.StatusInternalServerError, "保存配置失败: "+err.Error())
			return
		}
		log.Printf("[CONTROL] 🍪 用户在网页端成功更新了主配置文件 %s", configPath)
		s.sendJSONSuccess(w, r, nil)
		return
	}
	s.sendJSONError(w, r, http.StatusMethodNotAllowed, "Method not allowed")
}

func getStreamersData() []Streamer {
	data, err := os.ReadFile(app.AppCfg().LiveConfigPath)
	if err != nil {
		return []Streamer{}
	}
	content := strings.TrimPrefix(string(data), "\ufeff")
	var streamers []Streamer
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "\ufeff"))
		if line == "" {
			continue
		}
		line = strings.ReplaceAll(line, "，", ",")
		active := true
		if strings.HasPrefix(line, "#") {
			active = false
			line = strings.TrimPrefix(strings.TrimPrefix(line, "#"), "\ufeff")
		}
		parts := strings.SplitN(line, ",", 2)
		url := strings.ReplaceAll(strings.TrimSpace(parts[0]), "\ufeff", "")
		name := ""
		if len(parts) > 1 {
			name = strings.TrimSpace(parts[1])
			name = strings.TrimPrefix(name, "主播: ")
			name = strings.TrimPrefix(name, "主播:")
			name = strings.TrimPrefix(name, "主播：")
			name = strings.ReplaceAll(name, "\ufeff", "")
		}
		if name == "未命名" || name == "⏳ 等待自动获取..." {
			name = ""
		}
		streamers = append(streamers, Streamer{URL: url, Name: name, Active: active})
	}
	if streamers == nil {
		streamers = []Streamer{}
	}
	return streamers
}

func (s *Server) handleStreamers(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.sendJSONSuccess(w, r, getStreamersData())
		return
	}
	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		var req []Streamer
		if err := s.parseEncryptedRequest(r, &req); err != nil {
			s.sendJSONError(w, r, http.StatusBadRequest, "解析密文体异常")
			return
		}
		var sb strings.Builder
		for _, st := range req {
			if !st.Active {
				sb.WriteString("#")
			}
			sb.WriteString(strings.ReplaceAll(st.URL, "\ufeff", ""))
			cleanName := strings.ReplaceAll(st.Name, "\ufeff", "")
			if cleanName != "" && cleanName != "未命名" && cleanName != "⏳ 等待自动获取..." && !strings.Contains(cleanName, "等待引擎抓取") {
				sb.WriteString(",主播: " + cleanName)
			}
			sb.WriteString("\n")
		}
		if err := os.WriteFile(app.AppCfg().LiveConfigPath, []byte(sb.String()), 0644); err != nil {
			log.Printf("[CONTROL][ERR] 无法写入录制配置文件: %v", err)
			s.sendJSONError(w, r, http.StatusInternalServerError, "保存配置文件失败")
			return
		}
		log.Printf("[CONTROL] 🎥 用户更新了直播监控名单，共 %d 条记录已写入物理文件", len(req))
		app.BroadcastWS("streamersData", getStreamersData())
		s.sendJSONSuccess(w, r, nil)
		return
	}
	s.sendJSONError(w, r, http.StatusMethodNotAllowed, "Method not allowed")
}

func (s *Server) handleRecorderStatus(w http.ResponseWriter, r *http.Request) {
	s.sendJSONSuccess(w, r, s.docker.Status())
}

func (s *Server) handleRecorderControl(w http.ResponseWriter, r *http.Request) {
	if err := s.docker.Control(r.URL.Query().Get("action")); err != nil {
		log.Printf("[DOCKER][ERR] %v", err)
		s.sendJSONError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s.sendJSONSuccess(w, r, "操作成功执行")
}

func (s *Server) handleRecorderLogs(w http.ResponseWriter, r *http.Request) {
	out, err := s.docker.Logs(100)
	if err != nil {
		s.sendJSONError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	s.sendJSONSuccess(w, r, out)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 50
	}
	filtered := app.AppLogs.Snapshot(q.Get("level"), q.Get("keyword"))
	total := len(filtered)
	start := (page - 1) * limit
	if start >= total {
		s.sendJSONSuccess(w, r, map[string]interface{}{"items": []*logx.Entry{}, "total": total})
		return
	}
	end := start + limit
	if end > total {
		end = total
	}
	s.sendJSONSuccess(w, r, map[string]interface{}{"items": filtered[start:end], "total": total})
}

func (s *Server) handleLogsDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var exportLimit int
	if v := q.Get("limit"); v != "" {
		exportLimit, _ = strconv.Atoi(v)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="system_logs_%s.txt"`, time.Now().Format("20060102-150405")))
	filtered := app.AppLogs.SnapshotAsc(q.Get("level"), q.Get("keyword"))
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
	if !app.AppCfg().EnableEncryption {
		_, _ = w.Write([]byte(sb.String()))
		log.Printf("[CONTROL] 📥 用户导出了 %d 条明文系统日志", len(filtered))
		return
	}
	key, err := s.sessionKey(r)
	if err != nil {
		http.Error(w, "Unauthorized Session", 401)
		return
	}
	enc, _ := encryptPayload([]byte(sb.String()), key)
	_, _ = w.Write([]byte(enc))
	log.Printf("[CONTROL] 🛡️ 用户导出了 %d 条加密系统日志", len(filtered))
}

func (s *Server) handleActiveStreamers(w http.ResponseWriter, r *http.Request) {
	s.sendJSONSuccess(w, r, app.GetActiveStreamers())
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	encEnabled := app.AppCfg().EnableEncryption
	var key []byte
	var err error
	if encEnabled {
		key, err = s.sessionKey(r)
		if err != nil {
			http.Error(w, "Unauthorized Session", 401)
			return
		}
	}
	conn, err := s.wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if app.WSHub == nil {
		app.InitHubs()
	}
	client := &ws.Client{Conn: conn, AESKey: key}
	app.WSHub.Register(client)
	go ws.WritePump(client, 2*time.Second)
	go func() {
		defer app.WSHub.Unregister(client)
		defer conn.Close()
		for {
			var msg ws.Message
			if encEnabled && client.AESKey != nil {
				var encMsg struct {
					Encrypted string `json:"encrypted"`
				}
				if err := conn.ReadJSON(&encMsg); err != nil {
					return
				}
				decrypted, err := decryptPayload(encMsg.Encrypted, client.AESKey)
				if err != nil {
					continue
				}
				if err := json.Unmarshal(decrypted, &msg); err != nil {
					continue
				}
			} else {
				if err := conn.ReadJSON(&msg); err != nil {
					return
				}
			}
			if msg.Type == "ping" {
				pongRaw, _ := json.Marshal(ws.Message{Type: "pong", Payload: msg.Payload})
				final := pongRaw
				if encEnabled && client.AESKey != nil {
					if enc, err := encryptPayload(pongRaw, client.AESKey); err == nil {
						final = []byte(`{"encrypted":"` + enc + `"}`)
					}
				}
				client.TrySend(final)
			}
		}
	}()
}
