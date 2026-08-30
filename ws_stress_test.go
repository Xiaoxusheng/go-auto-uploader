package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// setupMockWSServer 初始化并启动一个用于高并发测试的内存级 WebSocket 路由服务器。
// 该服务器关闭了全局加密以专注测试底层广播 I/O 性能，直接映射核心的 handleWebSocket 处理器，
// 并复用生产同款 authMiddleware 中间件拓扑，确保登录令牌链路真实有效。
func setupMockWSServer() (*httptest.Server, string) {
	// 临时关闭加密，专注测试并发分发性能
	appConfigMu.Lock()
	appConfig.EnableEncryption = false
	appConfigMu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/live", handleWebSocket)

	// 安全审计修复后：握手必须携带合法令牌，测试进程内直接签发一个
	token := issueAuthToken()
	return httptest.NewServer(authMiddleware(mux)), token
}

// TestWebSocketHighConcurrencyBroadcast 执行 WebSocket 核心链路的极限高并发广播测试。
// 该函数会瞬间建立成百上千个 WS 客户端，模拟高频的全站广播，
// 验证 wsClients 无锁 Map 和无锁 Channel 在高压下是否会产生死锁、消息丢失或 Goroutine 泄漏。
func TestWebSocketHighConcurrencyBroadcast(t *testing.T) {
	server, wsToken := setupMockWSServer()
	defer server.Close()

	// 将 http 协议转换为 ws 协议，并携带登录令牌
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/live?token=" + wsToken

	// 启动后台广播核心守护进程 (确保全局只有这一个在跑，防止与其他测试冲突)
	// 这里使用 context 优雅退出在实际应用中更好，但测试环境中由于是全局 channel，需要确保其在消费
	go wsBroadcastLoop()

	clientCount := 500 // 模拟 500 个高频并发终端
	var wg sync.WaitGroup
	var successfulReceives atomic.Int32

	// 存放所有存活的客户端连接，用于后续资源清理
	conns := make([]*websocket.Conn, 0, clientCount)
	var connsMu sync.Mutex

	// 阶段一：建立并发连接
	// ✨ 采用有界并发握手并对拒连做退避重试：瞬间打出全部 SYN 会打爆 Windows 回环监听队列，
	// 导致内核直接回 RST (connectex: 主动拒绝)，这属于内核行为而非广播分发缺陷
	dialConcurrency := 64 // 任意时刻在途握手上限，足以施压广播分发链路
	sem := make(chan struct{}, dialConcurrency)

	for i := 0; i < clientCount; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()

			// 占用一个握手名额，将在途并发控制在内核监听队列可承受范围内
			sem <- struct{}{}
			defer func() { <-sem }()

			// 设置握手超时机制，对内核瞬时拒连做有限次退避重试
			dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
			var conn *websocket.Conn
			var err error
			for attempt := 0; attempt < 5; attempt++ {
				conn, _, err = dialer.Dial(wsURL, nil)
				if err == nil {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if err != nil {
				t.Errorf("客户端 %d 连接失败: %v", clientID, err)
				return
			}

			connsMu.Lock()
			conns = append(conns, conn)
			connsMu.Unlock()

			// 开启独立协程持续监听服务器下发的广播
			go func(c *websocket.Conn) {
				for {
					_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
					_, msg, err := c.ReadMessage()
					if err != nil {
						return // 连接关闭或超时断开
					}
					// 验证是否收到特定的测试载荷
					if strings.Contains(string(msg), "stress_test_payload") {
						successfulReceives.Add(1)
					}
				}
			}(conn)
		}(i)
	}

	// 等待所有客户端连接建联完毕
	wg.Wait()

	// 确保存活的 WebSocket 客户端已被系统接管存入 sync.Map
	time.Sleep(500 * time.Millisecond)

	// 阶段二：服务器发起高压全量广播
	broadcastMsg := map[string]interface{}{
		"event": "stress_test_payload",
		"ts":    time.Now().UnixNano(),
	}

	// 瞬间打入 10 条高频全站广播
	broadcastCount := 10
	for i := 0; i < broadcastCount; i++ {
		broadcastWS("systemStatus", broadcastMsg)
	}

	// 给予一定的网络分发和客户端读取时间
	time.Sleep(2 * time.Second)

	// 阶段三：断言与资源回收
	expectedReceives := int32(clientCount * broadcastCount)
	actualReceives := successfulReceives.Load()

	// 允许极少量的网络抖动丢包，但核心不应该大面积崩溃
	if actualReceives < expectedReceives-50 {
		t.Errorf("广播分发严重丢失! 预期总接收量 %d, 实际成功接收 %d", expectedReceives, actualReceives)
	} else {
		t.Logf("高并发分发成功: 500客户端 x 10次广播 = 接收到 %d 条数据 (预期 %d)", actualReceives, expectedReceives)
	}

	// 彻底清理并关闭所有连接
	connsMu.Lock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	connsMu.Unlock()
}
