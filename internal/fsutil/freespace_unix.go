//go:build !windows

// Package fsutil 文件系统辅助：原子写、磁盘剩余空间探测。
package fsutil

import (
	"path/filepath"
	"syscall"
)

// FreeSpace 返回路径所在文件系统的剩余空间（字节，非特权用户可用），出错返回 0。
// 走 statfs 系统调用，不依赖外部 df 进程。
func FreeSpace(pathStr string) int64 {
	if pathStr == "" {
		pathStr = "."
	}
	absPath, err := filepath.Abs(pathStr)
	if err != nil {
		absPath = pathStr
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(absPath, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
