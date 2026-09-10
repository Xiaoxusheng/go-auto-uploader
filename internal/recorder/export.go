package recorder

import (
	"net/http"
	"sync"
)

// Config 当前内置配置指针（可变字段可直接改）。
func Config() *BuiltinConfig { return builtinConfig }

// SetConfig 整体替换配置指针。
func SetConfig(c *BuiltinConfig) { builtinConfig = c }

// StatusMap 任务状态表。
func StatusMap() *sync.Map { return &builtinStatusMap }

// TaskStates 运行状态（running/paused/deleted）。
func TaskStates() *sync.Map { return &builtinTaskStates }

// Cancels 取消函数表。
func Cancels() *sync.Map { return &builtinCancels }

// CustomNames 自定义主播名表。
func CustomNames() *sync.Map { return &builtinCustomNames }

// ActiveTasks 活跃监控表。
func ActiveTasks() *sync.Map { return &builtinActiveTasks }

// FFmpegBin ffmpeg 路径。
func FFmpegBin() string { return builtinFfmpegPath }

// SyncAnchorToTxt 名单文件增删改。
func SyncAnchorToTxt(action, platform, roomID, rawLine string) {
	syncBuiltinAnchorToTxt(action, platform, roomID, rawLine)
}

// TriggerBroadcast 触发任务列表广播。
func TriggerBroadcast() { triggerBuiltinBroadcast() }

// StartMonitor 启动/复用监控协程。
func StartMonitor(p BuiltinPlatform, roomID string) { wrapperStartMonitorIfNotRunning(p, roomID) }

// DouyinPlatform 抖音平台实现。
func DouyinPlatform() BuiltinPlatform { return &DouyinBuiltinPlatform{} }

// KuaishouPlatform 快手平台实现。
func KuaishouPlatform() BuiltinPlatform { return &KuaishouBuiltinPlatform{} }

// SoopPlatform Soop 平台实现。
func SoopPlatform() BuiltinPlatform { return &SoopBuiltinPlatform{} }

// UpdateStatus 更新任务状态并通知。
func UpdateStatus(platform, roomID, anchorName, avatar, quality, statusMsg string) {
	updateBuiltinStatus(platform, roomID, anchorName, avatar, quality, statusMsg)
}

// ExtractCoverFromLocalFile 抽帧导出。
func ExtractCoverFromLocalFile(dir, prefix, coverPath, anchorName string) bool {
	return extractBuiltinCoverFromLocalFile(dir, prefix, coverPath, anchorName)
}

// ProxyImage 封面反代。
func ProxyImage(w http.ResponseWriter, r *http.Request) { apiProxyImage(w, r) }
