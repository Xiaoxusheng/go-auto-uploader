package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAuthMiddlewareEnforcement 安全审计回归测试：验证全局认证中间件的放行与拦截边界。
// 公开面（登录/密钥协商/静态资源）必须放行，其余 /api 与 /ws 通道必须强制令牌校验。
func TestAuthMiddlewareEnforcement(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", handleStatus)
	mux.HandleFunc("/api/v1/sec/pubkey", handleGetPubKey)
	mux.HandleFunc("/api/v1/auth/login", handleLogin)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	srv := httptest.NewServer(authMiddleware(mux))
	defer srv.Close()

	token := issueAuthToken()
	if token == "" {
		t.Fatal("令牌签发失败")
	}
	defer authSessions.Delete(token)

	cases := []struct {
		name string
		path string
		want int
	}{
		{"受保护接口无令牌", "/api/v1/status", http.StatusUnauthorized},
		{"受保护接口伪造令牌", "/api/v1/status?token=forge", http.StatusUnauthorized},
		{"受保护接口携带合法令牌", "/api/v1/status?token=" + token, http.StatusOK},
		{"公开密钥协商放行", "/api/v1/sec/pubkey", http.StatusOK},
		{"公开登录接口放行", "/api/v1/auth/login", 405}, // GET 被 handler 拒绝而非中间件 401，证明已穿透
		{"静态资源放行", "/index.html", http.StatusOK},
	}

	for _, tc := range cases {
		res, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatalf("[%s] 请求异常: %v", tc.name, err)
		}
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Errorf("[%s] 状态码错误: 预期 %d, 获得 %d", tc.name, tc.want, res.StatusCode)
		}
	}

	// Bearer 头方式同样必须有效
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Bearer 头请求异常: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("Bearer 头鉴权失败: 预期 200, 获得 %d", res.StatusCode)
	}
}

// TestLoginBruteForceLockout 安全审计回归测试：验证连续口令失败会触发防爆破锁定。
func TestLoginBruteForceLockout(t *testing.T) {
	// 保存并恢复全局状态，避免污染其他测试
	oldLock := loginLockUntil.Load()
	defer loginLockUntil.Store(oldLock)
	loginLockUntil.Store(0)
	loginFailCnt.Store(0)
	defer loginFailCnt.Store(0)

	reqBody := `{"username":"admin","password":"wrong"}`
	for i := 0; i < maxLoginAttempts; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(reqBody))
		w := httptest.NewRecorder()
		handleLogin(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("错误口令应返回 401, 获得 %d", w.Code)
		}
	}

	// 阈值达成后应被锁定
	if loginLockUntil.Load() <= time.Now().Unix() {
		t.Fatal("连续失败达阈值后未触发锁定")
	}

	// 锁定期间即使口令正确也应拒绝
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"admin"}`))
	w := httptest.NewRecorder()
	handleLogin(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("锁定期间应返回 429, 获得 %d", w.Code)
	}

	// 重置锁定后正确凭据应签发令牌
	loginLockUntil.Store(0)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"admin"}`))
	w = httptest.NewRecorder()
	handleLogin(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("正确凭据登录失败: 预期 200, 获得 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "token") || strings.Contains(body, "dash-token-") {
		t.Errorf("必须签发高熵随机令牌, 响应: %s", body)
	}
}

// TestProxyImageSSRFGuard 安全审计回归测试：图片代理必须拒绝非 http/https 协议。
func TestProxyImageSSRFGuard(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"file协议", "file:///etc/passwd"},
		{"无协议裸地址", "192.168.5.10"},
		{"gopher协议", "gopher://127.0.0.1:6379/_INFO"},
	}

	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/builtin_recorder/proxy_image?url="+tc.url, nil)
		w := httptest.NewRecorder()
		apiProxyImage(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("[%s] 预期 400 拒绝, 获得 %d", tc.name, w.Code)
		}
	}
}
