package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"upload/internal/app"
	"upload/internal/auth"
	"upload/internal/recorder"
)

func newTestServer() *Server {
	return New(Options{IndexHTML: "<html>ok</html>"})
}

func TestAuthMiddlewareEnforcement(t *testing.T) {
	s := newTestServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", s.handleStatus)
	mux.HandleFunc("/api/v1/sec/pubkey", s.handleGetPubKey)
	mux.HandleFunc("/api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	srv := httptest.NewServer(s.Middleware(mux))
	defer srv.Close()

	token := s.IssueToken()
	if token == "" {
		t.Fatal("令牌签发失败")
	}
	defer s.AuthSessions().Revoke(token)

	cases := []struct {
		name string
		path string
		want int
	}{
		{"受保护接口无令牌", "/api/v1/status", http.StatusUnauthorized},
		{"受保护接口伪造令牌", "/api/v1/status?token=forge", http.StatusUnauthorized},
		{"受保护接口携带合法令牌", "/api/v1/status?token=" + token, http.StatusOK},
		{"公开密钥协商放行", "/api/v1/sec/pubkey", http.StatusOK},
		{"公开登录接口放行", "/api/v1/auth/login", 405},
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

func TestLoginBruteForceLockout(t *testing.T) {
	app.DashUser = "admin"
	app.DashPass = "admin"
	s := newTestServer()
	s.authSessions.ResetLoginFailures()
	defer s.authSessions.ResetLoginFailures()

	reqBody := `{"username":"admin","password":"wrong"}`
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(reqBody))
		w := httptest.NewRecorder()
		s.handleLogin(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("错误口令应返回 401, 获得 %d", w.Code)
		}
	}
	if _, locked := s.authSessions.CheckLocked(); !locked {
		t.Fatal("连续失败达阈值后未触发锁定")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"admin"}`))
	w := httptest.NewRecorder()
	s.handleLogin(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("锁定期间应返回 429, 获得 %d", w.Code)
	}

	s.authSessions.ResetLoginFailures()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"admin"}`))
	w = httptest.NewRecorder()
	s.handleLogin(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("正确凭据登录失败: 预期 200, 获得 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "token") || strings.Contains(body, "dash-token-") {
		t.Errorf("必须签发高熵随机令牌, 响应: %s", body)
	}
}

// TestLoginIssuesSessionCookie 登录必须同时下发会话 Cookie，
// 否则封面图反代（<img src>）、日志导出（window.open）这类
// 浏览器原生请求拿不到凭据，一律 401。
func TestLoginIssuesSessionCookie(t *testing.T) {
	app.DashUser = "admin"
	app.DashPass = "admin"
	s := newTestServer()
	s.authSessions.ResetLoginFailures()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"admin"}`))
	w := httptest.NewRecorder()
	s.handleLogin(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("正确凭据登录失败: 预期 200, 获得 %d", w.Code)
	}

	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("登录响应未下发会话 Cookie")
	}
	if cookie.Value == "" || !s.AuthSessions().Verify(cookie.Value) {
		t.Errorf("会话 Cookie 值无效: %q", cookie.Value)
	}
	if !cookie.HttpOnly {
		t.Error("会话 Cookie 必须为 HttpOnly（前端 XHR 用 Bearer 令牌，无需脚本可读）")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Error("会话 Cookie 必须为 SameSite=Lax，避免跨站子资源请求携带")
	}
}

// TestCookieAuthScope 会话 Cookie 只对白名单里的只读接口生效：
// 能修好封面图 401，又不会把 CSRF 面扩大到普通读写接口。
func TestCookieAuthScope(t *testing.T) {
	s := newTestServer()
	mux := http.NewServeMux()
	stub := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	// 封面图挂真实 handler：带 Cookie 时应「穿过鉴权」到达业务层——
	// file:// 会被业务层的协议校验以 400 拒绝，401 则说明仍被中间件拦下。
	mux.HandleFunc("/api/v1/builtin_recorder/proxy_image", recorder.ProxyImage)
	mux.HandleFunc("/api/v1/logs/download", stub)
	mux.HandleFunc("/api/v1/status", s.handleStatus)

	srv := httptest.NewServer(s.Middleware(mux))
	defer srv.Close()

	token := s.IssueToken()
	if token == "" {
		t.Fatal("令牌签发失败")
	}
	defer s.AuthSessions().Revoke(token)
	sessionCookie := &http.Cookie{Name: auth.SessionCookie, Value: token}

	cases := []struct {
		name   string
		method string
		path   string
		cookie *http.Cookie
		want   int
	}{
		{"封面图反代带 Cookie 触达业务层", http.MethodGet, "/api/v1/builtin_recorder/proxy_image?url=file:///etc/passwd", sessionCookie, http.StatusBadRequest},
		{"日志导出带 Cookie 放行", http.MethodGet, "/api/v1/logs/download", sessionCookie, http.StatusOK},
		{"封面图反代无凭据拒绝", http.MethodGet, "/api/v1/builtin_recorder/proxy_image?url=file:///etc/passwd", nil, http.StatusUnauthorized},
		{"伪造 Cookie 拒绝", http.MethodGet, "/api/v1/builtin_recorder/proxy_image?url=file:///etc/passwd", &http.Cookie{Name: auth.SessionCookie, Value: "forge"}, http.StatusUnauthorized},
		{"普通接口不认 Cookie", http.MethodGet, "/api/v1/status", sessionCookie, http.StatusUnauthorized},
		{"白名单路径的写方法不认 Cookie", http.MethodPost, "/api/v1/logs/download", sessionCookie, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		if tc.cookie != nil {
			req.AddCookie(tc.cookie)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("[%s] 请求异常: %v", tc.name, err)
		}
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Errorf("[%s] 状态码错误: 预期 %d, 获得 %d", tc.name, tc.want, res.StatusCode)
		}
	}
}

// TestMiddlewareBackfillsSessionCookie 老会话（只有 Bearer 令牌、没有 Cookie）
// 在首个请求后应被补发 Cookie，否则用户升级后仍会看到封面图 401。
func TestMiddlewareBackfillsSessionCookie(t *testing.T) {
	s := newTestServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", s.handleStatus)
	srv := httptest.NewServer(s.Middleware(mux))
	defer srv.Close()

	token := s.IssueToken()
	if token == "" {
		t.Fatal("令牌签发失败")
	}
	defer s.AuthSessions().Revoke(token)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求异常: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("Bearer 鉴权失败: 预期 200, 获得 %d", res.StatusCode)
	}
	var found *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookie {
			found = c
		}
	}
	if found == nil {
		t.Fatal("携带 Bearer 令牌的请求未补发会话 Cookie")
	}
	if found.Value != token {
		t.Errorf("补发的 Cookie 值与令牌不一致: %q", found.Value)
	}
	if found.MaxAge <= 0 || found.MaxAge > int(auth.SessionTTL.Seconds()) {
		t.Errorf("补发 Cookie 的有效期应受令牌剩余 TTL 约束, 获得 MaxAge=%d", found.MaxAge)
	}
}

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
		recorder.ProxyImage(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("[%s] 预期 400 拒绝, 获得 %d", tc.name, w.Code)
		}
	}
}
