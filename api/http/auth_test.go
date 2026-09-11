package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"upload/internal/app"
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
