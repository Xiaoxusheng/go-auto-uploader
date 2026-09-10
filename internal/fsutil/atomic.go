// Package fsutil 提供崩溃安全的本地文件写入原语。
// 所有重要 JSON/文本落盘应走 AtomicWrite，避免进程被杀时文件写一半。
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// AtomicWrite 将 data 先写入同目录临时文件，再 rename 覆盖目标。
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	// 失败时清理临时文件
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		// Windows 上可能失败，忽略
		_ = err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	tmpName = "" // 已成功，defer 不再删除
	return nil
}
