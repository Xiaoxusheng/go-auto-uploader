package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"upload/internal/app"
	"upload/internal/config"
)

// TestHandleBilibiliStatus 总览接口：未配置 Cookie 时不打真实请求、字段齐全。
func TestHandleBilibiliStatus(t *testing.T) {
	app.CfgStore.Replace(config.Config{
		ScanInterval: 30,
		Workers:      1,
		Bilibili:     config.BilibiliSettings{Enable: true},
	})
	s := newTestServer()
	// 清掉可能残留的登录态缓存，确保本用例走「未配置 Cookie」分支
	s.biliNavMu.Lock()
	s.biliNavCache = nil
	s.biliNavMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/bilibili/status", nil)
	w := httptest.NewRecorder()
	s.handleBilibiliStatus(w, req)

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
	data := resp.Data.(map[string]interface{})
	if data["enable"] != true || data["cookieReady"] != false {
		t.Errorf("enable/cookieReady = %v/%v", data["enable"], data["cookieReady"])
	}
	login := data["login"].(map[string]interface{})
	if login["checked"] != true || login["err"] == "" {
		t.Errorf("未配置 Cookie 应直接短路: %v", login)
	}
	if _, ok := data["counts"].(map[string]interface{}); !ok {
		t.Errorf("counts 缺失")
	}
}

// TestHandleBilibiliQueue 空队列 GET / 非法 action POST / 未知 id POST。
func TestHandleBilibiliQueue(t *testing.T) {
	s := newTestServer()

	// GET 空队列
	req := httptest.NewRequest(http.MethodGet, "/api/v1/bilibili/queue", nil)
	w := httptest.NewRecorder()
	s.handleBilibiliQueue(w, req)
	var resp apiResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("GET 解析失败: %v", err)
	}
	jobs := resp.Data.(map[string]interface{})["jobs"].([]interface{})
	if len(jobs) != 0 {
		t.Errorf("期望空队列, got %d", len(jobs))
	}

	// POST 非法 action
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/bilibili/queue", strings.NewReader(`{"action":"rebuild","id":"x"}`))
	s.handleBilibiliQueue(w, req)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("非法 action 期望 400, got %d", w.Result().StatusCode)
	}

	// POST 未知 id
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/bilibili/queue", strings.NewReader(`{"action":"retry","id":"no-such"}`))
	s.handleBilibiliQueue(w, req)
	if w.Result().StatusCode != http.StatusConflict {
		t.Errorf("未知 id 期望 409, got %d", w.Result().StatusCode)
	}
}
