// Package storage 持有本地持久化状态：上传历史、成功日志、目录统计。
// 业务层只依赖 Store 方法，不感知 JSON 文件布局。
package storage

import (
	"sync"
)

// HistoryRecord 长驻内存的上传历史条目（接口 JSON 字段保持兼容）。
type HistoryRecord struct {
	UploadTime string `json:"uploadTime"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	LocalPath  string `json:"localPath"`
	Remote     string `json:"remote"`
	Status     string `json:"status"`
	Duration   int    `json:"duration"`
	ErrorMsg   string `json:"errorMsg"`
}

// HistoryStore 环形内存历史，上限 max 条。
type HistoryStore struct {
	mu   sync.RWMutex
	max  int
	list []*HistoryRecord
}

// NewHistoryStore 创建历史库；max<=0 时默认 1000。
func NewHistoryStore(max int) *HistoryStore {
	if max <= 0 {
		max = 1000
	}
	return &HistoryStore{max: max, list: make([]*HistoryRecord, 0, 64)}
}

// Add 追加一条历史，超过上限时丢弃最旧。
func (s *HistoryStore) Add(rec HistoryRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, &rec)
	if len(s.list) > s.max {
		s.list = s.list[len(s.list)-s.max:]
	}
}

// Snapshot 返回拷贝，供 API 分页/过滤。
func (s *HistoryStore) Snapshot() []*HistoryRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*HistoryRecord, len(s.list))
	copy(out, s.list)
	return out
}

// Len 当前条数。
func (s *HistoryStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.list)
}
