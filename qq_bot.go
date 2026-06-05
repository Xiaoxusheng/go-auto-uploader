package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

/* ==========================================
 * 高性能 QQ 机器人控制守护模块 (OneBot v11 / NapCatQQ 标准)
 * 核心升级：修复 NapCat 跨容器文件隔离导致无法发送的问题 (启用 base64 流封包)
 * 核心升级：引入 JSON-RPC Echo 机制，支持通过 file_id 穿透提取接收到的更新文件
 * ========================================== */

var (
	qqEnabled   bool
	qqMu        sync.RWMutex
	activeConn  *websocket.Conn
	qqWriteChan = make(chan interface{}, 1000)
	qqAdmin     int64

	// 交互状态机与高并发回调池
	qqSearchContext sync.Map // Key: UserID, Value: *qqSearchSession
	qqLastFileURL   sync.Map // Key: UserID, Value: string (文件标记)
	qqEchoCallbacks sync.Map // Key: string (EchoID), Value: chan []byte 用于同步请求
)

// qqSearchSession 定义单次搜索的生命周期上下文
type qqSearchSession struct {
	Tasks     []MenuTask
	ExpiresAt time.Time
}

// QQMessage 定义接收消息体
type QQMessage struct {
	PostType    string `json:"post_type"`
	MessageType string `json:"message_type"`
	NoticeType  string `json:"notice_type"`
	UserID      int64  `json:"user_id"`
	Message     string `json:"message"`
	FileURL     string `json:"file_url"`
}

type qqMessageEnvelope struct {
	PostType    string          `json:"post_type"`
	MessageType string          `json:"message_type"`
	NoticeType  string          `json:"notice_type"`
	UserID      json.RawMessage `json:"user_id"`
	Message     json.RawMessage `json:"message"`
	File        json.RawMessage `json:"file"`
}

type qqMessageSegment struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
}

func (m *QQMessage) UnmarshalJSON(data []byte) error {
	var raw qqMessageEnvelope
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	userID, err := parseQQUserID(raw.UserID)
	if err != nil {
		return err
	}

	messageText, _ := parseQQMessageText(raw.Message)

	m.PostType = raw.PostType
	m.MessageType = raw.MessageType
	m.NoticeType = raw.NoticeType
	m.UserID = userID
	m.Message = messageText

	if len(raw.File) > 0 && string(raw.File) != "null" {
		var fileData struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(raw.File, &fileData); err == nil {
			m.FileURL = fileData.URL
		}
	}
	return nil
}

func parseQQUserID(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var id int64
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, nil
	}
	var idText string
	if err := json.Unmarshal(raw, &idText); err == nil {
		idText = strings.TrimSpace(idText)
		if idText == "" {
			return 0, nil
		}
		var parsed int64
		if _, err := fmt.Sscanf(idText, "%d", &parsed); err == nil {
			return parsed, nil
		}
	}
	return 0, fmt.Errorf("invalid user_id")
}

func parseQQMessageText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var segments []qqMessageSegment
	if err := json.Unmarshal(raw, &segments); err == nil {
		var builder strings.Builder
		for _, segment := range segments {
			builder.WriteString(flattenQQMessageSegment(segment))
		}
		return builder.String(), nil
	}
	return "", fmt.Errorf("unsupported format")
}

// flattenQQMessageSegment 拍平并拦截 NapCat 传来的所有可能的文件形态
func flattenQQMessageSegment(segment qqMessageSegment) string {
	switch segment.Type {
	case "text":
		if text, ok := segment.Data["text"].(string); ok {
			return text
		}
	case "at":
		if qq, ok := segment.Data["qq"]; ok {
			return fmt.Sprintf("@%v", qq)
		}
	case "file":
		// 拦截所有形式的文件表示，确保 NapCat 的 file_id 不被遗漏
		if url, ok := segment.Data["url"].(string); ok && url != "" {
			return fmt.Sprintf(" [FILE_URL:%s] ", url)
		}
		if path, ok := segment.Data["path"].(string); ok && path != "" {
			return fmt.Sprintf(" [FILE_PATH:%s] ", path)
		}
		if fileId, ok := segment.Data["file"].(string); ok && fileId != "" {
			return fmt.Sprintf(" [FILE_ID:%s] ", fileId)
		}
	}

	if text, ok := segment.Data["text"].(string); ok {
		return text
	}
	return ""
}

type QQAction struct {
	Action string      `json:"action"`
	Params interface{} `json:"params"`
	Echo   string      `json:"echo,omitempty"`
}

func InitQQBot() {
	log.Println("[QQ-BOT] 🚀 开始初始化 QQ 机器人引擎 (兼容 NapCat)...")
	appConfigMu.RLock()
	wsURL := appConfig.QQBotWSURL
	token := appConfig.QQBotToken
	adminID := appConfig.QQAdminID
	appConfigMu.RUnlock()

	if wsURL == "" || adminID == 0 {
		log.Println("[QQ-BOT] ⚠️ 未配置参数，已跳过 QQ 引擎初始化")
		return
	}

	qqAdmin = adminID
	go checkAndNotifyQQOTASuccess()
	go qqWriteLoop()
	go qqConnectionManager(wsURL, token)
}

func checkAndNotifyQQOTASuccess() {
	flagPath := filepath.Join(".", ".ota_qq_user_id")
	flagData, err := os.ReadFile(flagPath)
	if err != nil {
		return
	}

	userID, parseErr := strconv.ParseInt(strings.TrimSpace(string(flagData)), 10, 64)
	if parseErr == nil && userID != 0 {
		sendQQAPIMessage(userID, "🎉 [系统级通知] OTA 热更新与服务重启已完成，新的核心引擎已经上线接管。")
		log.Printf("[QQ-BOT] ✅ OTA 成功回执已发送至 UserID: %d\n", userID)
	}
	_ = os.Remove(flagPath)
}

func qqConnectionManager(wsURL, token string) {
	for {
		log.Printf("[QQ-BOT] 🔌 正在连接 OneBot 服务器: %s ...\n", wsURL)
		headers := http.Header{}
		if token != "" {
			headers.Add("Authorization", "Bearer "+token)
		}

		conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
		if err != nil {
			log.Printf("[QQ-BOT-ERROR] ❌ 连接 QQ 服务器失败: %v，10秒后重试\n", err)
			time.Sleep(10 * time.Second)
			continue
		}

		qqMu.Lock()
		activeConn = conn
		qqEnabled = true
		qqMu.Unlock()

		log.Println("[QQ-BOT] ✅ 已成功连接至 QQ OneBot 架构")

		qqReadLoop(conn)

		qqMu.Lock()
		qqEnabled = false
		activeConn = nil
		qqMu.Unlock()

		conn.Close()
		log.Println("[QQ-BOT-WARNING] ⚠️ QQ 连接已断开，5秒后尝试重连...")
		time.Sleep(5 * time.Second)
	}
}

func qqWriteLoop() {
	for payload := range qqWriteChan {
		qqMu.RLock()
		conn := activeConn
		enabled := qqEnabled
		qqMu.RUnlock()

		if !enabled || conn == nil {
			continue
		}

		err := conn.WriteJSON(payload)
		if err != nil {
			log.Printf("[QQ-BOT-WRITE-ERR] ❌ 发送失败: %v\n", err)
		}
	}
}

func qqReadLoop(conn *websocket.Conn) {
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[QQ-BOT-DEBUG] 底层网络读取异常: %v\n", err)
			break
		}

		// 拦截并分发底层 JSON-RPC Echo 回调请求，供提取文件直链使用
		var rawMsg map[string]interface{}
		if err := json.Unmarshal(message, &rawMsg); err == nil {
			if echoVal, ok := rawMsg["echo"]; ok && echoVal != nil {
				echoStr := fmt.Sprintf("%v", echoVal)
				if ch, exists := qqEchoCallbacks.Load(echoStr); exists {
					ch.(chan []byte) <- message
					qqEchoCallbacks.Delete(echoStr)
					continue
				}
			}
		}

		var msg QQMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		if msg.PostType == "notice" && msg.NoticeType == "offline_file" {
			if msg.UserID != qqAdmin {
				continue
			}
			if msg.FileURL != "" {
				qqLastFileURL.Store(msg.UserID, "url:"+msg.FileURL)
				sendQQAPIMessage(msg.UserID, "📥 已接收到您发送的文件！\n如果这是新版引擎，请直接回复 /update 即可执行覆盖。")
			}
			continue
		}

		if msg.PostType == "message" && msg.MessageType == "private" {
			if msg.UserID != qqAdmin {
				sendQQAPIMessage(msg.UserID, "⛔ [安全拦截] 您没有权限控制此基站。")
				continue
			}
			go handleQQMessage(msg)
		}
	}
}

// callQQAPIWithReply 实现基于 Echo 模型的 JSON-RPC 同步阻塞请求
func callQQAPIWithReply(action string, params interface{}) []byte {
	echoID := fmt.Sprintf("%d", time.Now().UnixNano())
	ch := make(chan []byte, 1)
	qqEchoCallbacks.Store(echoID, ch)

	payload := QQAction{
		Action: action,
		Params: params,
		Echo:   echoID,
	}

	select {
	case qqWriteChan <- payload:
	default:
		qqEchoCallbacks.Delete(echoID)
		return nil
	}

	// 阻塞等待框架接口响应，超时机制 8 秒
	select {
	case res := <-ch:
		return res
	case <-time.After(8 * time.Second):
		qqEchoCallbacks.Delete(echoID)
		return nil
	}
}

func callQQAPI(action string, params interface{}) {
	payload := QQAction{
		Action: action,
		Params: params,
	}
	select {
	case qqWriteChan <- payload:
	default:
		log.Println("[QQ-BOT-WARN] ⚠️ 写入通道已满")
	}
}

func sendQQAPIMessage(userID int64, text string) {
	callQQAPI("send_private_msg", map[string]interface{}{
		"user_id": userID,
		"message": text,
	})
}

func SendQQNotification(title, body string) {
	qqMu.RLock()
	enabled := qqEnabled
	admin := qqAdmin
	qqMu.RUnlock()

	if !enabled || admin == 0 {
		return
	}

	cleanTitle := strings.NewReplacer("▶️ ", "", "停 ", "", "🛑 ", "", "⏸️ ", "").Replace(title)
	replacer := strings.NewReplacer("<b>", "", "</b>", "", "<code>", "", "</code>", "")
	cleanBody := replacer.Replace(body)

	icon := "ℹ️"
	if strings.Contains(title, "开播") {
		icon = "🟢"
	} else if strings.Contains(title, "下播") || strings.Contains(title, "暂停") {
		icon = "🟠"
	} else if strings.Contains(title, "异常") || strings.Contains(title, "失败") {
		icon = "🔴"
	}

	text := fmt.Sprintf("%s 【%s】\n\n%s", icon, cleanTitle, cleanBody)
	sendQQAPIMessage(admin, text)
}

func handleQQMessage(msg QQMessage) {
	cmdText := strings.TrimSpace(msg.Message)

	// 预处理：无缝拦截所有形式的文件发送 (兼容 NapCat 及传统框架)
	if strings.Contains(cmdText, "[FILE_") {
		matched := false

		urlRe := regexp.MustCompile(`\[FILE_URL:([^\]]+)\]`)
		if m := urlRe.FindStringSubmatch(cmdText); len(m) > 1 {
			qqLastFileURL.Store(msg.UserID, "url:"+m[1])
			matched = true
		} else {
			pathRe := regexp.MustCompile(`\[FILE_PATH:([^\]]+)\]`)
			if m := pathRe.FindStringSubmatch(cmdText); len(m) > 1 {
				qqLastFileURL.Store(msg.UserID, "path:"+m[1])
				matched = true
			} else {
				idRe := regexp.MustCompile(`\[FILE_ID:([^\]]+)\]`)
				if m := idRe.FindStringSubmatch(cmdText); len(m) > 1 {
					qqLastFileURL.Store(msg.UserID, "id:"+m[1])
					matched = true
				}
			}
		}

		if matched && !strings.Contains(cmdText, "/update") {
			sendQQAPIMessage(msg.UserID, "📥 已检测到您发来的文件！\n如果这是新版引擎，请直接回复 /update 尝试执行热更新。")
			return
		}
	}

	parts := strings.Fields(cmdText)
	if len(parts) == 0 {
		return
	}

	if num, err := strconv.Atoi(parts[0]); err == nil {
		if qqHandleInteractiveSearch(msg.UserID, num) {
			return
		}
	}

	cmd := strings.ToLower(parts[0])

	switch cmd {
	case "/status", "状态":
		sendQQAPIMessage(msg.UserID, qqHandleStatus())
	case "/list", "列表":
		filter := ""
		if len(parts) > 1 {
			filter = parts[1]
		}
		sendQQAPIMessage(msg.UserID, "📋 正在打包装配相册，请稍候...")
		qqHandleList(msg.UserID, filter)
	case "/find", "搜索":
		if len(parts) < 2 {
			sendQQAPIMessage(msg.UserID, "🔍 请输入关键字。例如: /find 张三")
		} else {
			qqHandleSearch(msg.UserID, parts[1])
		}
	case "/watermark":
		qqHandleWatermark(msg.UserID, parts)
	case "/update":
		qqHandleUpdate(msg.UserID, cmdText)
	case "/log":
		qqHandleLog(msg.UserID)
	case "/changelog", "/news", "/更新日志":
		qqHandleChangelog(msg.UserID)
	case "/dashboard":
		qqHandleDashboard(msg.UserID)
	case "/chart":
		qqHandleChart(msg.UserID)
	case "/pause", "/resume", "/delete", "/cover":
		if len(parts) < 2 {
			sendQQAPIMessage(msg.UserID, "❌ 缺少指纹参数。")
		} else {
			hash := parts[1]
			var fullKey string
			builtinStatusMap.Range(func(k, v interface{}) bool {
				if getTaskHash(k.(string)) == hash {
					fullKey = k.(string)
					return false
				}
				return true
			})

			if fullKey == "" {
				sendQQAPIMessage(msg.UserID, "⚠️ 未找到该指纹。")
				return
			}

			if cmd == "/cover" {
				sendQQAPIMessage(msg.UserID, "📸 正在提取后台实时画面...")
				qqHandlePreciseCover(msg.UserID, fullKey)
			} else {
				action := strings.TrimPrefix(cmd, "/")
				res := tgExecutePreciseControl(action, fullKey)
				sendQQAPIMessage(msg.UserID, res)
			}
		}
	case "/menu", "/help", "菜单", "帮助":
		helpText := `🎛️ Live-Engine 控制台

/status - 负载与流量统计
/list [平台] - 获取在线主播图集
/find [名] - 搜索并创建交互会话
/cover [码] - 提取指定实时画面
/pause [码] - 挂起指定录制任务
/resume [码] - 恢复指定录制任务
/delete [码] - 彻底移除监控记录
/watermark [选项] - 动态调整水印
/log - 获取最新运行日志文件
/changelog - 查看最近版本更新日志
/update [直链] - OTA 热更新内核
/dashboard - 生成免密 Web 入口
/chart - 渲染今日流量堆叠走势图`
		sendQQAPIMessage(msg.UserID, helpText)
	default:
		if strings.HasPrefix(cmdText, "http") || strings.Contains(cmdText, "v.douyin.com") {
			sendQQAPIMessage(msg.UserID, "⏳ 嗅探到链接，正在解析目标直播间...")
			res := tgHandleAdd(cmdText)
			sendQQAPIMessage(msg.UserID, res)
		}
	}
}

func qqHandleInteractiveSearch(userID int64, index int) bool {
	val, ok := qqSearchContext.Load(userID)
	if !ok {
		return false
	}
	session := val.(*qqSearchSession)
	if time.Now().After(session.ExpiresAt) {
		qqSearchContext.Delete(userID)
		sendQQAPIMessage(userID, "⚠️ 搜索会话已过期，请重新 /find。")
		return true
	}

	if index < 1 || index > len(session.Tasks) {
		sendQQAPIMessage(userID, "❌ 输入序号越界。")
		return true
	}

	target := session.Tasks[index-1]
	action := "pause"
	if target.IsPaused {
		action = "resume"
	}

	res := tgExecutePreciseControl(action, target.Key)
	sendQQAPIMessage(userID, fmt.Sprintf("🎯 执行对象: %s\n%s", target.Name, res))

	qqSearchContext.Delete(userID)
	return true
}

func saveBuiltinConfigQQ() {
	if builtinConfig == nil {
		return
	}
	data, err := json.MarshalIndent(builtinConfig, "", "    ")
	if err == nil {
		os.WriteFile("builtin_config.json", data, 0644)
	}
}

func qqHandleWatermark(userID int64, parts []string) {
	if len(parts) < 2 {
		help := `🎨 水印热控:
/watermark toggle
/watermark text [内容]
/watermark size [数字]
/watermark color [颜色]`
		sendQQAPIMessage(userID, help)
		return
	}

	subCmd := strings.ToLower(parts[1])
	if builtinConfig == nil {
		return
	}

	switch subCmd {
	case "toggle":
		builtinConfig.WatermarkEnable = !builtinConfig.WatermarkEnable
		saveBuiltinConfigQQ()
		state := "关闭"
		if builtinConfig.WatermarkEnable {
			state = "开启"
		}
		sendQQAPIMessage(userID, "✅ 水印已: "+state)
	case "text":
		if len(parts) >= 3 {
			builtinConfig.WatermarkText = strings.Join(parts[2:], " ")
			saveBuiltinConfigQQ()
			sendQQAPIMessage(userID, "✅ 文字已更新")
		}
	case "size":
		if len(parts) >= 3 {
			size, err := strconv.Atoi(parts[2])
			if err == nil && size > 0 {
				builtinConfig.WatermarkFontSize = size
				saveBuiltinConfigQQ()
				sendQQAPIMessage(userID, "✅ 字号已更新")
			}
		}
	case "color":
		if len(parts) >= 3 {
			builtinConfig.WatermarkFontColor = parts[2]
			saveBuiltinConfigQQ()
			sendQQAPIMessage(userID, "✅ 颜色已更新")
		}
	}
}

// qqHandleLog 强力适配版的底层日志提取引擎
// 机制：弃用磁盘路径挂载，在内存将数据转换为 Base64 流封包穿透 Docker，解决跨环境发送文件失败的问题。
func qqHandleLog(userID int64) {
	var buf strings.Builder
	logsMu.RLock()
	for _, entry := range logs {
		buf.WriteString(fmt.Sprintf("[%s] [%s] %s\n", entry.Time, entry.Level, entry.Message))
		if entry.Error != "" {
			buf.WriteString(fmt.Sprintf("  Error: %s\n", entry.Error))
		}
	}
	logsMu.RUnlock()

	if buf.Len() == 0 {
		sendQQAPIMessage(userID, "📭 当前内存中无日志数据。")
		return
	}

	sendQQAPIMessage(userID, "📁 正在打包文件流封包，准备绕过隔离传输...")

	// 转换为 base64 协议，完美绕过 NapCat 或跨目录的读写隔离限制
	base64Str := base64.StdEncoding.EncodeToString([]byte(buf.String()))
	fileUri := "base64://" + base64Str
	fileName := fmt.Sprintf("system_logs_%s.txt", time.Now().Format("20060102_150405"))

	callQQAPI("upload_private_file", map[string]interface{}{
		"user_id": userID,
		"file":    fileUri,
		"name":    fileName,
	})
}

func qqHandleChangelog(userID int64) {
	data, err := os.ReadFile("CHANGELOG.md")
	if err != nil {
		sendQQAPIMessage(userID, "❌ 读取更新日志失败: "+err.Error())
		return
	}

	text := strings.TrimSpace(string(data))
	if text == "" {
		sendQQAPIMessage(userID, "📭 当前没有可展示的更新日志。")
		return
	}

	lines := strings.Split(text, "\n")
	var filtered []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "---" {
			continue
		}
		if len(filtered) > 0 && strings.HasPrefix(line, "### [") {
			break
		}
		filtered = append(filtered, line)
	}

	qqSendLongMessage(userID, "📝 最近版本更新日志\n\n"+strings.Join(filtered, "\n"))
}

func qqSendLongMessage(userID int64, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	const limit = 1500
	runes := []rune(text)
	for len(runes) > 0 {
		size := limit
		if len(runes) < size {
			size = len(runes)
		}

		chunk := string(runes[:size])
		if len(runes) > size {
			if idx := strings.LastIndex(chunk, "\n"); idx > 0 && idx > len(chunk)/3 {
				chunk = chunk[:idx]
				size = len([]rune(chunk))
			}
		}

		sendQQAPIMessage(userID, strings.TrimSpace(chunk))
		runes = runes[size:]
	}
}

func qqHandleDashboard(userID int64) {
	appConfigMu.RLock()
	wsURL := appConfig.RemoteServer
	appConfigMu.RUnlock()

	if wsURL == "" {
		wsURL = "http://127.0.0.1:8080"
	}

	raw := fmt.Sprintf("live_engine_%d_%d", userID, time.Now().Unix())
	hash := sha256.Sum256([]byte(raw))
	token := hex.EncodeToString(hash[:])[:16]

	magicLink := fmt.Sprintf("%s/#/login?magic_token=%s", wsURL, token)
	msg := fmt.Sprintf("🔐 <b>一次性免密通道</b>\n\n专属直达链接：\n%s\n\n*(5分钟后销毁)*", magicLink)
	sendQQAPIMessage(userID, msg)
}

func qqHandleChart(userID int64) {
	var dates []string
	var values []float64

	trendStats.Range(func(key, value interface{}) bool {
		tp := value.(*TrendPoint)
		tp.Mu.Lock()
		dates = append(dates, tp.Date)
		values = append(values, tp.Size)
		tp.Mu.Unlock()
		return true
	})

	if len(dates) == 0 {
		sendQQAPIMessage(userID, "📭 系统暂无数据。")
		return
	}

	var maxVal float64 = 0
	for _, v := range values {
		if v > maxVal {
			maxVal = v
		}
	}

	var sb strings.Builder
	sb.WriteString("📈 <b>近期流量走势 (GB)</b>\n\n")

	for i := 0; i < len(dates); i++ {
		barLen := 0
		if maxVal > 0 {
			barLen = int((values[i] / maxVal) * 15)
		}
		blocks := strings.Repeat("█", barLen)
		sb.WriteString(fmt.Sprintf("%s │ %s %.2f\n", dates[i], blocks, values[i]))
	}

	sendQQAPIMessage(userID, sb.String())
}

// qqHandleUpdate 利用同步 API 机制从 NapCat 反向提取文件的终极 OTA 功能
func qqHandleUpdate(userID int64, cmdText string) {
	var fileRef string

	// 1. 尝试从当前文本嗅探常规直链
	urlRe := regexp.MustCompile(`https?://[^\s,\]]+`)
	if match := urlRe.FindString(cmdText); match != "" {
		fileRef = "url:" + match
	}

	// 2. 尝试从文件暂存状态机中提取
	if fileRef == "" {
		if val, ok := qqLastFileURL.Load(userID); ok {
			fileRef = val.(string)
			qqLastFileURL.Delete(userID)
		}
	}

	if fileRef == "" {
		sendQQAPIMessage(userID, "❌ 未检测到可用的更新文件。请先发文件给我，或输入带直链的 /update 命令。")
		return
	}

	sendQQAPIMessage(userID, "📥 <b>[OTA 热更新]</b> 正在解析提取新版核心引擎...")

	var fileURL string
	var localPath string

	if strings.HasPrefix(fileRef, "url:") {
		fileURL = strings.TrimPrefix(fileRef, "url:")
	} else if strings.HasPrefix(fileRef, "path:") {
		localPath = strings.TrimPrefix(fileRef, "path:")
	} else if strings.HasPrefix(fileRef, "id:") {
		// NapCat 特性：只给了 file_id，我们主动发起 API 请求问它拿实际地址
		fileID := strings.TrimPrefix(fileRef, "id:")
		sendQQAPIMessage(userID, "⏳ 正在通过 NapCat 框架接口拉取真实下载路径...")

		resBytes := callQQAPIWithReply("get_file", map[string]interface{}{"file_id": fileID})
		if resBytes != nil {
			var fileResp struct {
				Data struct {
					URL  string `json:"url"`
					Path string `json:"path"`
					File string `json:"file"`
				} `json:"data"`
			}
			json.Unmarshal(resBytes, &fileResp)

			if fileResp.Data.URL != "" {
				fileURL = fileResp.Data.URL
			} else if fileResp.Data.Path != "" {
				localPath = fileResp.Data.Path
			} else if fileResp.Data.File != "" {
				// 兼容不同版本的字段差异
				if strings.HasPrefix(fileResp.Data.File, "http") {
					fileURL = fileResp.Data.File
				} else {
					localPath = fileResp.Data.File
				}
			}
		} else {
			sendQQAPIMessage(userID, "❌ 调用 get_file 接口超时。框架可能不支持反向提取。")
			return
		}
	}

	// 统一处理流的读取覆盖
	var src io.ReadCloser
	if fileURL != "" {
		client := &http.Client{Timeout: 60 * time.Second}
		resp, err := client.Get(fileURL)
		if err != nil || resp.StatusCode != http.StatusOK {
			sendQQAPIMessage(userID, "❌ 网络下载失败或地址无效。")
			return
		}
		src = resp.Body
	} else if localPath != "" {
		cleanPath := strings.TrimPrefix(localPath, "file://")
		f, err := os.Open(cleanPath)
		if err != nil {
			sendQQAPIMessage(userID, "❌ 无法读取宿主机跨容器文件路径: "+err.Error())
			return
		}
		src = f
	} else {
		sendQQAPIMessage(userID, "❌ 无法提取有效的文件直链或本地路径，解析失败。")
		return
	}
	defer src.Close()

	exePath, err := os.Executable()
	if err != nil {
		sendQQAPIMessage(userID, "❌ 无法定位运行路径。")
		return
	}

	tmpPath := exePath + ".tmp"
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		sendQQAPIMessage(userID, "❌ 写入权限不足。")
		return
	}
	io.Copy(out, src)
	out.Close()
	os.Chmod(tmpPath, 0755)

	bakPath := exePath + ".bak"
	os.Remove(bakPath)
	os.Rename(exePath, bakPath)
	if err = os.Rename(tmpPath, exePath); err != nil {
		os.Rename(bakPath, exePath)
		sendQQAPIMessage(userID, "❌ 原子覆盖失败。")
		return
	}

	sendQQAPIMessage(userID, "✅ <b>[OTA 替换就绪]</b> 新版本已覆盖！正在执行重启...")

	flagPath := filepath.Join(filepath.Dir(exePath), ".ota_qq_user_id")
	_ = os.WriteFile(flagPath, []byte(strconv.FormatInt(userID, 10)), 0644)

	go func() {
		time.Sleep(2 * time.Second)
		scriptDir := filepath.Dir(exePath)
		scriptPath := filepath.Join(scriptDir, "restart.sh")

		unitName := fmt.Sprintf("uploader-ota-task-%d", time.Now().Unix())
		cmd := exec.Command("systemd-run", "--unit="+unitName, "bash", scriptPath)
		cmd.Dir = scriptDir

		if err := cmd.Start(); err != nil {
			fallbackCmd := exec.Command("bash", scriptPath)
			fallbackCmd.Dir = scriptDir
			fallbackCmd.Start()
			os.Exit(0)
		}
	}()
}

func qqHandleStatus() string {
	stats := buildStatusData()
	queues := buildQueueData()
	uptime := stats["uptime"].(int64)

	var totalSpeed int64 = 0
	liveTasks.Range(func(key, value interface{}) bool {
		task := value.(*Task)
		task.Mu.RLock()
		if task.Status == "uploading" {
			totalSpeed += task.Speed
		}
		task.Mu.RUnlock()
		return true
	})

	var totalUploadedSize int64 = 0
	if dirs, ok := stats["dirs"].([]map[string]interface{}); ok {
		for _, d := range dirs {
			if size, ok := d["uploadedSize"].(int64); ok {
				totalUploadedSize += size
			}
		}
	}

	todayStr := time.Now().Format("01-02")
	var todayTrafficBytes int64 = 0
	if val, exists := trendStats.Load(todayStr); exists {
		tp := val.(*TrendPoint)
		tp.Mu.Lock()
		todayTrafficBytes = int64(tp.Size * 1024 * 1024 * 1024)
		tp.Mu.Unlock()
	}

	return fmt.Sprintf(`📊 运行资源状态

⏱️ 运行时间: %d小时 %d分
💾 磁盘剩余: %s
🧠 引擎内存: %s
⚙️ 工作线程: %d
🚀 瞬时上行: %s/s
📅 今日流量: %s
☁️ 累计流量: %s

📤 上传队列:
等待: %v | 传输: %v
成功: %v | 失败: %v`,
		uptime/3600, (uptime%3600)/60,
		tgFormatBytes(stats["diskFree"].(int64)), tgFormatBytes(stats["ffmpegMem"].(int64)),
		stats["workers"], tgFormatBytes(totalSpeed),
		tgFormatBytes(todayTrafficBytes), tgFormatBytes(totalUploadedSize),
		queues["waiting"], queues["uploading"], queues["success"], queues["failed"])
}

func qqHandleSearch(userID int64, keyword string) {
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	var results []MenuTask

	builtinStatusMap.Range(func(k, v interface{}) bool {
		task := v.(*BuiltinTaskStatus)
		key := k.(string)
		name := task.AnchorName
		if name == "" {
			name = "未命名_" + task.RoomID
		}
		if strings.Contains(strings.ToLower(name), keyword) || strings.Contains(strings.ToLower(task.RoomID), keyword) {
			results = append(results, MenuTask{Key: key, Name: name, IsPaused: task.IsPaused})
		}
		return len(results) < 15
	})

	if len(results) == 0 {
		sendQQAPIMessage(userID, fmt.Sprintf("📭 未找到关键词 [%s] 相关任务。", keyword))
		return
	}

	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })

	qqSearchContext.Store(userID, &qqSearchSession{
		Tasks:     results,
		ExpiresAt: time.Now().Add(60 * time.Second),
	})

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🔍 找到以下目标，请直接回复【序号】切换状态:\n\n"))

	for i, item := range results {
		state := "🟢"
		if item.IsPaused {
			state = "🔘"
		}
		sb.WriteString(fmt.Sprintf("[%d] %s (%s)\n", i+1, item.Name, state))
	}
	sendQQAPIMessage(userID, sb.String())
}

func generateCQImage(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	return fmt.Sprintf("[CQ:image,file=base64://%s]", b64)
}

func qqHandleList(userID int64, filter string) {
	tasks := GetBuiltinRecorderTasks()
	var onlineTasks []BuiltinTaskStatus
	for _, t := range tasks {
		if t.Status == "录制中" {
			if filter != "" && !strings.Contains(strings.ToLower(t.Platform), strings.ToLower(filter)) {
				continue
			}
			onlineTasks = append(onlineTasks, t)
		}
	}

	if len(onlineTasks) == 0 {
		sendQQAPIMessage(userID, "📭 当前条件下没有正在录制的主播。")
		return
	}

	go func() {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("📋 聚合相册流：当前共有 %d 位在线\n\n", len(onlineTasks)))

		chunkSize := 5
		count := 0

		for i, t := range onlineTasks {
			name := t.AnchorName
			if name == "" {
				name = t.RoomID
			}

			fileName := fmt.Sprintf("%s_%s.png", t.Platform, t.RoomID)
			coverPath := filepath.Join(".", "covers", fileName)
			cqImg := generateCQImage(coverPath)

			hash := getTaskHash(t.Platform + "_" + t.RoomID)

			if cqImg != "" {
				sb.WriteString(cqImg)
			}
			sb.WriteString(fmt.Sprintf("\n🔴 %s [%s]\n指纹: %s | 大小: %s\n\n", name, t.Platform, hash, t.FileSize))

			count++
			if count == chunkSize || i == len(onlineTasks)-1 {
				sendQQAPIMessage(userID, strings.TrimSpace(sb.String()))
				sb.Reset()
				count = 0
				time.Sleep(1 * time.Second)
			}
		}
		sendQQAPIMessage(userID, "✅ 列表推送完毕。")
	}()
}

func qqHandlePreciseCover(userID int64, key string) {
	value, exists := builtinStatusMap.Load(key)
	if !exists {
		sendQQAPIMessage(userID, "⚠️ 任务不存在或已删除")
		return
	}
	task := value.(*BuiltinTaskStatus)
	fileName := fmt.Sprintf("%s_%s.png", task.Platform, task.RoomID)
	coverPath := filepath.Join(".", "covers", fileName)

	cqImg := generateCQImage(coverPath)
	if cqImg == "" {
		sendQQAPIMessage(userID, fmt.Sprintf("📭 主播 [%s] 暂未生成有效画面。", task.AnchorName))
		return
	}

	msg := fmt.Sprintf("%s\n📸 %s 实时监控画面", cqImg, task.AnchorName)
	sendQQAPIMessage(userID, msg)
}
