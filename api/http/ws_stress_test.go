package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"upload/internal/app"
	"upload/internal/config"
)

func setupMockWSServer() (*httptest.Server, string) {
	app.CfgStore.Update(func(c *config.Config) { c.EnableEncryption = false })
	if app.WSHub == nil {
		app.InitHubs()
	}

	s := newTestServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/live", s.handleWebSocket)
	token := s.IssueToken()
	return httptest.NewServer(s.Middleware(mux)), token
}

func TestWebSocketHighConcurrencyBroadcast(t *testing.T) {
	server, wsToken := setupMockWSServer()
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/live?token=" + wsToken
	go wsBroadcastLoop()

	clientCount := 500
	var wg sync.WaitGroup
	var successfulReceives atomic.Int32

	conns := make([]*websocket.Conn, 0, clientCount)
	var connsMu sync.Mutex

	dialConcurrency := 64
	sem := make(chan struct{}, dialConcurrency)

	for i := 0; i < clientCount; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

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

			go func(c *websocket.Conn) {
				for {
					_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
					_, msg, err := c.ReadMessage()
					if err != nil {
						return
					}
					if strings.Contains(string(msg), "stress_test_payload") {
						successfulReceives.Add(1)
					}
				}
			}(conn)
		}(i)
	}

	wg.Wait()
	time.Sleep(500 * time.Millisecond)

	broadcastMsg := map[string]interface{}{
		"event": "stress_test_payload",
		"ts":    time.Now().UnixNano(),
	}
	broadcastCount := 10
	for i := 0; i < broadcastCount; i++ {
		app.BroadcastWS("systemStatus", broadcastMsg)
	}
	time.Sleep(2 * time.Second)

	expectedReceives := int32(clientCount * broadcastCount)
	actualReceives := successfulReceives.Load()
	if actualReceives < expectedReceives-50 {
		t.Errorf("广播分发严重丢失! 预期总接收量 %d, 实际成功接收 %d", expectedReceives, actualReceives)
	} else {
		t.Logf("高并发分发成功: 500客户端 x 10次广播 = 接收到 %d 条数据 (预期 %d)", actualReceives, expectedReceives)
	}

	connsMu.Lock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	connsMu.Unlock()
}
