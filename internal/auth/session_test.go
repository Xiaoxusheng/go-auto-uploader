package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIssueVerifyRevoke(t *testing.T) {
	s := NewSessionStore()
	tok := s.Issue()
	if tok == "" || !s.Verify(tok) {
		t.Fatal("issue/verify")
	}
	s.Revoke(tok)
	if s.Verify(tok) {
		t.Fatal("revoked should fail")
	}
}

func TestTokenFromRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/x?token=abc", nil)
	if TokenFromRequest(r) != "abc" {
		t.Fatal("query token")
	}
	r2 := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	r2.Header.Set("Authorization", "Bearer xyz")
	if TokenFromRequest(r2) != "xyz" {
		t.Fatal("bearer")
	}
}

func TestLoginLockout(t *testing.T) {
	s := NewSessionStore()
	for i := 0; i < maxLoginAttempts; i++ {
		s.RecordLoginFailure()
	}
	if _, locked := s.CheckLocked(); !locked {
		t.Fatal("should lock after max attempts")
	}
	s.ResetLoginFailures()
	if _, locked := s.CheckLocked(); locked {
		t.Fatal("reset should unlock")
	}
}

func TestMiddleware(t *testing.T) {
	s := NewSessionStore()
	tok := s.Issue()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	h := Middleware(s, ok)

	// 公开接口
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("public login got %d", w.Code)
	}

	// 受保护无 token
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != 401 {
		t.Fatalf("no token got %d", w2.Code)
	}

	// 带 token
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req3.Header.Set("Authorization", "Bearer "+tok)
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, req3)
	if w3.Code != 200 {
		t.Fatalf("with token got %d", w3.Code)
	}

	// 静态页放行
	req4 := httptest.NewRequest(http.MethodGet, "/", nil)
	w4 := httptest.NewRecorder()
	h.ServeHTTP(w4, req4)
	if w4.Code != 200 {
		t.Fatalf("root got %d", w4.Code)
	}
	_ = time.Second
}
