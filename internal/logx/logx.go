// Package logx 提供应用日志环形缓冲与 stdout 拦截（供 Web 终端投递）。
package logx

import (
	"strings"
	"sync"
	"time"
)

// Entry 前端日志条目（JSON 字段与历史 API 兼容）。
type Entry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

// Store 环形日志缓冲 + 非阻塞入队。
type Store struct {
	mu   sync.RWMutex
	max  int
	list []*Entry
	ch   chan *Entry
}

// NewStore max 缓冲条数；chCap 入队通道容量。
func NewStore(max, chCap int) *Store {
	if max <= 0 {
		max = 5000
	}
	if chCap <= 0 {
		chCap = 1000
	}
	return &Store{max: max, list: make([]*Entry, 0, 256), ch: make(chan *Entry, chCap)}
}

// Chan 供 logCollector 消费。
func (s *Store) Chan() <-chan *Entry { return s.ch }

// Add 非阻塞入队。
func (s *Store) Add(level, message, errMsg string) {
	e := &Entry{
		Time:    time.Now().Format(time.DateTime),
		Level:   level,
		Message: message,
		Error:   errMsg,
	}
	select {
	case s.ch <- e:
	default:
	}
}

// Append 已消费条目写入环形列表。
func (s *Store) Append(e *Entry) {
	if e == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, e)
	if len(s.list) > s.max {
		s.list = s.list[len(s.list)-s.max:]
	}
}

// Snapshot 过滤查询；level/keyword 为空表示不过滤。返回倒序切片副本。
func (s *Store) Snapshot(level, keyword string) []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Entry, 0, len(s.list))
	kw := strings.ToLower(keyword)
	for i := len(s.list) - 1; i >= 0; i-- {
		e := s.list[i]
		if level != "" && e.Level != level {
			continue
		}
		if kw != "" && !strings.Contains(strings.ToLower(e.Message), kw) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// SnapshotAsc 同 Snapshot，但按时间正序（旧→新）。
func (s *Store) SnapshotAsc(level, keyword string) []*Entry {
	rev := s.Snapshot(level, keyword)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// Len 当前条数。
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.list)
}

// Interceptor 将 log 包 stdout 镜像进 Store，并剥离标准库时间前缀。
type Interceptor struct {
	Original interface{ Write(p []byte) (int, error) }
	Store    *Store
}

// Write 实现 io.Writer。
func (l *Interceptor) Write(p []byte) (n int, err error) {
	n, err = l.Original.Write(p)
	msg := strings.TrimSpace(string(p))
	parts := strings.SplitN(msg, " ", 3)
	if len(parts) >= 3 && strings.Contains(parts[0], "/") && strings.Contains(parts[1], ":") {
		msg = parts[2]
	}
	level := "info"
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "[err]") || strings.Contains(lower, "error") || strings.Contains(lower, "fail") {
		level = "error"
	} else if strings.Contains(lower, "[warn]") || strings.Contains(lower, "warning") {
		level = "warn"
	}
	if l.Store != nil {
		l.Store.Add(level, msg, "")
	}
	return n, err
}
