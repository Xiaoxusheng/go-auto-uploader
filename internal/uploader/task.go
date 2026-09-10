// Package uploader 持有上传任务模型、去重队列与 Worker 池生命周期。
package uploader

import (
	"sync"
	"time"
)

// TaskStatus 任务生命周期状态（字符串值与前端/API 历史协议兼容）。
type TaskStatus string

const (
	StatusPending     TaskStatus = "pending"
	StatusUploading   TaskStatus = "uploading"
	StatusSuccess     TaskStatus = "success"
	StatusSuccessFast TaskStatus = "success(秒传)"
	StatusFailed      TaskStatus = "failed"
	StatusRetrying    TaskStatus = "retrying"
)

// Task 单次上传运行时状态。Mu 保护字段并发读写（进度协程 vs 查询 API）。
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

// TaskView 只读快照，避免调用方持有锁。
type TaskView struct {
	ID       string
	Name     string
	Path     string
	Remote   string
	Size     int64
	Progress int
	Speed    int64
	Status   string
	Error    string
	Created  time.Time
	End      time.Time
}

// Snapshot 返回当前字段的拷贝。
func (t *Task) Snapshot() TaskView {
	t.Mu.RLock()
	defer t.Mu.RUnlock()
	return TaskView{
		ID: t.ID, Name: t.Name, Path: t.Path, Remote: t.Remote,
		Size: t.Size, Progress: t.Progress, Speed: t.Speed,
		Status: t.Status, Error: t.Error, Created: t.CreatedAt, End: t.EndTime,
	}
}

// SetStatus 在写锁内更新状态。
func (t *Task) SetStatus(st TaskStatus, errMsg string) {
	t.Mu.Lock()
	t.Status = string(st)
	if errMsg != "" {
		t.Error = errMsg
	}
	if st == StatusSuccess || st == StatusSuccessFast || st == StatusFailed {
		t.EndTime = time.Now()
		if st == StatusSuccess || st == StatusSuccessFast {
			t.Progress = 100
		}
	}
	t.Mu.Unlock()
}

// Registry 保存进行中的任务，key 为 taskID。
type Registry struct {
	m sync.Map
}

// Store 写入任务。
func (r *Registry) Store(id string, t *Task) { r.m.Store(id, t) }

// Load 读取任务。
func (r *Registry) Load(id string) (*Task, bool) {
	v, ok := r.m.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*Task), true
}

// Delete 删除任务。
func (r *Registry) Delete(id string) { r.m.Delete(id) }

// Range 遍历。
func (r *Registry) Range(fn func(id string, t *Task) bool) {
	r.m.Range(func(k, v any) bool { return fn(k.(string), v.(*Task)) })
}
