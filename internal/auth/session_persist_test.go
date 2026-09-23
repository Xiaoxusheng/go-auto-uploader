package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// writeSessionFile 按落盘格式写一个会话文件。
func writeSessionFile(t *testing.T, path, user, pass string, sessions map[string]int64) {
	t.Helper()
	raw, err := json.Marshal(sessionFile{
		Fingerprint: credentialFingerprint(user, pass),
		Sessions:    sessions,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// 回归：控制台登录令牌必须跨进程重启存活。
// 旧实现是纯内存的，服务每次重启（含每次部署）都会让所有浏览器掉回登录页，
// 用户得重新输一遍密码。
func TestSessionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")

	before := NewSessionStore()
	before.Init(path, "admin", "admin")
	tok := before.Issue()
	if tok == "" {
		t.Fatal("签发令牌失败")
	}

	// 模拟进程重启：全新的 store，只共享同一个落盘文件
	after := NewSessionStore()
	after.Init(path, "admin", "admin")
	if !after.Verify(tok) {
		t.Fatal("重启后旧令牌应当仍然有效")
	}
	if got := after.Count(); got != 1 {
		t.Fatalf("恢复的会话数 = %d, want 1", got)
	}
}

// 注销必须同步落盘，否则重启后已登出的令牌会"复活"。
func TestRevokePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")

	before := NewSessionStore()
	before.Init(path, "admin", "admin")
	tok := before.Issue()
	before.Revoke(tok)

	after := NewSessionStore()
	after.Init(path, "admin", "admin")
	if after.Verify(tok) {
		t.Fatal("已注销的令牌不应在重启后复活")
	}
}

// 改密码必须能踢掉旧会话：令牌活 30 天且跨重启，否则轮换密码形同虚设。
func TestCredentialChangeInvalidatesSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")

	before := NewSessionStore()
	before.Init(path, "admin", "admin")
	tok := before.Issue()

	// 只改密码
	byPass := NewSessionStore()
	byPass.Init(path, "admin", "new-secret")
	if byPass.Verify(tok) {
		t.Error("改密码后旧令牌应当失效")
	}

	// 只改账号
	byUser := NewSessionStore()
	byUser.Init(path, "boss", "admin")
	if byUser.Verify(tok) {
		t.Error("改账号后旧令牌应当失效")
	}

	// 凭据没变则照常恢复
	same := NewSessionStore()
	same.Init(path, "admin", "admin")
	if !same.Verify(tok) {
		t.Error("凭据未变时应当照常恢复会话")
	}
}

// 线上可能残留旧版「裸 map」格式的会话文件（{"<token>": 过期秒}）。
// 升级后必须照常恢复这些会话，否则一次部署就把所有人踢回登录页。
func TestLegacySessionFileIsAdopted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	raw, err := json.Marshal(map[string]int64{
		"legacytoken": time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewSessionStore()
	s.Init(path, "admin", "admin")
	if !s.Verify("legacytoken") {
		t.Fatal("旧格式会话应当被接管，而不是全部作废")
	}

	// 接管后必须立刻升级为带指纹的新格式，否则「改密码踢会话」对它不生效
	upgraded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f sessionFile
	if err := json.Unmarshal(upgraded, &f); err != nil {
		t.Fatalf("升级后应当是带 fingerprint 的新格式: %v", err)
	}
	if f.Fingerprint != credentialFingerprint("admin", "admin") {
		t.Error("升级后应当写入当前凭据指纹")
	}
	if _, ok := f.Sessions["legacytoken"]; !ok {
		t.Error("升级不应丢失原有会话")
	}
}

// 载入时应丢弃已过期的会话。
func TestExpiredSessionPrunedOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	writeSessionFile(t, path, "admin", "admin", map[string]int64{
		"deadbeef": time.Now().Add(-time.Hour).Unix(),
		"alive":    time.Now().Add(time.Hour).Unix(),
	})

	s := NewSessionStore()
	s.Init(path, "admin", "admin")
	if s.Verify("deadbeef") {
		t.Error("过期令牌不应被载入")
	}
	if !s.Verify("alive") {
		t.Error("未过期令牌应当被载入")
	}
	if got := s.Count(); got != 1 {
		t.Errorf("会话数 = %d, want 1", got)
	}
}

// 会话文件里是可直接登录的令牌，权限必须是 0600。
func TestSessionFilePermission(t *testing.T) {
	if runtime.GOOS == "windows" {
		// NTFS 不认 POSIX 权限位，os.Chmod 只能切只读位，这里必然量到 666。
		// 生产跑 Linux，权限检查在那里才有意义。
		t.Skip("Windows 不支持 POSIX 权限位")
	}
	path := filepath.Join(t.TempDir(), "sessions.json")
	s := NewSessionStore()
	s.Init(path, "admin", "admin")
	s.Issue()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("会话文件未落盘: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("会话文件权限 = %o, want 600", perm)
	}
}

// 损坏的会话文件不应让服务起不来，按未登录处理即可。
func TestCorruptSessionFileIsTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSessionStore()
	s.Init(path, "admin", "admin")
	if got := s.Count(); got != 0 {
		t.Errorf("会话数 = %d, want 0", got)
	}
	if s.Verify("anything") {
		t.Error("损坏文件不应放行任何令牌")
	}
}

// 不调用 Init（纯内存模式）时行为应与旧版一致。
func TestMemoryOnlyWhenInitNotCalled(t *testing.T) {
	s := NewSessionStore()
	tok := s.Issue()
	if tok == "" || !s.Verify(tok) {
		t.Fatal("纯内存模式应正常工作")
	}
	s.Revoke(tok)
	if s.Verify(tok) {
		t.Fatal("注销应生效")
	}
}

// 未登录时不应留下会话文件。
func TestNoFileWhenNoSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	s := NewSessionStore()
	s.Init(path, "admin", "admin")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("没有会话时不应创建文件, err = %v", err)
	}
}

// TTL 至少要有天级，否则"记住登录"没有意义。
func TestSessionTTLIsLongEnough(t *testing.T) {
	if SessionTTL < 7*24*time.Hour {
		t.Fatalf("SessionTTL = %v，太短（用户仍会频繁被要求重新登录）", SessionTTL)
	}
}
