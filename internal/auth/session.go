// Package auth 管理控制台登录令牌会话与 HTTP 鉴权中间件。
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	sessionTTL       = 24 * time.Hour
	maxLoginAttempts = 10
	loginLockWindow  = 5 * time.Minute
)

// SessionStore token -> 过期 Unix 秒。
type SessionStore struct {
	mu        sync.Map
	count     atomic.Int64
	failCnt   atomic.Int64
	lockUntil atomic.Int64
}

// NewSessionStore 创建会话库。
func NewSessionStore() *SessionStore { return &SessionStore{} }

// Issue 签发 256bit 随机令牌，TTL 后自动过期。
func (s *SessionStore) Issue() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		log.Printf("[AUTH] ⚠️ 随机数生成异常，拒绝签发令牌: %v", err)
		return ""
	}
	token := hex.EncodeToString(buf)
	s.mu.Store(token, time.Now().Add(sessionTTL).Unix())
	s.count.Add(1)
	time.AfterFunc(sessionTTL, func() {
		if _, ok := s.mu.Load(token); ok {
			s.mu.Delete(token)
			s.count.Add(-1)
		}
	})
	return token
}

// Verify 校验令牌是否有效。
func (s *SessionStore) Verify(token string) bool {
	if token == "" {
		return false
	}
	val, ok := s.mu.Load(token)
	if !ok {
		return false
	}
	if time.Now().Unix() > val.(int64) {
		s.mu.Delete(token)
		s.count.Add(-1)
		return false
	}
	return true
}

// Revoke 删除令牌。
func (s *SessionStore) Revoke(token string) {
	if _, ok := s.mu.LoadAndDelete(token); ok {
		s.count.Add(-1)
	}
}

// Count 当前有效会话数（近似）。
func (s *SessionStore) Count() int64 { return s.count.Load() }

// TokenFromRequest 从 Bearer 头或 ?token= 提取。
func TokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

// CheckLocked 判断是否处于登录锁定窗口。
func (s *SessionStore) CheckLocked() (until time.Time, locked bool) {
	u := s.lockUntil.Load()
	if u == 0 || time.Now().Unix() < u {
		if u != 0 && time.Now().Unix() < u {
			return time.Unix(u, 0), true
		}
		return time.Time{}, false
	}
	return time.Time{}, false
}

// RecordLoginFailure 记录失败；超过阈值则锁定 5 分钟。
func (s *SessionStore) RecordLoginFailure() {
	n := s.failCnt.Add(1)
	if n%maxLoginAttempts == 0 {
		s.lockUntil.Store(time.Now().Add(loginLockWindow).Unix())
	}
}

// ResetLoginFailures 登录成功后清零。
func (s *SessionStore) ResetLoginFailures() {
	s.failCnt.Store(0)
	s.lockUntil.Store(0)
}

// Middleware 强制 /api/ 与 /ws/ 必须持有合法令牌；公开登录与密钥协商接口。
func Middleware(store *SessionStore, next http.Handler) http.Handler {
	public := map[string]bool{
		"/api/v1/auth/login":   true,
		"/api/v1/sec/pubkey":   true,
		"/api/v1/sec/exchange": true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if public[p] {
			next.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/ws/") {
			next.ServeHTTP(w, r)
			return
		}
		if !store.Verify(TokenFromRequest(r)) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"未认证：请先登录"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
