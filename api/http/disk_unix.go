//go:build !windows

package httpapi

import (
	"path/filepath"
	"syscall"
)

// getDiskFreeSpaceStd 返回路径所在文件系统的剩余空间（字节），走 statfs 系统调用，
// 不再依赖外部 df 进程。
func getDiskFreeSpaceStd(pathStr string) int64 {
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
