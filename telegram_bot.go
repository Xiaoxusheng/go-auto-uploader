package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"regexp"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

/* ==========================================
 * 高性能 Telegram 机器人控制守护模块
 * 负责指令分发、状态机接管及异步消息投递
 * ========================================== */

var (
	tgBot     *tgbotapi.BotAPI
	tgEnabled bool
)

// InitTelegramBot 初始化 Telegram 机器人引擎。
// 职责：读取全局配置，建立与 Telegram 服务器的长连接，并挂载独立的指令监听协程，确保不阻塞主线程运行。
func InitTelegramBot() {
	appConfigMu.RLock()
	token := appConfig.TelegramToken
	appConfigMu.RUnlock()

	if token == "" {
		log.Println("[TG-BOT] ⚠️ 未配置 Telegram Token，已跳过机器人初始化")
		return
	}

	var err error
	tgBot, err = tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Printf("[TG-BOT] ❌ 机器人连接失败: %v\n", err)
		return
	}

	tgEnabled = true
	log.Printf("[TG-BOT] ✅ 已成功接管机器人: @%s", tgBot.Self.UserName)

	go startTelegramListener()
}

// startTelegramListener 启动长轮询监听器。
// 职责：设置高超时时间降低网络 I/O 消耗，持续拉取更新，并实施严格的 ChatID 权限拦截过滤非法用户的越权请求。
func startTelegramListener() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := tgBot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}

		appConfigMu.RLock()
		allowedChatID := appConfig.TelegramChatID
		appConfigMu.RUnlock()

		if allowedChatID != 0 && update.Message.Chat.ID != allowedChatID {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "⛔ <b>[安全拦截]</b> 您没有权限控制此录制基站。")
			msg.ParseMode = "HTML"
			tgBot.Send(msg)
			continue
		}

		go handleTelegramCommand(update.Message)
	}
}

// splitAndSendTelegramMsg 智能分片发送引擎（针对超长文本优化）。
// 职责：突破 Telegram 单条消息最多 4096 字符的物理限制，将超长列表安全切割并按顺序分批下发，保证数据绝对完整性。
func splitAndSendTelegramMsg(chatID int64, text string) {
	const maxLen = 4000 // 设定 4000 为安全阈值，预留 HTML 标签空间

	if len(text) <= maxLen {
		msg := tgbotapi.NewMessage(chatID, text)
		msg.ParseMode = "HTML"
		msg.DisableWebPagePreview = true
		_, err := tgBot.Send(msg)
		if err != nil {
			log.Printf("[TG-BOT] ❌ 消息发送失败 (HTML解析或网络异常): %v\n", err)
			// 降级为纯文本强制兜底发送
			msg.ParseMode = ""
			tgBot.Send(msg)
		}
		return
	}

	// 文本超长，按换行符进行安全切割分段，避免硬截断导致 HTML 标签残缺
	lines := strings.Split(text, "\n")
	var currentChunk strings.Builder

	for _, line := range lines {
		if currentChunk.Len()+len(line)+1 > maxLen {
			// 当前块已满，发送出去
			msg := tgbotapi.NewMessage(chatID, currentChunk.String())
			msg.ParseMode = "HTML"
			msg.DisableWebPagePreview = true
			if _, err := tgBot.Send(msg); err != nil {
				log.Printf("[TG-BOT] ❌ 分片消息发送失败: %v\n", err)
			}
			currentChunk.Reset()
		}
		currentChunk.WriteString(line + "\n")
	}

	// 发送最后剩余的残块
	if currentChunk.Len() > 0 {
		msg := tgbotapi.NewMessage(chatID, currentChunk.String())
		msg.ParseMode = "HTML"
		msg.DisableWebPagePreview = true
		if _, err := tgBot.Send(msg); err != nil {
			log.Printf("[TG-BOT] ❌ 尾部分片发送失败: %v\n", err)
		}
	}
}

// handleTelegramCommand 路由与指令执行中枢。
// 职责：拦截文本命令，解析参数，并直接穿透到底层内存字典与控制函数，实现系统级极速响应。
func handleTelegramCommand(message *tgbotapi.Message) {
	cmd := message.Command()
	args := message.CommandArguments()
	chatID := message.Chat.ID

	var responseText string

	switch cmd {
	case "start", "help":
		responseText = "🚀 <b>全自动直播录制基站 控制台</b>\n\n" +
			"🔸 <code>/list</code> - 查看当前正在录制的在线主播\n" +
			"🔸 <code>/add &lt;直播链接&gt;</code> - 解析并添加新主播\n" +
			"🔸 <code>/pause &lt;房间号/主播名&gt;</code> - 挂起指定录制\n" +
			"🔸 <code>/resume &lt;房间号/主播名&gt;</code> - 恢复指定录制\n" +
			"🔸 <code>/delete &lt;房间号/主播名&gt;</code> - 彻底删除任务\n" +
			"🔸 <code>/status</code> - 查看服务器系统资源与队列"

	case "list":
		responseText = tgHandleList()

	case "add":
		if args == "" {
			responseText = "⚠️ 参数缺失。\n用法示例：<code>/add https://live.douyin.com/12345678</code>"
		} else {
			responseText = tgHandleAdd(args)
		}

	case "pause", "resume", "delete":
		if args == "" {
			responseText = fmt.Sprintf("⚠️ 请指定目标。\n用法示例：<code>/%s 张三</code> 或 <code>/%s 123456</code>", cmd, cmd)
		} else {
			responseText = tgHandleControl(cmd, args)
		}

	case "status":
		responseText = tgHandleStatus()

	default:
		if cmd != "" {
			responseText = "❓ 未知指令，请发送 <code>/help</code> 查看支持的命令。"
		}
	}

	if responseText != "" {
		splitAndSendTelegramMsg(chatID, responseText)
	}
}

// SendTelegramNotification 异步全局消息投递器。
// 职责：格式化系统警报与上下播通知，进行 HTML 转义保障安全后，投递给授权的超级管理员。
func SendTelegramNotification(title, body string) {
	if !tgEnabled {
		return
	}

	appConfigMu.RLock()
	chatID := appConfig.TelegramChatID
	appConfigMu.RUnlock()

	if chatID == 0 {
		return
	}

	icon := "ℹ️"
	if strings.Contains(title, "开播") {
		icon = "🟢"
	} else if strings.Contains(title, "下播") || strings.Contains(title, "暂停") {
		icon = "🟠"
	} else if strings.Contains(title, "异常") || strings.Contains(title, "熔断") || strings.Contains(title, "失败") {
		icon = "🔴"
	}

	cleanTitle := strings.ReplaceAll(title, "▶️ ", "")
	cleanTitle = strings.ReplaceAll(cleanTitle, "⏹️ ", "")
	cleanTitle = strings.ReplaceAll(cleanTitle, "🛑 ", "")
	cleanTitle = strings.ReplaceAll(cleanTitle, "⏸️ ", "")

	// 采用标准库过滤特殊字符，彻底规避 HTML 注入与解析崩溃
	safeTitle := html.EscapeString(cleanTitle)
	safeBody := html.EscapeString(body)

	text := fmt.Sprintf("%s <b>%s</b>\n\n%s", icon, safeTitle, safeBody)

	splitAndSendTelegramMsg(chatID, text)
}

// tgHandleList 获取内置引擎内存中的任务快照，精准筛选并格式化只展示在线（录制中）的主播名单。
// 职责：遍历内置引擎的状态机，组装在线主播详情清单，剔除离线或等待中的冗余数据。
func tgHandleList() string {
	tasks := GetBuiltinRecorderTasks()
	var onlineTasks []BuiltinTaskStatus

	// 只筛选出状态为“录制中”的任务
	for _, t := range tasks {
		if t.Status == "录制中" {
			onlineTasks = append(onlineTasks, t)
		}
	}

	if len(onlineTasks) == 0 {
		return "📭 当前监控引擎中没有正在录制的主播。"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 <b>当前在线录制名单</b> (共 %d 个)\n\n", len(onlineTasks)))

	for _, t := range onlineTasks {
		name := t.AnchorName
		if name == "" || name == t.RoomID {
			name = "未命名主播"
		}

		safeName := html.EscapeString(name)
		safePlatform := html.EscapeString(t.Platform)
		safeDuration := html.EscapeString(t.Duration)
		safeFileSize := html.EscapeString(t.FileSize)

		sb.WriteString(fmt.Sprintf("🔴 <b>%s</b> [%s]\n", safeName, safePlatform))
		sb.WriteString(fmt.Sprintf("├ 耗时: %s\n", safeDuration))
		sb.WriteString(fmt.Sprintf("└ 大小: %s\n\n", safeFileSize))
	}

	return sb.String()
}

// tgHandleAdd 接收并下发添加新任务指令。
// 职责：拦截处理长短链接，调用底层 API 进行智能去重和引擎挂载。
func tgHandleAdd(rawArgs string) string {
	lines := strings.Split(rawArgs, "\n")
	added := 0

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		shortURLRe := regexp.MustCompile(`https?://v\.douyin\.com/[a-zA-Z0-9]+/?`)
		if shortURLRe.MatchString(line) {
			realURL, err := ExtractBuiltinDouyinLiveURL(line)
			if err == nil && realURL != "" {
				line = realURL
			}
		}

		isP, platformName, roomID, customName, rawURL := parseBuiltinLine(line)
		if roomID == "" || platformName == "" {
			urlRe := regexp.MustCompile(`https?://[^\s,]+`)
			if found := urlRe.FindString(line); found != "" {
				isP, platformName, roomID, customName, rawURL = parseBuiltinLine(found)
			}
		}

		if roomID == "" {
			continue
		}

		key := platformName + "_" + roomID
		if customName != "" {
			builtinCustomNames.Store(key, customName)
		}

		if _, exists := builtinActiveTasks.Load(key); exists {
			continue
		}

		var p BuiltinPlatform
		switch platformName {
		case "Douyin":
			p = &DouyinBuiltinPlatform{}
		case "Kuaishou":
			p = &KuaishouBuiltinPlatform{}
		case "Soop":
			p = &SoopBuiltinPlatform{}
		default:
			continue
		}

		fullLineToSave := rawURL
		if customName != "" {
			fullLineToSave = rawURL + ",主播:" + customName
		}
		syncBuiltinAnchorToTxt("add", platformName, roomID, fullLineToSave)

		displayName := customName
		if displayName == "" {
			displayName = roomID
		}

		if isP {
			builtinTaskStates.Store(key, "paused")
			updateBuiltinStatus(platformName, roomID, displayName, "", builtinConfig.Quality, "已暂停")
		} else {
			updateBuiltinStatus(platformName, roomID, displayName, "", builtinConfig.Quality, "初始化中")
			wrapperStartMonitorIfNotRunning(p, roomID)
		}
		added++
	}

	triggerBuiltinBroadcast()

	if added > 0 {
		return fmt.Sprintf("✅ 成功解析并添加了 %d 个录制任务！", added)
	}
	return "❌ 解析失败，或该主播已存在于队列中。"
}

// tgHandleControl 操控单条记录的状态机。
// 职责：通过模糊或精确匹配寻址内存记录，向协程安全地发送挂起、唤醒或销毁指令。
func tgHandleControl(action, target string) string {
	target = strings.ToLower(strings.TrimSpace(target))
	var targetKey, targetPlatform, targetRoom, targetName string

	builtinStatusMap.Range(func(k, v interface{}) bool {
		task := v.(*BuiltinTaskStatus)
		if strings.ToLower(task.RoomID) == target || strings.Contains(strings.ToLower(task.AnchorName), target) {
			targetKey = k.(string)
			targetPlatform = task.Platform
			targetRoom = task.RoomID
			targetName = task.AnchorName
			return false
		}
		return true
	})

	if targetKey == "" {
		return "⚠️ 未在监控列表中找到匹配的主播或房间号。"
	}

	safeTargetName := html.EscapeString(targetName)

	switch action {
	case "pause":
		builtinTaskStates.Store(targetKey, "paused")
		if cancel, ok := builtinCancels.Load(targetKey); ok {
			cancel.(context.CancelFunc)()
		}
		syncBuiltinAnchorToTxt("pause", targetPlatform, targetRoom, "")
		if existing, ok := builtinStatusMap.Load(targetKey); ok {
			task := existing.(*BuiltinTaskStatus)
			task.IsPaused = true
			task.Status = "已暂停"
			builtinStatusMap.Store(targetKey, task)
		}
		triggerBuiltinBroadcast()
		return fmt.Sprintf("⏸️ 已成功挂起主播 <b>%s</b> 的监控任务。", safeTargetName)

	case "resume":
		builtinTaskStates.Store(targetKey, "running")
		syncBuiltinAnchorToTxt("resume", targetPlatform, targetRoom, "")
		if existing, ok := builtinStatusMap.Load(targetKey); ok {
			task := existing.(*BuiltinTaskStatus)
			task.IsPaused = false
			task.Status = "监控中"
			builtinStatusMap.Store(targetKey, task)
		}
		var p BuiltinPlatform
		switch targetPlatform {
		case "Douyin":
			p = &DouyinBuiltinPlatform{}
		case "Kuaishou":
			p = &KuaishouBuiltinPlatform{}
		case "Soop":
			p = &SoopBuiltinPlatform{}
		}
		if p != nil {
			wrapperStartMonitorIfNotRunning(p, targetRoom)
		}
		triggerBuiltinBroadcast()
		return fmt.Sprintf("▶️ 已成功恢复主播 <b>%s</b> 的监控任务。", safeTargetName)

	case "delete":
		builtinTaskStates.Store(targetKey, "deleted")
		if cancel, ok := builtinCancels.Load(targetKey); ok {
			cancel.(context.CancelFunc)()
		}
		syncBuiltinAnchorToTxt("delete", targetPlatform, targetRoom, "")
		builtinStatusMap.Delete(targetKey)
		builtinActiveTasks.Delete(targetKey)
		triggerBuiltinBroadcast()
		return fmt.Sprintf("🗑️ 已彻底删除主播 <b>%s</b> 的监控任务。", safeTargetName)
	}

	return "操作无效"
}

// tgHandleStatus 拉取底层系统资源监控。
// 职责：格式化提取系统的硬盘、内存及上传队列积压情况，拼装成易于审阅的面板报表。
func tgHandleStatus() string {
	stats := buildStatusData()
	queues := buildQueueData()

	uptime := stats["uptime"].(int64)
	h := uptime / 3600
	m := (uptime % 3600) / 60

	diskFreeMB := stats["diskFree"].(int64) / 1024 / 1024
	ffmpegMemMB := stats["ffmpegMem"].(int64) / 1024 / 1024

	var sb strings.Builder
	sb.WriteString("📊 <b>系统运行资源状态</b>\n\n")
	sb.WriteString(fmt.Sprintf("⏱️ 运行时间: <code>%d小时 %d分</code>\n", h, m))
	sb.WriteString(fmt.Sprintf("💾 磁盘剩余: <code>%d MB</code>\n", diskFreeMB))
	sb.WriteString(fmt.Sprintf("🧠 引擎内存: <code>%d MB</code>\n", ffmpegMemMB))
	sb.WriteString(fmt.Sprintf("⚙️ 工作线程: <code>%d</code>\n\n", stats["workers"]))

	sb.WriteString("📤 <b>上传队列积压</b>\n")
	sb.WriteString(fmt.Sprintf("├ 等待中: <code>%v</code>\n", queues["waiting"]))
	sb.WriteString(fmt.Sprintf("├ 传输中: <code>%v</code>\n", queues["uploading"]))
	sb.WriteString(fmt.Sprintf("├ 已成功: <code>%v</code>\n", queues["success"]))
	sb.WriteString(fmt.Sprintf("└ 失败量: <code>%v</code>\n", queues["failed"]))

	return sb.String()
}
