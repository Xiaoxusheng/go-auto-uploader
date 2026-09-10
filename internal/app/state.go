// Package app 持有上传编排共享状态与生命周期（扫描 / Worker / 持久化 / 报告）。
package app

import (
	"context"
	"sync"
	"sync/atomic"

	"upload/internal/config"
	"upload/internal/hashstore"
	"upload/internal/logx"
	"upload/internal/remote"
	"upload/internal/storage"
	"upload/internal/uploader"
)

const (
	// SafeBaseDir 远端安全前缀
	SafeBaseDir = "/home/_safe_uploads"
	// SuccessLogFile 成功日志
	SuccessLogFile = "upload_success.json"
	// DirStatusFile 目录状态
	DirStatusFile = "dir_status.json"
	// HashFile 秒传哈希库
	HashFile = "uploaded_hash.db"
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
	SuccessStore   = storage.NewSuccessStore(SuccessLogFile, 500000)
	DirStatusStore = storage.NewDirStatusStore(DirStatusFile)

	TaskQueue  = uploader.NewQueue(100000)
	UploadPool *uploader.WorkerPool
	HashDB     *hashstore.Store
	RemoteCli  *remote.OpenListClient

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

	StartTimeUnix int64
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

// IncFail 熔断计数 +1。
func IncFail() int32 { return atomic.AddInt32(&ConsecFail, 1) }

// ResetFail 熔断清零。
func ResetFail() { atomic.StoreInt32(&ConsecFail, 0) }
