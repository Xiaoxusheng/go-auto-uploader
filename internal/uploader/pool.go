package uploader

import (
	"context"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Handler 处理单个路径（上传主流程）。
type Handler func(ctx context.Context, path string)

// WorkerPool 动态扩容的 Worker 池，生命周期挂在 ctx 上。
type WorkerPool struct {
	q       *Queue
	handle  Handler
	paused  func() bool
	workers int32
	wg      sync.WaitGroup

	// active 当前正在执行任务的 Worker 数
	active int64
}

// NewWorkerPool 创建池。paused 返回 true 时 Worker 丢弃任务不执行。
func NewWorkerPool(q *Queue, handle Handler, paused func() bool) *WorkerPool {
	if paused == nil {
		paused = func() bool { return false }
	}
	return &WorkerPool{q: q, handle: handle, paused: paused}
}

// Start 启动管理循环：按 desired 动态补 Worker，直至 ctx 取消。
// desiredFn 每次巡检读取期望 Worker 数。
func (p *WorkerPool) Start(ctx context.Context, desiredFn func() int) {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		p.ensure(ctx, desiredFn())
		for {
			select {
			case <-ctx.Done():
				p.wg.Wait()
				return
			case <-ticker.C:
				p.ensure(ctx, desiredFn())
			}
		}
	}()
}

func (p *WorkerPool) ensure(ctx context.Context, desired int) {
	if desired < 1 {
		desired = 1
	}
	cur := atomic.LoadInt32(&p.workers)
	for int(cur) < desired {
		newID := int(atomic.AddInt32(&p.workers, 1))
		p.wg.Add(1)
		go p.loop(ctx, newID)
		cur = atomic.LoadInt32(&p.workers)
	}
	// 收缩：多余 Worker 在完成当前任务后自然退出（Dequeue 见 ctx 或通过退出检查）
	// 简单策略：不强杀，仅不再扩容；ctx 取消时全部退出。
}

func (p *WorkerPool) loop(ctx context.Context, id int) {
	defer p.wg.Done()
	defer atomic.AddInt32(&p.workers, -1)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		path, ok := p.q.Dequeue(ctx)
		if !ok {
			return
		}
		if p.paused() {
			p.q.Drop(path)
			continue
		}
		atomic.AddInt64(&p.active, 1)
		start := time.Now()
		p.handle(ctx, path)
		log.Printf("[UPLOAD][DONE][W%d] 文件:%s 耗时:%s", id, filepath.Base(path), time.Since(start).Truncate(time.Millisecond))
		p.q.Done(path)
		atomic.AddInt64(&p.active, -1)
	}
}

// Active 当前执行中的 Worker 数。
func (p *WorkerPool) Active() int64 { return atomic.LoadInt64(&p.active) }

// Workers 当前存活 Worker 数。
func (p *WorkerPool) Workers() int32 { return atomic.LoadInt32(&p.workers) }
