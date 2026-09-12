package app

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"upload/internal/remote"
	"upload/internal/storage"
)

// Task 单个上传任务运行时状态。
type Task struct {
	Mu         sync.RWMutex
	ID         string
	Name       string
	Path       string
	Remote     string
	Size       int64
	Progress   int
	Speed      int64
	WorkerID   int
	Status     string
	RetryCount int
	CreatedAt  time.Time
	EndTime    time.Time
	Error      string
}

// isRemoteNameConflict 判断远端拒绝原因是否为「同名文件已存在」。
// OpenList/alist 系返回 500 + 中文文案，个别驱动返回英文，做宽松匹配。
func isRemoteNameConflict(msg string) bool {
	if msg == "" {
		return false
	}
	lower := strings.ToLower(msg)
	return strings.Contains(msg, "文件名冲突") ||
		strings.Contains(msg, "已存在同名文件") ||
		strings.Contains(msg, "同名文件") ||
		strings.Contains(lower, "file name conflict") ||
		strings.Contains(lower, "filename conflict") ||
		strings.Contains(lower, "already exists")
}

// ProgressReader 带限速与进度上报的 Reader。
type ProgressReader struct {
	name        string
	r           io.Reader
	total       int64
	read        int64
	last        time.Time
	start       time.Time
	taskID      string
	lastLogProg int
}

// NewProgressReader 绑定文件流与任务 ID。
func NewProgressReader(name string, r io.Reader, total int64, taskID string) *ProgressReader {
	return &ProgressReader{
		name:        name,
		r:           r,
		total:       total,
		start:       time.Now(),
		taskID:      taskID,
		lastLogProg: -1,
	}
}

// Read 实现 io.Reader，内嵌限速与进度广播。
func (p *ProgressReader) Read(b []byte) (int, error) {
	startRead := time.Now()
	n, err := p.r.Read(b)
	p.read += int64(n)

	if rateMB := CurrentRate(); rateMB > 0 {
		rate := int64(rateMB) * 1024 * 1024
		expect := time.Duration(int64(time.Second) * int64(n) / rate)
		if d := time.Since(startRead); d < expect {
			time.Sleep(expect - d)
		}
	}

	if time.Since(p.last) > 500*time.Millisecond {
		p.last = time.Now()

		var progress int
		if p.total > 0 {
			progress = int(float64(p.read) * 100 / float64(p.total))
		} else {
			progress = 100
		}

		elapsed := time.Since(p.start).Seconds()
		var speed int64
		if elapsed > 0.1 {
			speed = int64(float64(p.read) / elapsed)
		}

		step := progress / 10
		if step > p.lastLogProg {
			p.lastLogProg = step
			log.Printf("[UPLOAD][PROGRESS] 文件: %s -> 进度: %d%%", p.name, step*10)
		}

		if val, exists := LiveTasks.Load(p.taskID); exists {
			task := val.(*Task)
			task.Mu.Lock()
			task.Progress = progress
			task.Speed = speed
			wsName := task.Name
			wsPath := task.Path
			wsSize := task.Size
			wsStatus := task.Status
			wsStartTime := task.CreatedAt.UnixMilli()
			task.Mu.Unlock()

			BroadcastWS("uploadProgress", map[string]interface{}{
				"id":        p.taskID,
				"filename":  wsName,
				"path":      wsPath,
				"size":      wsSize,
				"uploaded":  p.read,
				"speed":     speed,
				"status":    wsStatus,
				"startTime": wsStartTime,
			})
		}
	}
	return n, err
}

// CleanupFailedTasksByPath 清理同路径失败任务残留。
func CleanupFailedTasksByPath(targetPath string) {
	LiveTasks.Range(func(key, value interface{}) bool {
		task := value.(*Task)
		task.Mu.RLock()
		p := task.Path
		st := task.Status
		task.Mu.RUnlock()

		if p == targetPath && st == "failed" {
			LiveTasks.Delete(key)
			if _, loaded := QueueFail.LoadAndDelete(key); loaded {
				atomic.AddInt64(&QueueFailCount, -1)
			}
		}
		return true
	})
}

// AddHistory 写入上传历史。
func AddHistory(local, remotePath string, size int64, status string, duration float64, errorMsg string) {
	HistoryStore.Add(storage.HistoryRecord{
		UploadTime: time.Now().Format("2006-01-02 15:04:05"),
		Name:       filepath.Base(remotePath),
		Size:       size,
		LocalPath:  local,
		Remote:     remotePath,
		Status:     status,
		Duration:   int(duration),
		ErrorMsg:   errorMsg,
	})
}

// Upload 建立远端 PUT 上传，并维护任务状态与熔断。
func Upload(local, remotePath string, size int64) bool {
	f, err := os.Open(local)
	if err != nil {
		log.Printf("[UPLOAD][ERR] 无法打开文件 %s: %v", local, err)
		return false
	}
	defer f.Close()

	taskID := fmt.Sprintf("task-%d", time.Now().UnixNano())
	startTime := time.Now()
	pr := NewProgressReader(filepath.Base(remotePath), f, size, taskID)

	newTask := &Task{
		ID:        taskID,
		Name:      filepath.Base(remotePath),
		Path:      local,
		Size:      size,
		Progress:  0,
		Speed:     0,
		Status:    "uploading",
		CreatedAt: startTime,
	}
	LiveTasks.Store(taskID, newTask)

	QueueUploading.Store(taskID, struct{}{})
	atomic.AddInt64(&QueueUploadingCount, 1)

	BroadcastWS("uploadProgress", map[string]interface{}{
		"id":        taskID,
		"filename":  filepath.Base(remotePath),
		"path":      local,
		"size":      size,
		"uploaded":  0,
		"speed":     0,
		"status":    "uploading",
		"startTime": startTime.UnixMilli(),
	})

	if RemoteCli == nil {
		cfg := AppCfg()
		RemoteCli = remote.NewOpenListClient(cfg.RemoteServer, cfg.RemoteUser, cfg.RemotePass, HTTPCli)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	putRes, err := RemoteCli.Put(ctx, remotePath, pr, size)

	if err != nil {
		log.Printf("[UPLOAD][HTTP][ERR] %s -> %v", filepath.Base(local), err)
		SendAlert("error", "上传连接失败", "无法连接远端服务器: "+err.Error())

		if val, exists := LiveTasks.Load(taskID); exists {
			task := val.(*Task)
			task.Mu.Lock()
			task.Status = "failed"
			task.Error = err.Error()
			task.EndTime = time.Now()
			task.Mu.Unlock()
		}

		if _, loaded := QueueUploading.LoadAndDelete(taskID); loaded {
			atomic.AddInt64(&QueueUploadingCount, -1)
		}
		QueueFail.Store(taskID, struct{}{})
		atomic.AddInt64(&QueueFailCount, 1)

		AddHistory(local, remotePath, size, "failed", time.Since(startTime).Seconds(), err.Error())
		BroadcastWS("taskDone", map[string]interface{}{"id": taskID, "status": "fail", "error": err.Error()})

		fails := IncFail()
		if fails >= 30 {
			PauseOnFailure(fmt.Sprintf("已连续 %d 次无法连接到远端服务器，网络可能断开或远端已宕机。", fails))
		}
		return false
	}

	if putRes.OK() || isRemoteNameConflict(putRes.Message) {
		if !putRes.OK() {
			// 远端已存在同名文件：本系统文件名为录制会话时间戳，同名即同内容
			// （典型场景：上传中途服务重启，远端实际已落盘而本地指纹未记录）。
			// 视为上传成功并记录指纹，避免反复重传与失败告警堆积。
			log.Printf("[UPLOAD][REMOTE][SKIP] 远端已存在同名文件，视为上传成功: %s", filepath.Base(local))
		}
		if val, exists := LiveTasks.Load(taskID); exists {
			task := val.(*Task)
			task.Mu.Lock()
			task.Status = "success"
			task.Progress = 100
			task.EndTime = time.Now()
			task.Mu.Unlock()
		}

		if _, loaded := QueueUploading.LoadAndDelete(taskID); loaded {
			atomic.AddInt64(&QueueUploadingCount, -1)
		}
		QueueSuccess.Store(taskID, struct{}{})
		atomic.AddInt64(&QueueSuccessCount, 1)

		historyErr := ""
		if !putRes.OK() {
			historyErr = "远端已存在同名文件，跳过重复上传"
		}
		AddHistory(local, remotePath, size, "success", time.Since(startTime).Seconds(), historyErr)
		BroadcastWS("taskDone", map[string]interface{}{
			"id": taskID, "status": "success", "progress": 100, "size": size,
		})
		CleanupFailedTasksByPath(local)
		ResetFail()
		return true
	}

	log.Printf("[UPLOAD][REMOTE][ERR] 远端服务器拒绝或异常，状态码: %d 详细报错: %s 文件: %s", putRes.Code, putRes.Message, filepath.Base(local))
	errMsg := fmt.Sprintf("远端拒绝 (Code: %d, 报错: %s)", putRes.Code, putRes.Message)
	SendAlert("error", "上传遭拒绝", fmt.Sprintf("文件: %s\n状态码: %d\n详细报错: %s", filepath.Base(local), putRes.Code, putRes.Message))

	if val, exists := LiveTasks.Load(taskID); exists {
		task := val.(*Task)
		task.Mu.Lock()
		task.Status = "failed"
		task.Error = errMsg
		task.EndTime = time.Now()
		task.Mu.Unlock()
	}

	if _, loaded := QueueUploading.LoadAndDelete(taskID); loaded {
		atomic.AddInt64(&QueueUploadingCount, -1)
	}
	QueueFail.Store(taskID, struct{}{})
	atomic.AddInt64(&QueueFailCount, 1)

	AddHistory(local, remotePath, size, "failed", time.Since(startTime).Seconds(), errMsg)
	BroadcastWS("taskDone", map[string]interface{}{"id": taskID, "status": "fail", "error": errMsg})

	fails := IncFail()
	if fails >= 30 {
		PauseOnFailure(fmt.Sprintf("连续 %d 个文件被远端服务器拒绝接收 (状态码: %d，报错: %s)。", fails, putRes.Code, putRes.Message))
	}
	return false
}
