// Package ws 提供 WebSocket 广播 Hub：注册客户端、非阻塞 fan-out、慢客户端踢除。
package ws

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Message 广播载荷，与前端协议一致。
type Message struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

// Client 单连接：独立发送缓冲 + 可选 AES 密钥。
type Client struct {
	Conn   *websocket.Conn
	AESKey []byte
	send   chan []byte

	// sendMu/closed/closeOnce 保护发送通道的关闭与投递：
	// Hub 广播协程、慢客户端踢除、Unregister 三处都可能碰同一个 send，
	// 裸 close + 裸 send 会撞出「close of closed channel」或
	// 「send on closed channel」，两者都是进程级 panic（Hub.Run 无 recover）。
	sendMu    sync.RWMutex
	closed    bool
	closeOnce sync.Once
}

// Send 缓冲通道（只读暴露给 Hub）。
func (c *Client) Send() <-chan []byte { return c.send }

// SendBuf 可写缓冲（Hub 内部使用）。
//
// 注意：绕过 sendMu 的裸发送与 closeSend 竞争会 panic，Hub 内部一律走 trySend。
func (c *Client) SendBuf() chan []byte { return c.send }

// trySend 非阻塞投递；客户端已关闭时返回 false。
// 判定与发送在同一把读锁内完成，保证不会向正在关闭的通道写入。
func (c *Client) trySend(b []byte) bool {
	c.sendMu.RLock()
	defer c.sendMu.RUnlock()
	if c.closed || c.send == nil {
		return false
	}
	select {
	case c.send <- b:
		return true
	default:
		return false
	}
}

// TrySend 非阻塞向单客户端投递；失败返回 false。
func (c *Client) TrySend(b []byte) bool { return c.trySend(b) }

// closeSend 幂等关闭发送通道；关闭后 trySend 一律返回 false。
func (c *Client) closeSend() {
	c.closeOnce.Do(func() {
		c.sendMu.Lock()
		c.closed = true
		send := c.send
		c.sendMu.Unlock()
		if send != nil {
			close(send)
		}
	})
}

// closeConn 安全关闭连接（Conn 可能为 nil，用于测试）。
func (c *Client) closeConn() {
	if c.Conn != nil {
		_ = c.Conn.Close()
	}
}

// EncryptFunc 将明文字节加密为可放进 JSON 的密文字符串；失败返回 error。
type EncryptFunc func(plain []byte, key []byte) (string, error)

// EncryptEnabledFunc 每次广播前判断是否启用加密。
type EncryptEnabledFunc func() bool

// Hub 管理客户端集合与广播队列。
type Hub struct {
	clients   sync.Map // *Client -> struct{}
	broadcast chan Message

	encrypt         EncryptFunc
	encryptEnabled  EncryptEnabledFunc
	clientSendCap   int
	broadcastBuf    int
	writeDeadline   time.Duration
	slowDropEnabled bool
}

// Option 配置 Hub。
type Option func(*Hub)

// WithEncrypt 注入加密回调。
func WithEncrypt(fn EncryptFunc, enabled EncryptEnabledFunc) Option {
	return func(h *Hub) {
		h.encrypt = fn
		h.encryptEnabled = enabled
	}
}

// WithBuffer 自定义缓冲大小。
func WithBuffer(broadcastBuf, clientSendCap int) Option {
	return func(h *Hub) {
		if broadcastBuf > 0 {
			h.broadcastBuf = broadcastBuf
		}
		if clientSendCap > 0 {
			h.clientSendCap = clientSendCap
		}
	}
}

// New 创建 Hub。
func New(opts ...Option) *Hub {
	h := &Hub{
		broadcast:     make(chan Message, 1024),
		clientSendCap: 1024,
		broadcastBuf:  1024,
		writeDeadline: 2 * time.Second,
	}
	for _, o := range opts {
		o(h)
	}
	h.broadcast = make(chan Message, h.broadcastBuf)
	return h
}

// Register 注册客户端。
func (h *Hub) Register(c *Client) {
	if c.send == nil {
		c.send = make(chan []byte, h.clientSendCap)
	}
	h.clients.Store(c, struct{}{})
}

// Unregister 移除并关闭发送通道（幂等：重复调用不会重复 close）。
func (h *Hub) Unregister(c *Client) {
	if _, loaded := h.clients.LoadAndDelete(c); loaded {
		c.closeSend()
	}
}

// ClientCount 在线连接数。
func (h *Hub) ClientCount() int {
	n := 0
	h.clients.Range(func(_, _ any) bool { n++; return true })
	return n
}

// Publish 非阻塞投递广播消息。
func (h *Hub) Publish(msg Message) {
	select {
	case h.broadcast <- msg:
	default:
	}
}

// PublishTyped 便捷方法。
func (h *Hub) PublishTyped(msgType string, payload any) {
	h.Publish(Message{Type: msgType, Payload: payload})
}

// Run 消费广播队列直至 stop 关闭。
func (h *Hub) Run(stop <-chan struct{}) {
	encRefresh := time.NewTicker(5 * time.Second)
	defer encRefresh.Stop()
	encOn := h.encryptEnabled != nil && h.encryptEnabled()

	for {
		select {
		case <-stop:
			return
		case msg := <-h.broadcast:
			raw, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			h.clients.Range(func(key, _ any) bool {
				client := key.(*Client)
				final := raw
				if encOn && h.encrypt != nil && client.AESKey != nil {
					enc, eerr := h.encrypt(raw, client.AESKey)
					if eerr == nil {
						final = []byte(`{"encrypted":"` + enc + `"}`)
					}
				}
				if client.trySend(final) {
					return true
				}
				// 投递失败：慢客户端踢除。用 LoadAndDelete 抢占，只有抢到的一方
				// 才 close —— 否则会与 Unregister 的 close 撞成 double close panic。
				if _, loaded := h.clients.LoadAndDelete(client); loaded {
					client.closeSend()
					client.closeConn()
				}
				return true
			})
		case <-encRefresh.C:
			if h.encryptEnabled != nil {
				encOn = h.encryptEnabled()
			}
		}
	}
}

// WritePump 独立写协程，写超时由 Hub 配置。
func WritePump(c *Client, deadline time.Duration) {
	if c.Conn != nil {
		defer c.Conn.Close()
	}
	if deadline <= 0 {
		deadline = 2 * time.Second
	}
	for message := range c.send {
		c.Conn.SetWriteDeadline(time.Now().Add(deadline))
		if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
			return
		}
	}
	_ = c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
}
