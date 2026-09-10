// Package cryptox 提供 AES-GCM 载荷加解密与会话密钥存储（RSA 换钥后的 PFS 隧道）。
package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// Envelope 加密信封载荷，与前端协议一致。
type Envelope struct {
	Encrypted string `json:"encrypted"`
}

// Encrypt 使用 AES-256-GCM 加密明文，输出 Base64(nonce||ciphertext)。
func Encrypt(plaintext []byte, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aesGCM.NonceSize())
	if _, err = io.ReadFull(cryptorand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := aesGCM.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt 解密 Base64(nonce||ciphertext)。
func Decrypt(cryptoText string, key []byte) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(cryptoText)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := aesGCM.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("密文格式被破坏或长度不足")
	}
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return aesGCM.Open(nil, nonce, ciphertext, nil)
}

// SessionStore sessionID -> AES key，带容量上限防止刷爆。
type SessionStore struct {
	m     sync.Map
	count atomic.Int64
	max   int64
}

// NewSessionStore max<=0 时默认 4096。
func NewSessionStore(max int64) *SessionStore {
	if max <= 0 {
		max = 4096
	}
	return &SessionStore{max: max}
}

// Put 存入会话密钥；超限返回错误。
func (s *SessionStore) Put(id string, key []byte) error {
	if id == "" || len(key) == 0 {
		return fmt.Errorf("empty session id or key")
	}
	if s.count.Load() >= s.max {
		return fmt.Errorf("session pool full")
	}
	if _, loaded := s.m.LoadOrStore(id, key); !loaded {
		s.count.Add(1)
	}
	return nil
}

// Get 读取会话密钥。
func (s *SessionStore) Get(id string) ([]byte, bool) {
	v, ok := s.m.Load(id)
	if !ok {
		return nil, false
	}
	return v.([]byte), true
}

// Delete 删除会话。
func (s *SessionStore) Delete(id string) {
	if _, ok := s.m.LoadAndDelete(id); ok {
		s.count.Add(-1)
	}
}

// Count 当前会话数。
func (s *SessionStore) Count() int64 { return s.count.Load() }

// SessionIDFromRequest 从 Header 或 Query 提取 session_id。
func SessionIDFromRequest(r *http.Request) string {
	if sid := r.Header.Get("X-Session-Id"); sid != "" {
		return sid
	}
	return r.URL.Query().Get("session_id")
}

// SessionKeyFromRequest 校验并返回该请求的 AES 密钥。
func (s *SessionStore) SessionKeyFromRequest(r *http.Request) ([]byte, error) {
	sid := SessionIDFromRequest(r)
	if sid == "" {
		return nil, fmt.Errorf("missing session id")
	}
	key, ok := s.Get(sid)
	if !ok {
		return nil, fmt.Errorf("invalid or expired session id")
	}
	return key, nil
}

// DecodeEnvelope 将请求体解为信封。
func DecodeEnvelope(body []byte) (Envelope, error) {
	var env Envelope
	err := json.Unmarshal(body, &env)
	return env, err
}
