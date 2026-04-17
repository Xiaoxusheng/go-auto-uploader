package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

/* ==========================================
 * 高性能 Telegram 机器人控制守护模块 (终极版)
 * 包含：全链路防假死网络引擎、动态分页 UI、MD5 按键指纹、OTA 逃逸更新、内存极速搜索
 * ========================================== */

var (
	tgBot     *tgbotapi.BotAPI
	tgEnabled bool
)

// InitTelegramBot 初始化 Telegram 机器人引擎。
// 职责：注入防假死网络客户端，读取全局配置，建立长连接，挂载异步监听，并自动注册原生快捷菜单栏。
func InitTelegramBot() {
	log.Println("[TG-BOT] 🚀 开始初始化 Telegram 引擎...")
	appConfigMu.RLock()
	token := appConfig.TelegramToken
	appConfigMu.RUnlock()

	if token == "" {
		log.Println("[TG-BOT] ⚠️ 未配置 Telegram Token，已跳过机器人初始化")
		return
	}

	// 核心网络优化：注入底层防挂死 HTTP Client
	// 针对代理环境容易切断长连接的问题，强制开启高频 TCP Keep-Alive 和严格超时
	customClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 15 * time.Second, // 强制 TCP 心跳，防止被代理路由杀后台
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		Timeout: 90 * time.Second, // 给整个 Client 加上超时上限，彻底杜绝僵尸死锁
	}

	var err error
	log.Println("[TG-BOT] 🔌 正在连接 Telegram 官方服务器...")

	// 使用自定义的强力 Client 替代默认的 http.DefaultClient
	tgBot, err = tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, customClient)
	if err != nil {
		log.Printf("[TG-BOT] ❌ 机器人连接失败 (可能网络不通): %v\n", err)
		return
	}

	tgEnabled = true
	log.Printf("[TG-BOT] ✅ 已成功接管机器人: @%s", tgBot.Self.UserName)

	go checkAndNotifyOTASuccess()
	go registerBotCommands()
	go startTelegramListener()
}

// registerBotCommands 自动化注册 Telegram 原生快捷菜单栏。
// 职责：向官方服务器写入命令列表，让用户输入框左下角永久出现 Menu 快捷入口。
func registerBotCommands() {
	log.Println("[TG-BOT] 📝 正在向官方注册快捷菜单...")
	commands := tgbotapi.NewSetMyCommands(
		tgbotapi.BotCommand{Command: "menu", Description: "🎛️ 打开可视化全能控制主面板"},
		tgbotapi.BotCommand{Command: "find", Description: "🔍 搜索主播以快速控制录制状态"},
		tgbotapi.BotCommand{Command: "list", Description: "📋 查看在线主播与截图"},
		tgbotapi.BotCommand{Command: "status", Description: "📊 查看服务器系统资源状态"},
		tgbotapi.BotCommand{Command: "help", Description: "📖 查看所有指令与使用指南"},
	)
	if _, err := tgBot.Request(commands); err != nil {
		log.Printf("[TG-BOT] ⚠️ 快捷菜单栏注册失败 (可忽略): %v\n", err)
	} else {
		log.Println("[TG-BOT] ✅ 快捷菜单注册成功")
	}
}

// checkAndNotifyOTASuccess 嗅探并发送 OTA 热更新成功回执。
// 职责：在 Systemd 逃逸拉起新进程后，读取隐藏的信标文件并向超管发送上线通知。
func checkAndNotifyOTASuccess() {
	flagPath := filepath.Join(".", ".ota_chat_id")
	flagData, err := os.ReadFile(flagPath)
	if err != nil {
		return
	}

	log.Printf("[TG-BOT] 🔍 发现 OTA 重启信标，准备发送成功回执...")
	chatIDToNotify, parseErr := strconv.ParseInt(strings.TrimSpace(string(flagData)), 10, 64)
	if parseErr == nil && chatIDToNotify != 0 {
		successMsg := tgbotapi.NewMessage(chatIDToNotify, "🎉 <b>[系统级通知]</b> OTA 热更新与 Systemd 服务重启已圆满完成！\n🚀 新版核心引擎现已接管系统，恢复最高性能运作。")
		successMsg.ParseMode = "HTML"
		tgBot.Send(successMsg)
		log.Printf("[TG-BOT] ✅ OTA 成功回执已发送至 ChatID: %d\n", chatIDToNotify)
	}
	os.Remove(flagPath)
}

// startTelegramListener 启动长轮询监听器。
// 职责：拦截文本、智能嗅探链接、处理文件更新，并极速响应 CallbackQuery 按钮点击，设置极短轮询超时防假死。
func startTelegramListener() {
	log.Println("[TG-BOT] 📡 开启长轮询，正在监听所有来自 Telegram 的事件...")
	u := tgbotapi.NewUpdate(0)

	// 核心网络优化：把等待超时从 60 秒降低到 15 秒
	// 抢在路由器的 NAT 连接闲置失效之前主动结束并重新发起请求
	u.Timeout = 15

	updates := tgBot.GetUpdatesChan(u)

	for update := range updates {
		appConfigMu.RLock()
		allowedChatID := appConfig.TelegramChatID
		appConfigMu.RUnlock()

		if update.CallbackQuery != nil {
			if allowedChatID != 0 && update.CallbackQuery.Message.Chat.ID != allowedChatID {
				log.Printf("[TG-BOT-DEBUG] ⛔ 非法越权按钮点击，已拦截\n")
				tgBot.Request(tgbotapi.NewCallback(update.CallbackQuery.ID, "⛔ 无权限操作"))
				continue
			}
			go handleTelegramCallback(update.CallbackQuery)
			continue
		}

		if update.Message == nil {
			continue
		}

		if allowedChatID != 0 && update.Message.Chat.ID != allowedChatID {
			log.Printf("[TG-BOT-DEBUG] ⛔ 拦截到陌生人发消息 (ChatID: %d)\n", update.Message.Chat.ID)
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "⛔ <b>[安全拦截]</b> 您没有权限控制此录制基站。")
			msg.ParseMode = "HTML"
			tgBot.Send(msg)
			continue
		}

		// ✨ 捕获 ForceReply 回复内容 (用于交互式搜索)
		if update.Message.ReplyToMessage != nil && strings.Contains(update.Message.ReplyToMessage.Text, "请输入要搜索的主播名称") {
			go tgHandleSearch(update.Message.Chat.ID, update.Message.Text)
			continue
		}

		if update.Message.Document != nil && strings.TrimSpace(update.Message.Caption) == "/update" {
			log.Println("[TG-BOT] 🚀 触发 OTA 热更新流程")
			go tgHandleUpdateBinary(update.Message)
			continue
		}

		if strings.Contains(update.Message.Text, "http://") || strings.Contains(update.Message.Text, "https://") {
			log.Println("[TG-BOT] 🔍 触发零指令链接嗅探添加...")
			go func() {
				chatID := update.Message.Chat.ID
				loadingMsg := tgbotapi.NewMessage(chatID, "⏳ 嗅探到链接，正在解析目标直播间特征...")
				sent, _ := tgBot.Send(loadingMsg)

				res := tgHandleAdd(update.Message.Text)

				if sent.MessageID != 0 {
					tgBot.Send(tgbotapi.NewDeleteMessage(chatID, sent.MessageID))
				}
				splitAndSendTelegramMsg(chatID, res)
			}()
			continue
		}

		go handleTelegramCommand(update.Message)
	}
}

// getTaskHash 为每个超长的内置录制 Key 生成 8 位 MD5 短指纹。
// 职责：完美压缩 Telegram Callback 数据长度，避开官方 64 字节截断导致的 UTF-8 乱码崩溃。
func getTaskHash(key string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(key)))[:8]
}

// handleTelegramCallback 路由动态按钮点击事件中心。
// 职责：解析多级菜单请求、处理分页参数，反向解析 MD5 指纹定位内存目标，并向下兼容旧版本面板的点击。
func handleTelegramCallback(query *tgbotapi.CallbackQuery) {
	chatID := query.Message.Chat.ID
	action := query.Data
	msgID := query.Message.MessageID

	switch {
	case action == "action_main":
		tgBot.Request(tgbotapi.NewCallback(query.ID, ""))
		tgEditToMainMenu(chatID, msgID)

	case action == "action_help":
		tgBot.Request(tgbotapi.NewCallback(query.ID, ""))
		tgHandleHelp(chatID, msgID)

	case action == "action_search": // ✨ 新增的搜索回调拦截
		tgBot.Request(tgbotapi.NewCallback(query.ID, "请在键盘输入关键字"))
		msg := tgbotapi.NewMessage(chatID, "🔍 <b>请输入要搜索的主播名称或房间号：</b>\n*(直接回复本条消息，或随时使用 <code>/find 关键字</code>)*")
		msg.ParseMode = "HTML"
		// 启用 ForceReply，让用户体验极其顺滑
		msg.ReplyMarkup = tgbotapi.ForceReply{ForceReply: true, Selective: true}
		tgBot.Send(msg)

	case action == "action_list":
		tgBot.Request(tgbotapi.NewCallback(query.ID, "正在抓取画面..."))
		tgHandleList(chatID)

	case action == "action_status":
		tgBot.Request(tgbotapi.NewCallback(query.ID, ""))
		splitAndSendTelegramMsg(chatID, tgHandleStatus())

	case action == "action_log":
		tgBot.Request(tgbotapi.NewCallback(query.ID, "正在生成日志包..."))
		tgHandleLog(chatID)

	case action == "action_add":
		tgBot.Request(tgbotapi.NewCallbackWithAlert(query.ID, "💡 智能添加模式已开启：\n\n无需任何指令，请直接将直播链接粘贴并发送给机器人即可！"))

	case action == "action_close":
		tgBot.Request(tgbotapi.NewCallback(query.ID, ""))
		tgBot.Send(tgbotapi.NewDeleteMessage(chatID, msgID))

	// 二级菜单展开逻辑 (智能兼容旧版无分页数据: menu_pause vs 新版: menu_pause_1)
	case strings.HasPrefix(action, "menu_"):
		parts := strings.Split(action, "_")
		if len(parts) >= 2 {
			targetAction := parts[1]
			page := 1 // 默认退回第一页
			if len(parts) >= 3 {
				page, _ = strconv.Atoi(parts[2])
			}
			tgBot.Request(tgbotapi.NewCallback(query.ID, ""))
			tgEditToTargetMenu(chatID, msgID, targetAction, page)
		}

	// 精确指令执行逻辑 (带指纹映射，防数据截断报错)
	case strings.HasPrefix(action, "do_"):
		parts := strings.Split(action, "_")
		var cmd, key string
		var page int = 1

		if len(parts) >= 4 {
			cmd = parts[1]
			p, err := strconv.Atoi(parts[2])
			if err == nil {
				// 新版指纹格式: do_pause_1_a1b2c3d4
				page = p
				hash := parts[3]
				// 内存反向暴搜匹配指纹
				builtinStatusMap.Range(func(k, v interface{}) bool {
					if getTaskHash(k.(string)) == hash {
						key = k.(string)
						return false
					}
					return true
				})
			} else {
				// 兼容极早期的非数字分页老按钮格式
				key = strings.Join(parts[2:], "_")
			}
		} else if len(parts) == 3 {
			// 兼容旧面板格式: do_pause_Douyin_123
			cmd = parts[1]
			key = parts[2]
		}

		if key == "" {
			tgBot.Request(tgbotapi.NewCallback(query.ID, "⚠️ 任务已过期或未找到"))
			return
		}

		var toastText string
		if cmd == "cover" {
			toastText = "正在后台提取画面..."
			tgBot.Request(tgbotapi.NewCallback(query.ID, toastText))
			tgHandlePreciseCover(chatID, key)
		} else {
			toastText = tgExecutePreciseControl(cmd, key)
			tgBot.Request(tgbotapi.NewCallback(query.ID, toastText))
			tgEditToTargetMenu(chatID, msgID, cmd, page)
		}
	default:
		tgBot.Request(tgbotapi.NewCallback(query.ID, "未知操作"))
	}
}

// tgEditToMainMenu 将现有消息就地刷新为九宫格主菜单。
// 职责：构建主界面的核心入口控制台键盘。
func tgEditToMainMenu(chatID int64, messageID int) {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 列表与画面", "action_list"),
			tgbotapi.NewInlineKeyboardButtonData("📊 资源与队列", "action_status"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📸 单独寻址截图", "menu_cover_1"),
			tgbotapi.NewInlineKeyboardButtonData("📁 导出系统日志", "action_log"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⏸️ 挂起指定", "menu_pause_1"),
			tgbotapi.NewInlineKeyboardButtonData("▶️ 恢复指定", "menu_resume_1"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("➕ 智能添加新主播", "action_add"),
			tgbotapi.NewInlineKeyboardButtonData("🔍 搜索与快捷操作", "action_search"), // ✨ 新增搜索控制流入口
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🗑️ 彻底删除指定", "menu_delete_1"),
			tgbotapi.NewInlineKeyboardButtonData("📖 使用指南", "action_help"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ 阅后即焚", "action_close"),
		),
	)

	msg := tgbotapi.NewEditMessageTextAndMarkup(chatID, messageID, "🎛️ <b>全自动直播录制引擎 - 主控制台</b>\n\n请点击下方功能按钮：", keyboard)
	msg.ParseMode = "HTML"
	if _, err := tgBot.Send(msg); err != nil {
		log.Printf("[TG-BOT] ⚠️ 刷新主菜单界面被拦截 (可能内容未变): %v\n", err)
	}
}

// MenuTask 用于存储从并发安全的 map 中提取出排序快照的结构体。
type MenuTask struct {
	Key      string
	Name     string
	IsPaused bool
}

// tgEditToTargetMenu 动态提取内存数据生成包含所有主播的二级子菜单面板。
// 职责：计算分页、截断过长字符串防止 UI 撑爆，并将 Key 哈希压缩成指纹后渲染导航栏。
func tgEditToTargetMenu(chatID int64, messageID int, action string, page int) {
	actionNameCN := ""
	switch action {
	case "cover":
		actionNameCN = "获取画面"
	case "pause":
		actionNameCN = "挂起"
	case "resume":
		actionNameCN = "恢复"
	case "delete":
		actionNameCN = "删除"
	}

	var allItems []MenuTask

	// 1. 提取全量数据
	builtinStatusMap.Range(func(k, v interface{}) bool {
		task := v.(*BuiltinTaskStatus)
		key := k.(string)

		name := task.AnchorName
		if name == "" || name == task.RoomID {
			name = "未命名_" + task.RoomID
		}

		allItems = append(allItems, MenuTask{
			Key:      key,
			Name:     name,
			IsPaused: task.IsPaused,
		})
		return true
	})

	// 2. 字母排序，防止翻页时数据乱跳
	sort.Slice(allItems, func(i, j int) bool {
		return allItems[i].Key < allItems[j].Key
	})

	// 3. 分页计算引擎 (每页限制 12 个，提供极佳手机版阅读体验)
	pageSize := 12
	totalItems := len(allItems)
	totalPages := (totalItems + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	startIdx := (page - 1) * pageSize
	endIdx := startIdx + pageSize
	if endIdx > totalItems {
		endIdx = totalItems
	}

	pageItems := allItems[startIdx:endIdx]

	var rows [][]tgbotapi.InlineKeyboardButton

	for _, item := range pageItems {
		nameRunes := []rune(item.Name)
		safeName := item.Name
		if len(nameRunes) > 12 { // 截断超长名字防止撑破 UI
			safeName = string(nameRunes[:11]) + ".."
		}

		btnText := ""
		switch action {
		case "cover":
			btnText = "📸 截取: " + safeName
		case "pause":
			if item.IsPaused {
				btnText = "🔘 [已挂起] " + safeName
			} else {
				btnText = "⏸️ 挂起: " + safeName
			}
		case "resume":
			if !item.IsPaused {
				btnText = "🟢 [录制中] " + safeName
			} else {
				btnText = "▶️ 恢复: " + safeName
			}
		case "delete":
			btnText = "🗑️ 删除: " + safeName
		}

		// 核心保护：利用 MD5 生成绝对短指纹，绝不超时 64 Bytes 触发 Telegram 宕机异常
		callbackData := fmt.Sprintf("do_%s_%d_%s", action, page, getTaskHash(item.Key))

		btn := tgbotapi.NewInlineKeyboardButtonData(btnText, callbackData)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(btn))
	}

	if totalItems == 0 {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📭 当前监控队列为空", "ignore"),
		))
	}

	// 5. 组装底部翻页导航器
	var navRow []tgbotapi.InlineKeyboardButton
	if page > 1 {
		navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("⬅️ 上一页", fmt.Sprintf("menu_%s_%d", action, page-1)))
	}
	navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%d / %d", page, totalPages), "ignore"))
	if page < totalPages {
		navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("下一页 ➡️", fmt.Sprintf("menu_%s_%d", action, page+1)))
	}

	if len(navRow) > 0 {
		rows = append(rows, navRow)
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 返回主控制台", "action_main"),
	))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)
	text := fmt.Sprintf("🎯 <b>对象选择面板</b>\n当前指令模式：<b>%s</b>\n队列总数：<b>%d</b> 人\n\n请点击目标：", actionNameCN, totalItems)
	msg := tgbotapi.NewEditMessageTextAndMarkup(chatID, messageID, text, keyboard)
	msg.ParseMode = "HTML"
	if _, err := tgBot.Send(msg); err != nil {
		log.Printf("[TG-BOT-DEBUG] ℹ️ 面板刷新可能因内容未变化而被静默: %v\n", err)
	}
}

// tgExecutePreciseControl 直接底层操作目标内存键值的控制中心。
// 职责：极速修改底层状态机，挂起/恢复引擎核心，并广播通知。
func tgExecutePreciseControl(action, key string) string {
	log.Printf("[TG-BOT] 🛠️ 正在执行系统控制: 行动=%s, Key=%s\n", action, key)
	value, exists := builtinStatusMap.Load(key)
	if !exists {
		return "⚠️ 该任务已不存在"
	}

	task := value.(*BuiltinTaskStatus)
	targetPlatform := task.Platform
	targetRoom := task.RoomID

	switch action {
	case "pause":
		builtinTaskStates.Store(key, "paused")
		if cancel, ok := builtinCancels.Load(key); ok {
			cancel.(context.CancelFunc)()
		}
		syncBuiltinAnchorToTxt("pause", targetPlatform, targetRoom, "")
		task.IsPaused = true
		task.Status = "已暂停"
		builtinStatusMap.Store(key, task)
		triggerBuiltinBroadcast()
		log.Println("[TG-BOT] ✅ 成功下发挂起指令")
		return "✅ 挂起指令下发成功"

	case "resume":
		builtinTaskStates.Store(key, "running")
		syncBuiltinAnchorToTxt("resume", targetPlatform, targetRoom, "")
		task.IsPaused = false
		task.Status = "监控中"
		builtinStatusMap.Store(key, task)

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
		log.Println("[TG-BOT] ✅ 成功下发唤醒指令")
		return "✅ 唤醒指令下发成功"

	case "delete":
		builtinTaskStates.Store(key, "deleted")
		if cancel, ok := builtinCancels.Load(key); ok {
			cancel.(context.CancelFunc)()
		}
		syncBuiltinAnchorToTxt("delete", targetPlatform, targetRoom, "")
		builtinStatusMap.Delete(key)
		builtinActiveTasks.Delete(key)
		triggerBuiltinBroadcast()
		log.Println("[TG-BOT] 🗑️ 成功彻底删除指令")
		return "🗑️ 彻底销毁成功"
	}

	return "操作无效"
}

// tgHandlePreciseCover 从精准提取内存 Key 读取封面并下发。
// 职责：在内存中找到主播数据，从文件系统读取无损截图并作为 Photo 发送。
func tgHandlePreciseCover(chatID int64, key string) {
	log.Printf("[TG-BOT] 📸 正在寻址获取封面: %s\n", key)
	value, exists := builtinStatusMap.Load(key)
	if !exists {
		return
	}
	task := value.(*BuiltinTaskStatus)

	fileName := fmt.Sprintf("%s_%s.png", task.Platform, task.RoomID)
	coverPath := filepath.Join(".", "covers", fileName)

	info, err := os.Stat(coverPath)
	if err != nil || info.Size() == 0 {
		log.Printf("[TG-BOT-DEBUG] ⚠️ 获取封面失败: %s 路径无效或无数据\n", coverPath)
		splitAndSendTelegramMsg(chatID, fmt.Sprintf("📭 主播 <b>%s</b> 目前暂无有效的实时画面截图。", html.EscapeString(task.AnchorName)))
		return
	}

	go func() {
		log.Printf("[TG-BOT-DEBUG] 📤 正在上传封装封面文件: %s...\n", coverPath)
		photo := tgbotapi.NewPhoto(chatID, tgbotapi.FilePath(coverPath))
		photo.Caption = fmt.Sprintf("📸 <b>%s</b> 实时画面", html.EscapeString(task.AnchorName))
		photo.ParseMode = "HTML"
		tgBot.Send(photo)
	}()
}

// splitAndSendTelegramMsg 智能分片发送引擎（针对超长文本优化）。
// 职责：安全切割并发送长文本，避免破坏 HTML 标签，保证数据不丢弃。
func splitAndSendTelegramMsg(chatID int64, text string) {
	const maxLen = 4000
	if len(text) <= maxLen {
		msg := tgbotapi.NewMessage(chatID, text)
		msg.ParseMode = "HTML"
		msg.DisableWebPagePreview = true
		if _, err := tgBot.Send(msg); err != nil {
			log.Printf("[TG-BOT-DEBUG] ⚠️ 发送消息 (HTML) 失败，尝试降级为纯文本兜底: %v\n", err)
			msg.ParseMode = ""
			tgBot.Send(msg)
		}
		return
	}

	lines := strings.Split(text, "\n")
	var currentChunk strings.Builder

	for _, line := range lines {
		if currentChunk.Len()+len(line)+1 > maxLen {
			msg := tgbotapi.NewMessage(chatID, currentChunk.String())
			msg.ParseMode = "HTML"
			msg.DisableWebPagePreview = true
			tgBot.Send(msg)
			currentChunk.Reset()
		}
		currentChunk.WriteString(line + "\n")
	}

	if currentChunk.Len() > 0 {
		msg := tgbotapi.NewMessage(chatID, currentChunk.String())
		msg.ParseMode = "HTML"
		msg.DisableWebPagePreview = true
		tgBot.Send(msg)
	}
}

// handleTelegramCommand 路由文本指令。
// 职责：引导用户使用全新的 UI 面板，解析兼容老用户的习惯，并新增搜索引擎接入点。
func handleTelegramCommand(message *tgbotapi.Message) {
	cmd := message.Command()
	chatID := message.Chat.ID

	log.Printf("[TG-BOT-DEBUG] ⚙️ 收到指令: /%s \n", cmd)

	switch cmd {
	case "start", "menu":
		msg := tgbotapi.NewMessage(chatID, "初始化面板引擎中...")
		sentMsg, _ := tgBot.Send(msg)
		tgEditToMainMenu(chatID, sentMsg.MessageID)

	case "find", "search": // ✨ 拦截搜索指令
		keyword := message.CommandArguments()
		if keyword == "" {
			msg := tgbotapi.NewMessage(chatID, "🔍 请输入要搜索的关键字。例如: <code>/find 张三</code>")
			msg.ParseMode = "HTML"
			tgBot.Send(msg)
		} else {
			tgHandleSearch(chatID, keyword)
		}

	case "help":
		msg := tgbotapi.NewMessage(chatID, "正在生成使用说明面板...")
		sentMsg, _ := tgBot.Send(msg)
		tgHandleHelp(chatID, sentMsg.MessageID)

	case "list":
		tgHandleList(chatID)

	case "status":
		splitAndSendTelegramMsg(chatID, tgHandleStatus())

	case "log":
		tgHandleLog(chatID)

	case "add", "cover", "pause", "resume", "delete":
		splitAndSendTelegramMsg(chatID, "💡 <b>交互全面升级：</b>\n已不再需要手动输入主播名字或参数。\n\n▶️ 请直接使用 <code>/menu</code> 唤出可视化控制台，点击对应的按钮进行一键操作即可！\n*(如果是添加主播，请直接把直播链接发送给我)*")

	default:
		if cmd != "" {
			splitAndSendTelegramMsg(chatID, "❓ 无法识别指令。请直接使用 <code>/menu</code> 打开可视化控制面板。")
		}
	}
}

// tgHandleSearch 高性能内存搜索引擎。 (✨ 全新增加)
// 职责：通过关键字忽略大小写无锁极速遍历内存状态表，渲染出自带停止/恢复按钮的结果面板。
func tgHandleSearch(chatID int64, keyword string) {
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	log.Printf("[TG-BOT] 🔍 执行主播高速搜索: %s\n", keyword)

	var results []MenuTask

	// 1. 无锁内存极速遍历
	builtinStatusMap.Range(func(k, v interface{}) bool {
		task := v.(*BuiltinTaskStatus)
		key := k.(string)
		name := task.AnchorName
		if name == "" || name == task.RoomID {
			name = "未命名_" + task.RoomID
		}

		// 忽略大小写智能匹配名字或房间号
		if strings.Contains(strings.ToLower(name), keyword) || strings.Contains(strings.ToLower(task.RoomID), keyword) {
			results = append(results, MenuTask{
				Key:      key,
				Name:     name,
				IsPaused: task.IsPaused,
			})
		}

		// 高性能容量截断：防止因搜索词太短（如 "a"）导致 Telegram 的 UI 组件崩溃
		if len(results) >= 20 {
			return false // 终止遍历
		}
		return true
	})

	if len(results) == 0 {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("📭 未找到包含关键字 <b>%s</b> 的主播。", html.EscapeString(keyword)))
		msg.ParseMode = "HTML"
		tgBot.Send(msg)
		return
	}

	// 2. 保证每次呈现的顺序稳定
	sort.Slice(results, func(i, j int) bool {
		return results[i].Name < results[j].Name
	})

	// 3. 动态组装快捷操作按钮
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, item := range results {
		safeName := item.Name
		nameRunes := []rune(item.Name)
		if len(nameRunes) > 15 {
			safeName = string(nameRunes[:14]) + ".."
		}

		btnText := ""
		callbackData := ""

		// 智能判断状态，提供最迫切需要的操作
		if item.IsPaused {
			btnText = "▶️ 恢复录制: " + safeName
			callbackData = fmt.Sprintf("do_resume_1_%s", getTaskHash(item.Key))
		} else {
			btnText = "⏸️ 停止录制: " + safeName
			callbackData = fmt.Sprintf("do_pause_1_%s", getTaskHash(item.Key))
		}

		btn := tgbotapi.NewInlineKeyboardButtonData(btnText, callbackData)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(btn))
	}

	// 添加返回主菜单按钮形成闭环
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 返回主控制台", "action_main"),
	))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)
	text := fmt.Sprintf("🔍 <b>搜索结果: %s</b>\n共找到 <b>%d</b> 个主播，请点击对应按钮快速操作：", html.EscapeString(keyword), len(results))
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.ReplyMarkup = keyboard
	tgBot.Send(msg)
}

// tgHandleHelp 生成并展示全局指令与使用说明面板。
// 职责：详细列出机器人的所有文本指令、智能交互模式以及按钮使用规则，并提供返回主面板的导航键。
func tgHandleHelp(chatID int64, messageID int) {
	text := `📖 <b>系统功能与全指令使用指南</b>

🤖 <b>快捷基础指令</b>
<code>/menu</code> 或 <code>/start</code> - 唤起可视化全能主控制台
<code>/find [关键字]</code> - 🔍 <b>新增</b>：极速搜索主播并快速挂起或恢复
<code>/list</code> - 极速获取当前在线录制的主播列表与实时画面截图
<code>/status</code> - 实时查看服务器资源负载及上传队列积压情况
<code>/log</code> - 导出底层系统最新运行内存日志包
<code>/help</code> - 显示此操作说明

💡 <b>智能无指令交互</b>
🔗 <b>快速添加主播</b>：无需任何繁琐指令，直接将 <code>抖音</code>、<code>快手</code> 或 <code>Soop</code> 的直播间长/短链接发送给机器人，引擎将自动嗅探、解析并将其添加入监控队列。
🔄 <b>OTA 热更新</b>：直接将新版编译好的核心引擎二进制文件发送给机器人，并在文本说明(Caption)处填写 <code>/update</code>，即可无缝触发底层逃逸与原子替换重启。

🎛️ <b>控制台功能指引</b>
通过 <code>/menu</code> 打开面板后，您可以点击对应按钮进行图形化操作，包括：<b>挂起、恢复、彻底删除</b>特定任务，或单独拉取指定主播的最新<b>实时截图</b>。
*(系统所有按钮交互均采用 MD5 指纹安全寻址，绝对防碰撞与长文本截断)*`

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 返回主控制台", "action_main"),
		),
	)

	msg := tgbotapi.NewEditMessageTextAndMarkup(chatID, messageID, text, keyboard)
	msg.ParseMode = "HTML"
	if _, err := tgBot.Send(msg); err != nil {
		log.Printf("[TG-BOT-DEBUG] ⚠️ 说明面板刷新异常: %v\n", err)
	}
}

// SendTelegramNotification 异步全局消息投递器。
// 职责：格式化系统警报与上下播通知进行安全下发。
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
	} else if strings.Contains(title, "异常") || strings.Contains(title, "失败") {
		icon = "🔴"
	}

	cleanTitle := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(title, "▶️ ", ""), "⏹️ ", ""), "🛑 ", ""), "⏸️ ", "")

	safeTitle := html.EscapeString(cleanTitle)
	safeBody := html.EscapeString(body)
	text := fmt.Sprintf("%s <b>%s</b>\n\n%s", icon, safeTitle, safeBody)

	splitAndSendTelegramMsg(chatID, text)
}

// tgHandleUpdateBinary 接收文档，执行无缝热替换与 CGroup 逃逸重启。
// 职责：原生网络包下载、原子覆盖写入，以及 systemd-run 脱离主进程进行安全重启。
func tgHandleUpdateBinary(message *tgbotapi.Message) {
	chatID := message.Chat.ID
	doc := message.Document

	log.Printf("[TG-BOT] 🛠️ 正在处理 OTA 文件下载: %s\n", doc.FileName)

	msg := tgbotapi.NewMessage(chatID, "📥 <b>[OTA 热更新]</b> 正在从安全信道拉取新版核心引擎:\n<code>"+html.EscapeString(doc.FileName)+"</code> ...")
	msg.ParseMode = "HTML"
	statusMsg, _ := tgBot.Send(msg)

	fileURL, err := tgBot.GetFileDirectURL(doc.FileID)
	if err != nil {
		log.Printf("[TG-BOT-DEBUG] ❌ OTA 获取直链失败: %v\n", err)
		splitAndSendTelegramMsg(chatID, "❌ 获取下载链接失败: "+err.Error())
		return
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(fileURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		log.Printf("[TG-BOT-DEBUG] ❌ OTA 网络下载失败或超时: %v\n", err)
		splitAndSendTelegramMsg(chatID, "❌ 下载失败。")
		return
	}
	defer resp.Body.Close()

	exePath, err := os.Executable()
	if err != nil {
		splitAndSendTelegramMsg(chatID, "❌ 无法定位路径: "+err.Error())
		return
	}

	tmpPath := exePath + ".tmp"
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		splitAndSendTelegramMsg(chatID, "❌ 写入权限不足: "+err.Error())
		return
	}
	_, err = io.Copy(out, resp.Body)
	out.Close()

	bakPath := exePath + ".bak"
	os.Remove(bakPath)
	os.Rename(exePath, bakPath)
	if err = os.Rename(tmpPath, exePath); err != nil {
		log.Printf("[TG-BOT-DEBUG] ❌ OTA 原子重命名失败: %v\n", err)
		os.Rename(bakPath, exePath)
		splitAndSendTelegramMsg(chatID, "❌ 原子覆盖失败: "+err.Error())
		return
	}

	log.Printf("[TG-BOT] ✅ OTA 二进制替换成功，写入信标并准备逃逸重启...\n")
	flagPath := filepath.Join(filepath.Dir(exePath), ".ota_chat_id")
	os.WriteFile(flagPath, []byte(fmt.Sprintf("%d", chatID)), 0644)

	successText := "✅ <b>[OTA 替换就绪]</b> 新版本核心已完成原子替换！\n\n🔄 正在通过 Systemd 逃逸机制执行重启脚本...\n请等待稍后的成功上线回执。"
	if statusMsg.MessageID != 0 {
		editMsg := tgbotapi.NewEditMessageText(chatID, statusMsg.MessageID, successText)
		editMsg.ParseMode = "HTML"
		tgBot.Send(editMsg)
	}

	// 终极防杀方案：使用 systemd-run 逃逸 CGroup 防止在 restart 中被连带杀死
	go func() {
		time.Sleep(2 * time.Second)
		scriptDir := filepath.Dir(exePath)
		scriptPath := filepath.Join(scriptDir, "restart.sh")

		cmd := exec.Command("systemd-run", "--unit=uploader-ota-task", "bash", scriptPath)
		cmd.Dir = scriptDir
		if err := cmd.Start(); err != nil {
			log.Printf("[TG-BOT-DEBUG] ❌ systemd-run 触发失败，紧急自尽重启: %v\n", err)
			os.Exit(0)
		}
	}()
}

// tgHandleList 获取内存快照遍历发送截图。
// 职责：提取活跃录制数据，引入微秒休眠解决发送过多图片被官方封禁 Flood Wait 的危险。
func tgHandleList(chatID int64) string {
	log.Printf("[TG-BOT] 📋 开始处理全员画面抓取请求...\n")
	tasks := GetBuiltinRecorderTasks()
	var onlineTasks []BuiltinTaskStatus

	for _, t := range tasks {
		if t.Status == "录制中" {
			onlineTasks = append(onlineTasks, t)
		}
	}

	if len(onlineTasks) == 0 {
		return "📭 当前监控引擎中没有正在录制的主播。"
	}

	summaryMsg := fmt.Sprintf("📋 <b>当前在线录制名单</b> (共 %d 个)\n⏳ 正在提取实时截图...", len(onlineTasks))
	msg := tgbotapi.NewMessage(chatID, summaryMsg)
	msg.ParseMode = "HTML"
	tgBot.Send(msg)

	go func() {
		log.Printf("[TG-BOT-DEBUG] ⏳ 正在后台异步推送 %d 张截图，启用防风控节流引擎...\n", len(onlineTasks))
		for _, t := range onlineTasks {
			name := t.AnchorName
			if name == "" || name == t.RoomID {
				name = "未命名_" + t.RoomID
			}

			caption := fmt.Sprintf("🔴 <b>%s</b> [%s]\n├ 耗时: %s\n└ 大小: %s", html.EscapeString(name), html.EscapeString(t.Platform), html.EscapeString(t.Duration), html.EscapeString(t.FileSize))

			fileName := fmt.Sprintf("%s_%s.png", t.Platform, t.RoomID)
			coverPath := filepath.Join(".", "covers", fileName)

			if info, err := os.Stat(coverPath); err == nil && info.Size() > 0 {
				photo := tgbotapi.NewPhoto(chatID, tgbotapi.FilePath(coverPath))
				photo.Caption = caption
				photo.ParseMode = "HTML"
				tgBot.Send(photo)
			} else {
				textMsg := tgbotapi.NewMessage(chatID, caption+"\n<i>(画面初始化中...)</i>")
				textMsg.ParseMode = "HTML"
				tgBot.Send(textMsg)
			}
			time.Sleep(300 * time.Millisecond) // 防风控节流
		}
		log.Printf("[TG-BOT] ✅ 全部截图推送完成\n")
	}()

	return ""
}

// tgHandleAdd 接收无前缀长短链接引擎注入。
// 职责：支持各类长短链接、格式不规范链接的去重解析。
func tgHandleAdd(rawArgs string) string {
	log.Printf("[TG-BOT] ➕ 处理添加主播请求: %s\n", rawArgs)
	lines := strings.Split(rawArgs, "\n")
	added := 0

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		shortURLRe := regexp.MustCompile(`https?://v\.douyin\.com/[a-zA-Z0-9]+/?`)
		if shortURLRe.MatchString(line) {
			if realURL, err := ExtractBuiltinDouyinLiveURL(line); err == nil && realURL != "" {
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
		log.Printf("[TG-BOT] ✅ 成功解析并添加了 %d 个录制任务！\n", added)
		return fmt.Sprintf("✅ 成功解析并添加了 %d 个录制任务！", added)
	}
	return "❌ 解析失败，或该主播已存在于队列中。"
}

// tgHandleLog 提取常驻内存系统运行日志组装投递。
// 职责：极速组装底层的环形日志缓冲，实现不接触磁盘的高性能输出。
func tgHandleLog(chatID int64) string {
	log.Printf("[TG-BOT] 📁 正在导出系统底层内存日志...\n")
	go func() {
		var buf bytes.Buffer
		buf.Grow(1024 * 1024)

		logsMu.RLock()
		for _, entry := range logs {
			buf.WriteString(fmt.Sprintf("[%s] [%s] %s\n", entry.Time, entry.Level, entry.Message))
			if entry.Error != "" {
				buf.WriteString(fmt.Sprintf("  Error: %s\n", entry.Error))
			}
		}
		logsMu.RUnlock()

		if buf.Len() == 0 {
			splitAndSendTelegramMsg(chatID, "📭 当前内存中没有任何系统日志数据。")
			return
		}

		fileName := fmt.Sprintf("system_logs_%s.txt", time.Now().Format("20060102_150405"))
		fileBytes := tgbotapi.FileBytes{Name: fileName, Bytes: buf.Bytes()}

		doc := tgbotapi.NewDocument(chatID, fileBytes)
		doc.Caption = "📁 最新内存运行日志快照。"
		tgBot.Send(doc)
		log.Printf("[TG-BOT] ✅ 日志导出并推送成功\n")
	}()
	return ""
}

// tgHandleStatus 拉取底层监控格式化排版。
// 职责：向终端呈现直观的资源堆栈负载情况。
func tgHandleStatus() string {
	log.Printf("[TG-BOT] 📊 正在汇聚系统监控面板资源数据...\n")
	stats := buildStatusData()
	queues := buildQueueData()

	uptime := stats["uptime"].(int64)
	h := uptime / 3600
	m := (uptime % 3600) / 60

	var sb strings.Builder
	sb.WriteString("📊 <b>系统运行资源状态</b>\n\n")
	sb.WriteString(fmt.Sprintf("⏱️ 运行时间: <code>%d小时 %d分</code>\n", h, m))
	sb.WriteString(fmt.Sprintf("💾 磁盘剩余: <code>%d MB</code>\n", stats["diskFree"].(int64)/1024/1024))
	sb.WriteString(fmt.Sprintf("🧠 引擎内存: <code>%d MB</code>\n", stats["ffmpegMem"].(int64)/1024/1024))
	sb.WriteString(fmt.Sprintf("⚙️ 工作线程: <code>%d</code>\n\n", stats["workers"]))

	sb.WriteString("📤 <b>上传队列积压</b>\n")
	sb.WriteString(fmt.Sprintf("├ 等待中: <code>%v</code>\n", queues["waiting"]))
	sb.WriteString(fmt.Sprintf("├ 传输中: <code>%v</code>\n", queues["uploading"]))
	sb.WriteString(fmt.Sprintf("├ 已成功: <code>%v</code>\n", queues["success"]))
	sb.WriteString(fmt.Sprintf("└ 失败量: <code>%v</code>\n", queues["failed"]))

	return sb.String()
}
