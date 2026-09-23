// Package auth 管理控制台登录令牌会话与 HTTP 鉴权中间件。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"upload/internal/fsutil"
)

const (
	// SessionTTL 控制台登录令牌有效期。
	// 取 30 天：控制台是日常要看的页面，24 小时太短——隔天打开就得重新输一遍密码。
	// 令牌另见 SessionStore 的落盘持久化，进程重启（含每次部署）后依然有效。
	SessionTTL       = 30 * 24 * time.Hour
	maxLoginAttempts = 10
	loginLockWindow  = 5 * time.Minute

	// credentialSalt 凭据指纹的固定盐，避免弱口令的指纹被查表直接还原。
	credentialSalt = "upload-auth-v1\x00"

	// SessionCookie 会话 Cookie 名。
	// 浏览器原生发起的资源请求（<img src>、window.open 下载）无法附加
	// Authorization 头，只能依赖同源 Cookie 自动携带，故登录时同步下发。
	SessionCookie = "upload_session"
)

// cookieAuthPaths 允许仅凭会话 Cookie 通过鉴权的路径白名单。
//
// 这些接口都是「只读、幂等、返回公开数据」的：封面图反代拿的是直播间公开封面，
// 日志下载只是把本机日志文件推给已登录的浏览器。它们必须放行 Cookie，
// 否则 <img>/window.open 这类原生请求一律 401（浏览器不给它们加 Bearer 头）。
//
// 其余 /api/ 接口一律仍要 Bearer 令牌：Cookie 是浏览器自动携带的，
// 一旦全局接受就等于引入 CSRF 面，不如把这个例外收窄到明确的只读接口上。
var cookieAuthPaths = map[string]bool{
	"/api/v1/builtin_recorder/proxy_image": true,
	"/api/v1/logs/download":                true,
}

// sessionFile 会话落盘结构。
// Fingerprint 是账号密码的摘要：一旦改过账号或密码，历史会话全部作废
// （令牌有效期 30 天且跨重启存活，改密码必须能踢掉旧会话）。
type sessionFile struct {
	Fingerprint string           `json:"fingerprint"`
	Sessions    map[string]int64 `json:"sessions"` // token -> 过期 Unix 秒
}

// SessionStore token -> 过期 Unix 秒。
//
// 令牌默认落盘到 <dataDir>/sessions.json：进程重启（含每次部署）后仍然有效，
// 已登录的浏览器不必重新输密码。未调用 Init 时退化为纯内存，行为与旧版一致。
type SessionStore struct {
	mu        sync.Map // token -> 过期 Unix 秒（int64）
	count     atomic.Int64
	failCnt   atomic.Int64
	lockUntil atomic.Int64

	initMu sync.RWMutex
	path   string // 落盘路径；空 = 纯内存
	fp     string // 当前控制台账号密码的指纹
	wmu    sync.Mutex
}

// NewSessionStore 创建会话库。
func NewSessionStore() *SessionStore { return &SessionStore{} }

// Init 指定会话落盘文件、绑定当前控制台账号密码，并载入历史会话。
// 落盘文件里的指纹与当前凭据不一致时，历史会话一律作废。
// 应在服务开始处理请求前调用一次；path 为空则退化为纯内存。
func (s *SessionStore) Init(path, user, pass string) {
	s.initMu.Lock()
	s.path = path
	s.fp = credentialFingerprint(user, pass)
	s.initMu.Unlock()
	s.load()
}

func (s *SessionStore) config() (path, fp string) {
	s.initMu.RLock()
	defer s.initMu.RUnlock()
	return s.path, s.fp
}

// credentialFingerprint 账号密码的指纹（只存摘要，不落明文）。
func credentialFingerprint(user, pass string) string {
	sum := sha256.Sum256([]byte(credentialSalt + user + "\x00" + pass))
	return hex.EncodeToString(sum[:16])
}

// load 全量替换式载入磁盘会话（幂等，可重复调用）。
func (s *SessionStore) load() {
	s.mu.Range(func(k, _ interface{}) bool {
		s.mu.Delete(k)
		return true
	})
	s.count.Store(0)

	path, fp := s.config()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return
	}
	var f sessionFile
	legacyUpgrade := false
	if uerr := json.Unmarshal(data, &f); uerr != nil || f.Sessions == nil {
		// 旧版落盘格式是裸 map（{"<token>": 过期秒}），没有 fingerprint 字段。
		// 线上可能残留这种文件（历史版本写过），不能因为读不懂就把所有浏览器
		// 踢回登录页——那正是用户要避免的事。按当前凭据接管，并立刻以新格式重写，
		// 让「改密码踢会话」从这一刻起生效。
		var legacy map[string]int64
		if lerr := json.Unmarshal(data, &legacy); lerr != nil {
			log.Printf("[AUTH][ERR] 解析会话文件 %s 失败: %v（按未登录处理）", path, lerr)
			return
		}
		if len(legacy) == 0 {
			return
		}
		log.Printf("[AUTH] 🔄 会话文件 %s 为旧格式（无凭据指纹），按当前凭据接管", path)
		f = sessionFile{Fingerprint: fp, Sessions: legacy}
		legacyUpgrade = true
	}
	if f.Fingerprint != fp {
		if len(f.Sessions) > 0 {
			log.Printf("[AUTH] 🔒 控制台账号或密码已变更，%d 个历史登录会话已作废", len(f.Sessions))
		}
		return
	}
	now := time.Now().Unix()
	restored := 0
	for token, exp := range f.Sessions {
		if exp <= now {
			continue
		}
		s.mu.Store(token, exp)
		s.scheduleExpiry(token, exp)
		restored++
	}
	s.count.Store(int64(restored))
	if restored > 0 {
		log.Printf("[AUTH] 🔑 已恢复 %d 个控制台登录会话（重启/部署后无需重新登录）", restored)
	}
	if legacyUpgrade {
		s.save() // 升级为新格式，补上凭据指纹
	}
}

// scheduleExpiry 到点自动清除令牌。
func (s *SessionStore) scheduleExpiry(token string, exp int64) {
	d := time.Until(time.Unix(exp, 0))
	if d <= 0 {
		d = time.Second
	}
	time.AfterFunc(d, func() {
		if _, ok := s.mu.LoadAndDelete(token); ok {
			s.count.Add(-1)
			s.save()
		}
	})
}

// Issue 签发 256bit 随机令牌，TTL 后自动过期。
func (s *SessionStore) Issue() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		log.Printf("[AUTH] ⚠️ 随机数生成异常，拒绝签发令牌: %v", err)
		return ""
	}
	token := hex.EncodeToString(buf)
	exp := time.Now().Add(SessionTTL).Unix()
	s.mu.Store(token, exp)
	s.count.Add(1)
	s.scheduleExpiry(token, exp)
	s.save()
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
		s.save()
	}
}

// Count 当前有效会话数（近似）。
func (s *SessionStore) Count() int64 { return s.count.Load() }

// RemainingTTL 返回令牌剩余有效期；令牌无效或已过期时返回 0。
// 用于补发会话 Cookie 时对齐真实过期时间，避免 Cookie 反过来把令牌「续命」。
func (s *SessionStore) RemainingTTL(token string) time.Duration {
	if token == "" {
		return 0
	}
	val, ok := s.mu.Load(token)
	if !ok {
		return 0
	}
	exp, ok := val.(int64)
	if !ok {
		return 0
	}
	d := time.Until(time.Unix(exp, 0))
	if d <= 0 {
		return 0
	}
	return d
}

// save 原子写回磁盘（权限 0600：文件里是可直接登录的令牌）。
// 令牌数量是个位数，每次签发/注销直接落盘即可，不需要脏标记 + 定时合并。
func (s *SessionStore) save() {
	path, fp := s.config()
	if path == "" {
		return
	}
	now := time.Now().Unix()
	snapshot := map[string]int64{}
	s.mu.Range(func(k, v interface{}) bool {
		if exp, ok := v.(int64); ok && exp > now {
			snapshot[k.(string)] = exp
		}
		return true
	})
	data, err := json.Marshal(sessionFile{Fingerprint: fp, Sessions: snapshot})
	if err != nil {
		log.Printf("[AUTH][ERR] 序列化会话失败: %v", err)
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if werr := fsutil.AtomicWrite(path, data, 0o600); werr != nil {
		log.Printf("[AUTH][ERR] 会话落盘失败: %v", werr)
	}
}

// TokenFromRequest 从 Bearer 头或 ?token= 提取。
func TokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

// tokenFromCookie 从会话 Cookie 提取令牌。
// 仅在白名单路径的 GET/HEAD 请求上生效：写操作的 CSRF 面必须保持为零。
func tokenFromCookie(r *http.Request) string {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return ""
	}
	if !cookieAuthPaths[r.URL.Path] {
		return ""
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// SetSessionCookie 下发会话 Cookie，供浏览器原生资源请求携带。
//
// HttpOnly：脚本读不到（前端 XHR 用的是 localStorage 里的 Bearer 令牌，不依赖它）；
// SameSite=Lax：跨站子资源请求（他站 <img> 热链封面）带不上 Cookie，避免被借道滥用；
// Secure 按实际连接协议决定：纯 HTTP 部署（含反代终止 TLS）下不能带，否则 Cookie 会被丢弃。
func SetSessionCookie(w http.ResponseWriter, r *http.Request, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie 注销时清除会话 Cookie，避免「点了退出但图片接口仍能访问」。
func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
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
		// 先看请求头/查询串里的令牌；没有才回落到白名单路径上的会话 Cookie
		//（浏览器原生资源请求 <img>/window.open 不会带 Authorization 头）。
		explicitToken := TokenFromRequest(r)
		token := explicitToken
		if token == "" {
			token = tokenFromCookie(r)
		}
		if !store.Verify(token) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"未认证：请先登录"}`))
			return
		}
		// 老会话升级：浏览器手里有 localStorage 的 Bearer 令牌、却还没有会话 Cookie
		// （本次改动前登录的，或 Cookie 被清理过）。首个带令牌的请求顺手按真实剩余有效期补发，
		// 封面图与日志导出随后立即可用，不必强制用户重新登录一次。
		if explicitToken != "" {
			if c, err := r.Cookie(SessionCookie); err != nil || c.Value != explicitToken {
				if ttl := store.RemainingTTL(explicitToken); ttl > 0 {
					SetSessionCookie(w, r, explicitToken, ttl)
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
