package recorder

import (
	"context"
	"net/http"
	"sync"
	"time"

	"upload/internal/config"
)

// cfgStore 统一配置仓库（config.json）；由 main 注入。
// 内置引擎不再单独维护 builtin_config.json / builtin_cookies.json。
var cfgStore *config.Store

// SetConfigStore 注入统一配置仓库（进程内一次）。
func SetConfigStore(s *config.Store) { cfgStore = s }

// ConfigStore 返回统一配置仓库（可能为 nil）。
func ConfigStore() *config.Store { return cfgStore }

// PersistConfig 把当前内置配置（含 cookies）写回统一 config.json。
func PersistConfig() error {
	if cfgStore == nil {
		return nil
	}
	snap := *Config()
	cfgStore.Update(func(c *config.Config) { c.Builtin = snap })
	return cfgStore.Save()
}

// Config 返回内置配置快照（永不返回 nil，未初始化时返回缺省值）。
// 返回的指针是只读快照，禁止原地修改；要改配置请用 UpdateConfig。
func Config() *BuiltinConfig {
	if p := builtinCfgPtr.Load(); p != nil {
		return p
	}
	def := BuiltinConfig{Quality: "uhd", CheckInterval: 30, SavePath: "./downloads"}
	def.ApplyDefaults()
	return &def
}

// SetConfig 整体替换内置配置（复制后原子发布，调用方后续改动不会影响已发布快照）。
func SetConfig(c *BuiltinConfig) {
	cp := BuiltinConfig{}
	if c != nil {
		cp = *c
	}
	cp.ApplyDefaults()
	builtinCfgPtr.Store(&cp)
}

// UpdateConfig 复制-修改-原子发布，保证并发读方永远看到完整结构。
// 返回发布后的新快照。
func UpdateConfig(fn func(*BuiltinConfig)) *BuiltinConfig {
	cp := *Config()
	fn(&cp)
	cp.ApplyDefaults()
	builtinCfgPtr.Store(&cp)
	return &cp
}

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

// ActiveTaskCount 当前活跃的录制监控协程数（停机日志用）。
func ActiveTaskCount() int {
	n := 0
	builtinActiveTasks.Range(func(_, _ interface{}) bool {
		n++
		return true
	})
	return n
}

// StopAllRecordings 优雅停机：置停机标记 + 取消所有进行中的录制会话，返回被通知的会话数。
//
// 两步都不能少：
//   - 置标记 → 监控协程在下一轮循环顶部退出。不置的话，RecordStream 收尾返回后
//     循环会立刻用新的 ctx 重开录制，停机就变成"反复重开"。
//   - 取消会话 → 正在 RecordStream 里阻塞的协程进入收尾（向 ffmpeg 发 q，
//     最多等 10s 封装；超时则 Kill）。
//
// 调用方随后必须调用 WaitActiveTasks 等待真正结束，再 os.Exit。
func StopAllRecordings() int {
	builtinShuttingDown.Store(true)
	n := 0
	builtinCancels.Range(func(_, v interface{}) bool {
		if cancel, ok := v.(context.CancelFunc); ok {
			cancel()
			n++
		}
		return true
	})
	return n
}

// WaitActiveTasks 等待所有录制监控协程退出，最多等 timeout，返回是否全部结束。
//
// 每个监控协程要等 RecordStream 收尾完（内含最多 10s 的 ffmpeg 封装等待）才退出，
// 所以 timeout 必须显著大于 10s。用于 os.Exit 之前——直接退出会把 ffmpeg 子进程
// 丢成孤儿继续录制（2026-09-26 实测：每次重启必产生一批）。
func WaitActiveTasks(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if ActiveTaskCount() == 0 {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// FFmpegBin ffmpeg 路径。
func FFmpegBin() string { return builtinFfmpegPath }

// SyncAnchorToTxt 名单文件增删改。
func SyncAnchorToTxt(action, platform, roomID, rawLine string) {
	syncBuiltinAnchorToTxt(action, platform, roomID, rawLine)
}

// TriggerBroadcast 触发任务列表广播。
func TriggerBroadcast() { triggerBuiltinBroadcast() }

// RestartActiveRecordings 取消正在录制的会话，监控循环会按新配置立即重开。
// 用于视频水印等“只在 ffmpeg 启动时生效”的开关热更新。
// 会先打「配置重载」标记，避免监控循环把它误判为断流（误报下播 + 30 秒退避）。
func RestartActiveRecordings() {
	builtinCancels.Range(func(k, v interface{}) bool {
		if key, ok := k.(string); ok {
			markConfigRestart(key)
		}
		if cancel, ok := v.(context.CancelFunc); ok {
			cancel()
		}
		return true
	})
}

// markConfigRestart 标记某任务因配置变更被主动中断。
func markConfigRestart(key string) { builtinConfigRestart.Store(key, true) }

// isConfigRestart 查询是否存在配置重载标记（不清除）。
func isConfigRestart(key string) bool {
	_, ok := builtinConfigRestart.Load(key)
	return ok
}

// clearConfigRestart 清除标记并返回此前是否存在。
func clearConfigRestart(key string) bool {
	_, ok := builtinConfigRestart.LoadAndDelete(key)
	return ok
}

// StartMonitor 启动/复用监控协程。
func StartMonitor(p BuiltinPlatform, roomID string) { wrapperStartMonitorIfNotRunning(p, roomID) }

// NewBuiltinPlatform 按平台名构造平台实现，未知平台返回 nil。
// 新增平台时在此登记，替换散落在各入口的 switch 注册。
func NewBuiltinPlatform(name string) BuiltinPlatform {
	switch name {
	case "Douyin":
		return &DouyinBuiltinPlatform{}
	case "Kuaishou":
		return &KuaishouBuiltinPlatform{}
	case "Soop":
		return &SoopBuiltinPlatform{}
	case "Bilibili":
		return &BilibiliBuiltinPlatform{}
	case "Twitch":
		return &TwitchBuiltinPlatform{}
	}
	return nil
}

// DouyinPlatform 抖音平台实现。
func DouyinPlatform() BuiltinPlatform { return &DouyinBuiltinPlatform{} }

// KuaishouPlatform 快手平台实现。
func KuaishouPlatform() BuiltinPlatform { return &KuaishouBuiltinPlatform{} }

// SoopPlatform Soop 平台实现。
func SoopPlatform() BuiltinPlatform { return &SoopBuiltinPlatform{} }

// BilibiliPlatform B 站平台实现。
func BilibiliPlatform() BuiltinPlatform { return &BilibiliBuiltinPlatform{} }

// TwitchPlatform Twitch 平台实现。
func TwitchPlatform() BuiltinPlatform { return &TwitchBuiltinPlatform{} }

// UpdateStatus 更新任务状态并通知。
func UpdateStatus(platform, roomID, anchorName, avatar, quality, statusMsg string) {
	updateBuiltinStatus(platform, roomID, anchorName, avatar, quality, statusMsg)
}

// ExtractCoverFromLocalFile 抽帧导出（水印跟随全局开关）。
func ExtractCoverFromLocalFile(dir, prefix, coverPath, anchorName string) bool {
	return extractBuiltinCoverFromLocalFile(dir, prefix, coverPath, anchorName, Config().WatermarkEnable)
}

// ProxyImage 封面反代。
func ProxyImage(w http.ResponseWriter, r *http.Request) { apiProxyImage(w, r) }
