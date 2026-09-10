package uploader

import (
	"context"
	"sync"
	"sync/atomic"
)

// Queue 是去重的路径队列：入队防重，出队阻塞直到有任务或 ctx 取消。
type Queue struct {
	ch       chan string
	enqueued sync.Map
	pending  int64
}

// NewQueue cap 为缓冲大小；cap<=0 时默认 100000。
func NewQueue(cap int) *Queue {
	if cap <= 0 {
		cap = 100000
	}
	return &Queue{ch: make(chan string, cap)}
}

// Enqueue 若路径未在队列中且缓冲未满则入队，返回是否成功。
func (q *Queue) Enqueue(path string) bool {
	if _, loaded := q.enqueued.LoadOrStore(path, struct{}{}); loaded {
		return false
	}
	select {
	case q.ch <- path:
		atomic.AddInt64(&q.pending, 1)
		return true
	default:
		q.enqueued.Delete(path)
		return false
	}
}

// Dequeue 阻塞取一个路径；ctx 取消时返回 ok=false。
func (q *Queue) Dequeue(ctx context.Context) (string, bool) {
	select {
	case <-ctx.Done():
		return "", false
	case p, ok := <-q.ch:
		if ok {
			atomic.AddInt64(&q.pending, -1)
		}
		return p, ok
	}
}

// Contains 判断路径是否已在队列/处理中标记。
func (q *Queue) Contains(path string) bool {
	_, ok := q.enqueued.Load(path)
	return ok
}

// Done 任务结束后清除防重标记，允许未来重新入队。
func (q *Queue) Done(path string) {
	if _, loaded := q.enqueued.LoadAndDelete(path); loaded {
		// pending 在 Dequeue 时已减，此处仅清标记
	}
}

// Drop 丢弃标记且修正 pending（Worker 在暂停时丢任务用）。
func (q *Queue) Drop(path string) {
	if _, loaded := q.enqueued.LoadAndDelete(path); loaded {
		atomic.AddInt64(&q.pending, -1)
	}
}

// Pending 当前已入队未完成计数（近似）。
func (q *Queue) Pending() int64 { return atomic.LoadInt64(&q.pending) }

// EnqueuedCount 防重表大小（含处理中）。
func (q *Queue) EnqueuedCount() int64 {
	var n int64
	q.enqueued.Range(func(_, _ any) bool { n++; return true })
	return n
}
