package storage

import (
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"upload/internal/fsutil"
)

const defaultSuccessCap = 500000

// SuccessStore 成功上传记录 + 增量图表统计 + 原子落盘。
type SuccessStore struct {
	mu      sync.Mutex
	path    string
	cap     int
	records []UploadRecord

	trend sync.Map // date -> *TrendPoint
	rank  sync.Map // streamer -> *atomic.Int64

	dirty int32
}

// NewSuccessStore path 为 upload_success.json。
func NewSuccessStore(path string, max int) *SuccessStore {
	if max <= 0 {
		max = defaultSuccessCap
	}
	return &SuccessStore{path: path, cap: max}
}

// Load 从磁盘恢复记录并重建统计。
func (s *SuccessStore) Load() {
	data, err := readFileIfExists(s.path)
	if err != nil || len(data) == 0 {
		s.records = make([]UploadRecord, 0)
		return
	}
	var list []UploadRecord
	if err := json.Unmarshal(data, &list); err != nil {
		log.Printf("[SUCCESS_LOG][ERR] 解析 %s 失败: %v", s.path, err)
		s.records = make([]UploadRecord, 0)
		return
	}
	s.records = list
	for _, rec := range list {
		s.applyStats(rec)
	}
}

func (s *SuccessStore) applyStats(rec UploadRecord) {
	day := rec.Time.Format("01-02")
	tVal, _ := s.trend.LoadOrStore(day, &TrendPoint{Date: day})
	tp := tVal.(*TrendPoint)
	tp.Mu.Lock()
	tp.Size += float64(rec.Size) / 1024 / 1024 / 1024
	tp.Count++
	tp.Mu.Unlock()

	rVal, _ := s.rank.LoadOrStore(rec.Streamer, new(atomic.Int64))
	rVal.(*atomic.Int64).Add(rec.Size)
}

// Add 追加成功记录并打脏标记（由 PersistLoop 合并落盘）。
func (s *SuccessStore) Add(rec UploadRecord) {
	s.applyStats(rec)
	s.mu.Lock()
	s.records = append(s.records, rec)
	if len(s.records) > s.cap {
		s.records = s.records[len(s.records)-s.cap:]
	}
	s.mu.Unlock()
	atomic.StoreInt32(&s.dirty, 1)
}

// Snapshot 返回记录拷贝。
func (s *SuccessStore) Snapshot() []UploadRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]UploadRecord, len(s.records))
	copy(out, s.records)
	return out
}

// Len 记录条数。
func (s *SuccessStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// Flush 原子写回磁盘。
func (s *SuccessStore) Flush() {
	snap := s.Snapshot()
	data, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[SUCCESS_LOG][ERR] 序列化失败: %v", err)
		return
	}
	if werr := fsutil.AtomicWrite(s.path, data, 0644); werr != nil {
		log.Printf("[SUCCESS_LOG][ERR] 落盘失败: %v", werr)
	}
}

// MarkDirty 打脏标记。
func (s *SuccessStore) MarkDirty() { atomic.StoreInt32(&s.dirty, 1) }

// TakeDirty CAS 取走脏标记。
func (s *SuccessStore) TakeDirty() bool {
	return atomic.CompareAndSwapInt32(&s.dirty, 1, 0)
}

// PersistLoop 后台合并落盘，直至 stop 关闭。
func (s *SuccessStore) PersistLoop(interval time.Duration, stop <-chan struct{}) {
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

// TrendPointDTO 无锁只读聚合快照，供 API/机器人使用。
type TrendPointDTO struct {
	Date  string  `json:"date"`
	Size  float64 `json:"size"`
	Count int     `json:"count"`
}

// TrendSnapshot 导出日期聚合（供前端大屏）。
func (s *SuccessStore) TrendSnapshot() []TrendPointDTO {
	var out []TrendPointDTO
	s.trend.Range(func(_, v any) bool {
		tp := v.(*TrendPoint)
		tp.Mu.Lock()
		out = append(out, TrendPointDTO{Date: tp.Date, Size: tp.Size, Count: tp.Count})
		tp.Mu.Unlock()
		return true
	})
	return out
}

// TrendByDate 查询某日聚合。
func (s *SuccessStore) TrendByDate(date string) (size float64, count int, ok bool) {
	v, exists := s.trend.Load(date)
	if !exists {
		return 0, 0, false
	}
	tp := v.(*TrendPoint)
	tp.Mu.Lock()
	defer tp.Mu.Unlock()
	return tp.Size, tp.Count, true
}

// RankTop 返回流量前 n 的主播。
func (s *SuccessStore) RankTop(n int) [][2]any {
	type pair struct {
		name string
		size int64
	}
	var all []pair
	s.rank.Range(func(k, v any) bool {
		all = append(all, pair{name: k.(string), size: v.(*atomic.Int64).Load()})
		return true
	})
	// 插入排序足够（主播数有限）
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].size > all[j-1].size; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	if n > 0 && len(all) > n {
		all = all[:n]
	}
	out := make([][2]any, len(all))
	for i, p := range all {
		out[i] = [2]any{p.name, p.size}
	}
	return out
}
