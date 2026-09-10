// Package notification 统一通知扇出：业务只调 Notifier，不感知微信/TG/QQ 实现。
package notification

import (
	"context"
	"log"
	"sync"
)

// Message 通知载荷。
type Message struct {
	Title string
	Body  string
}

// Notifier 单通道通知器。
type Notifier interface {
	Notify(ctx context.Context, msg Message) error
	Name() string
}

// FuncNotifier 将函数适配为 Notifier（便于注入 SendTelegram/SendQQ 等）。
type FuncNotifier struct {
	Label string
	Fn    func(ctx context.Context, msg Message) error
}

func (f FuncNotifier) Name() string {
	if f.Label == "" {
		return "func"
	}
	return f.Label
}

func (f FuncNotifier) Notify(ctx context.Context, msg Message) error {
	return f.Fn(ctx, msg)
}

// Hub 向多个 Notifier 异步扇出；单路失败不影响其它通道。
type Hub struct {
	mu        sync.RWMutex
	notifiers []Notifier
}

// New 创建空 Hub。
func New() *Hub { return &Hub{} }

// Register 追加通道。
func (h *Hub) Register(n Notifier) {
	if n == nil {
		return
	}
	h.mu.Lock()
	h.notifiers = append(h.notifiers, n)
	h.mu.Unlock()
}

// NotifyAll 同步扇出（调用方可在 goroutine 中调用）。
func (h *Hub) NotifyAll(ctx context.Context, msg Message) {
	h.mu.RLock()
	list := make([]Notifier, len(h.notifiers))
	copy(list, h.notifiers)
	h.mu.RUnlock()

	var wg sync.WaitGroup
	for _, n := range list {
		wg.Add(1)
		go func(n Notifier) {
			defer wg.Done()
			if err := n.Notify(ctx, msg); err != nil {
				log.Printf("[NOTIFY][ERR] %s: %v", n.Name(), err)
			}
		}(n)
	}
	wg.Wait()
}

// NotifyAsync 在后台 goroutine 扇出，不阻塞调用方。
func (h *Hub) NotifyAsync(msg Message) {
	go h.NotifyAll(context.Background(), msg)
}

// Len 已注册通道数。
func (h *Hub) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.notifiers)
}
