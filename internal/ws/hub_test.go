package ws

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublishAndClientCount(t *testing.T) {
	h := New()
	stop := make(chan struct{})
	defer close(stop)
	go h.Run(stop)

	// 无真实连接，仅验证 Publish 不阻塞、ClientCount
	h.PublishTyped("ping", map[string]int{"n": 1})
	if h.ClientCount() != 0 {
		t.Fatal("no clients expected")
	}
	time.Sleep(20 * time.Millisecond)
}

func TestSlowClientDrop(t *testing.T) {
	h := New(WithBuffer(8, 1))
	stop := make(chan struct{})
	defer close(stop)
	go h.Run(stop)

	// 极小 send 缓冲：塞满后广播应踢除
	c := &Client{send: make(chan []byte, 1)}
	h.Register(c)
	// 先占满
	c.send <- []byte(`{}`)

	var done int32
	go func() {
		// 触发多次广播
		for i := 0; i < 5; i++ {
			h.PublishTyped("x", i)
		}
		atomic.StoreInt32(&done, 1)
	}()

	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&done) != 1 {
		t.Fatal("publish blocked")
	}
	// 慢客户端应被移除
	if h.ClientCount() != 0 {
		t.Fatalf("slow client should be dropped, count=%d", h.ClientCount())
	}
}

// 广播踢除（慢客户端）与 Unregister 会从两条路径 close 同一个 send 通道。
// 必须幂等且互斥：原实现两处都裸 close，撞上就是 close of closed channel，
// 而 Hub.Run 没有 recover，属于进程级崩溃。
func TestConcurrentDropAndUnregisterNoPanic(t *testing.T) {
	h := New(WithBuffer(64, 1))
	stop := make(chan struct{})
	defer close(stop)
	go h.Run(stop)

	for i := 0; i < 200; i++ {
		c := &Client{}
		h.Register(c)
		c.send <- []byte(`{}`) // 先塞满：下一次广播必然走踢除分支

		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); h.PublishTyped("x", i) }()
		go func() { defer wg.Done(); h.Unregister(c) }()
		go func() { defer wg.Done(); h.Unregister(c) }()
		wg.Wait()
	}
	time.Sleep(20 * time.Millisecond)
}

// 关闭后的客户端再投递必须安全返回 false，且 Unregister 可重复调用。
func TestTrySendAfterClose(t *testing.T) {
	h := New()
	c := &Client{}
	h.Register(c)
	h.Unregister(c)
	if c.TrySend([]byte("x")) {
		t.Fatal("closed client should reject send")
	}
	h.Unregister(c) // 幂等，不应 panic
}
