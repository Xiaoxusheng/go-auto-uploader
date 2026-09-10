package storage

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"upload/internal/fsutil"
)

// DirStatus 单个监控目录的累计统计。
type DirStatus struct {
	Mu            sync.RWMutex
	Path          string `json:"path"`
	TotalFiles    int    `json:"totalFiles"`
	UploadedFiles int    `json:"uploadedFiles"`
	PendingFiles  int    `json:"pendingFiles"`
	TotalSize     int64  `json:"totalSize"`
	UploadedSize  int64  `json:"uploadedSize"`
	LastScanTime  int64  `json:"lastScanTime"`
}

// DirStatusStore 目录状态字典 + 脏标记合并落盘。
type DirStatusStore struct {
	m     sync.Map
	path  string
	dirty int32
}

// NewDirStatusStore path 为 dir_status.json。
func NewDirStatusStore(path string) *DirStatusStore {
	return &DirStatusStore{path: path}
}

// Load 从磁盘恢复。
func (s *DirStatusStore) Load() {
	data, err := readFileIfExists(s.path)
	if err != nil || len(data) == 0 {
		return
	}
	var temp map[string]*DirStatus
	if err := json.Unmarshal(data, &temp); err != nil {
		log.Printf("[DIR_STATUS][ERR] 解析失败: %v", err)
		return
	}
	for k, v := range temp {
		s.m.Store(k, v)
	}
}

// Load 获取目录状态。
func (s *DirStatusStore) Get(root string) (*DirStatus, bool) {
	v, ok := s.m.Load(root)
	if !ok {
		return nil, false
	}
	return v.(*DirStatus), true
}

// GetOrCreate 取或初始化目录状态。
func (s *DirStatusStore) GetOrCreate(root string) *DirStatus {
	if ds, ok := s.Get(root); ok {
		return ds
	}
	ds := &DirStatus{Path: root, LastScanTime: time.Now().UnixMilli()}
	actual, _ := s.m.LoadOrStore(root, ds)
	return actual.(*DirStatus)
}

// Put 写入指定目录状态（覆盖）。
func (s *DirStatusStore) Put(root string, ds *DirStatus) {
	s.m.Store(root, ds)
}

// Range 遍历。
func (s *DirStatusStore) Range(fn func(root string, ds *DirStatus) bool) {
	s.m.Range(func(k, v any) bool { return fn(k.(string), v.(*DirStatus)) })
}

// MarkDirty 打脏。
func (s *DirStatusStore) MarkDirty() { atomic.StoreInt32(&s.dirty, 1) }

// TakeDirty CAS 取脏。
func (s *DirStatusStore) TakeDirty() bool {
	return atomic.CompareAndSwapInt32(&s.dirty, 1, 0)
}

// Flush 原子写回。
func (s *DirStatusStore) Flush() {
	temp := make(map[string]*DirStatus)
	s.m.Range(func(key, value any) bool {
		ds := value.(*DirStatus)
		ds.Mu.RLock()
		temp[key.(string)] = &DirStatus{
			Path:          ds.Path,
			TotalFiles:    ds.TotalFiles,
			UploadedFiles: ds.UploadedFiles,
			PendingFiles:  ds.PendingFiles,
			TotalSize:     ds.TotalSize,
			UploadedSize:  ds.UploadedSize,
			LastScanTime:  ds.LastScanTime,
		}
		ds.Mu.RUnlock()
		return true
	})
	data, err := json.MarshalIndent(temp, "", "  ")
	if err != nil {
		return
	}
	if werr := fsutil.AtomicWrite(s.path, data, 0644); werr != nil {
		log.Printf("[DIR_STATUS][ERR] 落盘失败: %v", werr)
	}
}

// PersistLoop 后台合并落盘。
func (s *DirStatusStore) PersistLoop(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if s.TakeDirty() {
				s.Flush()
			}
		}
	}
}

func readFileIfExists(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return os.ReadFile(path)
}
