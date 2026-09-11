// Package app 持有上传编排共享状态与生命周期（扫描 / Worker / 持久化 / 报告）。
package app

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"upload/internal/config"
	"upload/internal/hashstore"
	"upload/internal/logx"
	"upload/internal/remote"
	"upload/internal/storage"
	"upload/internal/uploader"
	"upload/internal/ws"
)

const (
	// SafeBaseDir 远端安全前缀
	SafeBaseDir = "/home/_safe_uploads"

	// 运行时数据文件的基名（实际落盘位置 = config.dataDir 目录下）
	successLogName = "upload_success.json"
	dirStatusName  = "dir_status.json"
	hashName       = "uploaded_hash.db"
)

var (
	CfgStore = config.NewStore("config.json")
	AppLogs  = logx.NewStore(5000, 1000)

	LiveTasks      sync.Map
	QueueUploading sync.Map
	QueueSuccess   sync.Map
	QueueFail      sync.Map
	QueueRetrying  sync.Map

	QueueUploadingCount int64
	QueueSuccessCount   int64
	QueueFailCount      int64
	QueueRetryingCount  int64

	HistoryStore   = storage.NewHistoryStore(1000)
	SuccessStore   = storage.NewSuccessStore(successLogName, 500000)
	DirStatusStore = storage.NewDirStatusStore(dirStatusName)

	TaskQueue  = uploader.NewQueue(100000)
	UploadPool *uploader.WorkerPool
	// HashDB 秒传哈希库。这里就创建实例（Run 只 Repath 不重建），
	// 以免其它包 init 阶段捕获到 nil 或失效引用。
	HashDB    = hashstore.New(hashName)
	RemoteCli *remote.OpenListClient

	// HTTPCli 连接池（OpenList / PushPlus）
	HTTPCli = &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	AppCtx    context.Context
	AppCancel context.CancelFunc

	QueueCount   int64
	ActiveWorker int64

	DynInterval  int64
	NextScanUnix int64
	ConsecFail   int32

	TriggerScanCh   = make(chan string, 1)
	TriggerReportCh = make(chan struct{}, 1)

	Running   bool
	RunningMu sync.RWMutex

	StartTime time.Time

	DashUser string
	DashPass string

	// WSHub 控制台广播
	WSHub *ws.Hub

	// Pipeline 上传编排
	Pipeline *uploader.Pipeline

	// BuiltinActiveNamesHook 返回内置引擎当前正在录制的主播名（已清洗，由 main 注入）
	BuiltinActiveNamesHook func() []string

	// FFmpegPathHook 返回 ffmpeg 可执行文件路径
	FFmpegPathHook func() string
)

// AppCfg 配置快照。
func AppCfg() config.Config { return CfgStore.Get() }

// TriggerScan 强制扫描。
func TriggerScan(reason string) {
	select {
	case TriggerScanCh <- reason:
	default:
	}
}

// TriggerReportReset 重置报告倒计时。
func TriggerReportReset() {
	select {
	case TriggerReportCh <- struct{}{}:
	default:
	}
}

// MarkDirty 目录状态脏标记。
func MarkDirty() { DirStatusStore.MarkDirty() }

// IsRunning 是否在跑。
func IsRunning() bool {
	RunningMu.RLock()
	defer RunningMu.RUnlock()
	return Running
}

// SetRunning 设置运行开关。
func SetRunning(v bool) {
	RunningMu.Lock()
	Running = v
	RunningMu.Unlock()
}

// SetStartTime 记录启动时刻。
func SetStartTime(t time.Time) {
	RunningMu.Lock()
	StartTime = t
	RunningMu.Unlock()
}

// UptimeSeconds 运行秒数。
func UptimeSeconds() int64 {
	RunningMu.RLock()
	st := StartTime
	RunningMu.RUnlock()
	if st.IsZero() {
		return 0
	}
	return int64(time.Since(st).Seconds())
}

// IncFail 熔断计数 +1。
func IncFail() int32 { return atomic.AddInt32(&ConsecFail, 1) }

// ResetFail 熔断清零。
func ResetFail() { atomic.StoreInt32(&ConsecFail, 0) }
