package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"upload/internal/app"
	"upload/internal/config"
)

// TestHandleHighlightLive 队列快照接口：200 + 关键字段齐全（字段名是前端消费契约）。
func TestHandleHighlightLive(t *testing.T) {
	app.CfgStore.Replace(config.Config{ScanInterval: 30, Workers: 1})
	s := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/highlight/live", nil)
	w := httptest.NewRecorder()
	s.handleHighlightLive(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", w.Result().StatusCode)
	}
	var resp apiResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if resp.Code != 200 {
		t.Fatalf("业务码 = %d", resp.Code)
	}
	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("data 应为对象: %T", resp.Data)
	}
	for _, k := range []string{"enabled", "running", "gate_enabled", "pending",
		"queue_head", "current", "today", "recent_done", "last_pass", "scan_interval_min"} {
		if _, ok := data[k]; !ok {
			t.Errorf("缺少字段 %q", k)
		}
	}
}
