//go:build windows

package httpapi

import (
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// getDiskFreeSpaceStd 返回路径所在卷对当前用户可用的剩余空间（字节）。
// 直接调用 Win32 GetDiskFreeSpaceEx：不依赖外部进程。
// （旧实现走 wmic，但 wmic 已被新版 Windows 11 移除，会导致剩余空间恒为 0。）
func getDiskFreeSpaceStd(pathStr string) int64 {
	if pathStr == "" {
		pathStr = "."
	}
	absPath, err := filepath.Abs(pathStr)
	if err != nil {
		absPath = pathStr
	}
	p, err := syscall.UTF16PtrFromString(absPath)
	if err != nil {
		return 0
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0
	}
	return int64(avail)
}
