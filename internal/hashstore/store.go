// Package hashstore 管理上传秒传用的文件 SHA-256 集合。
// 内存 sync.Map 承载查询，磁盘为每行一个哈希的追记文本；写入用文件锁串行化。
package hashstore

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Store 是可注入的哈希库实例，避免包级全局状态。
type Store struct {
	path  string
	mu    sync.Mutex
	cache sync.Map
}

// New 创建指向 path 的哈希库（不自动加载，需调用 Load）。
func New(path string) *Store {
	return &Store{path: path}
}

// Repath 切换落盘路径（启动时重定向到 config.dataDir）。
// 保持实例标识不变，避免外部提前捕获的引用失效。须在 Load 之前调用。
func (s *Store) Repath(path string) {
	s.mu.Lock()
	s.path = path
	s.mu.Unlock()
}

// FileHash 计算文件 SHA-256 十六进制串；失败返回空串。
func FileHash(path string) string {
	f, err := os.Open(path)
	if err != nil {
		log.Printf("[HASH][ERR] 无法打开文件计算哈希 %s: %v", path, err)
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		log.Printf("[HASH][ERR] 读取文件计算哈希失败 %s: %v", path, err)
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Load 从磁盘一次性灌入内存；文件不存在时静默返回。
func (s *Store) Load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		s.cache.Store(line, struct{}{})
		count++
	}
	log.Printf("[HASH] 已从磁盘加载 %d 条哈希记录到内存集合中", count)
}

// Exists O(1) 查询。
func (s *Store) Exists(hash string) bool {
	if hash == "" {
		return false
	}
	_, ok := s.cache.Load(hash)
	return ok
}

// Save 先写内存再追加落盘。
func (s *Store) Save(hash string) {
	if hash == "" {
		return
	}
	s.cache.Store(hash, struct{}{})

	s.mu.Lock()
	defer s.mu.Unlock()
	// 父目录不存在时先补建，避免静默丢失秒传记录
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("[HASH][ERR] 创建哈希库目录失败 %s: %v", dir, err)
			return
		}
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[HASH][ERR] 打开哈希库文件失败: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(hash + "\n"); err != nil {
		log.Printf("[HASH][ERR] 追加哈希失败: %v", err)
	}
}

// Len 统计内存中哈希条数（用于诊断）。
func (s *Store) Len() int {
	n := 0
	s.cache.Range(func(_, _ any) bool { n++; return true })
	return n
}
