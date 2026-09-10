package ws

import (
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
