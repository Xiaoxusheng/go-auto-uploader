package uploader

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueEnqueueDedup(t *testing.T) {
	q := NewQueue(10)
	if !q.Enqueue("/a") {
		t.Fatal("first enqueue should succeed")
	}
	if q.Enqueue("/a") {
		t.Fatal("dup should fail")
	}
	if !q.Contains("/a") {
		t.Fatal("should contain")
	}
	p, ok := q.Dequeue(context.Background())
	if !ok || p != "/a" {
		t.Fatalf("dequeue %v %v", p, ok)
	}
	q.Done("/a")
	if q.Contains("/a") {
		t.Fatal("after Done should not contain")
	}
}

func TestQueueFull(t *testing.T) {
	q := NewQueue(1)
	if !q.Enqueue("/x") {
		t.Fatal("first ok")
	}
	if q.Enqueue("/y") {
		t.Fatal("full should reject")
	}
	if q.Contains("/y") {
		t.Fatal("rejected path must not stay marked")
	}
}

func TestWorkerPoolProcesses(t *testing.T) {
	q := NewQueue(100)
	var n int64
	pool := NewWorkerPool(q, func(ctx context.Context, path string) {
		atomic.AddInt64(&n, 1)
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx, func() int { return 2 })

	for i := 0; i < 20; i++ {
		if !q.Enqueue(string(rune('a'+i%26)) + string(rune('0'+i/26))) {
			// unlikely collision
		}
	}
	// more unique paths
	for i := 0; i < 20; i++ {
		q.Enqueue("/f" + string(rune('0'+i)))
	}

	deadline := time.After(3 * time.Second)
	for atomic.LoadInt64(&n) < 20 {
		select {
		case <-deadline:
			t.Fatalf("processed %d want >=20", n)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
}

func TestWorkerPoolDropWhenPaused(t *testing.T) {
	q := NewQueue(10)
	var ran int64
	paused := int32(1)
	pool := NewWorkerPool(q, func(ctx context.Context, path string) {
		atomic.AddInt64(&ran, 1)
	}, func() bool { return atomic.LoadInt32(&paused) == 1 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx, func() int { return 1 })
	q.Enqueue("/p")
	time.Sleep(200 * time.Millisecond)
	if atomic.LoadInt64(&ran) != 0 {
		t.Fatal("paused pool should not handle")
	}
	if q.Contains("/p") {
		t.Fatal("dropped task should clear enqueued mark")
	}
}
