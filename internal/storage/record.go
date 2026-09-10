package storage

import (
	"sync"
	"time"
)

// UploadRecord 成功上传统计条目。
type UploadRecord struct {
	Time     time.Time
	Streamer string
	Name     string
	Remote   string
	Size     int64
}

// TrendPoint 单日流量聚合。
type TrendPoint struct {
	Mu    sync.Mutex
	Date  string  `json:"date"`
	Size  float64 `json:"size"`
	Count int     `json:"count"`
}
