package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExtremeIOExceptions 执行极端环境下的文件系统 I/O 异常注入测试。
// 该函数会故意传递不存在的路径、构造破坏性权限的目录来欺骗底层的分析函数，
// 验证系统的旁路抽帧、磁盘容量检测机制是否具备优雅的故障降级和防御性返回能力。
func TestExtremeIOExceptions(t *testing.T) {
	// 测试场景 1: 磁盘容量统计面对完全不存在的路径
	// 预期: 静默返回 0 或不引发 Panic，优雅降级
	t.Run("DiskFreeSpace_InvalidPath", func(t *testing.T) {
		invalidPath := "/path/that/absolutely/does/not/exist/in/the/universe"
		freeSpace := getDiskFreeSpaceStd(invalidPath)
		// 不同的 OS 底层对不存在路径的 df/wmic 表现不同，但绝不能崩溃
		if freeSpace < 0 {
			t.Errorf("底层空间探测返回了非法的负数: %d", freeSpace)
		}
	})

	// 测试场景 2: 旁路抽帧器遇到空目录或无效文件
	// 预期: 由于找不到符合后缀的 TS 切片，应该直接短路返回 false，不占用内存
	t.Run("CoverExtract_EmptyDirectory", func(t *testing.T) {
		tempDir := t.TempDir() // 利用 go test 框架生成安全的临时测试目录
		coverPath := filepath.Join(tempDir, "output_cover.png")

		success := extractBuiltinCoverFromLocalFile(tempDir, "test_prefix", coverPath, "TestAnchor")
		if success {
			t.Error("空目录下提取截帧预期应该失败，却返回了成功")
		}
	})

	// 测试场景 3: 旁路抽帧器强行读取被剥夺权限的残缺目录
	// 预期: 能够捕获 OS 级别的权限拒绝错误，并安全退出流程
	t.Run("CoverExtract_NoPermissionDir", func(t *testing.T) {
		tempDir := t.TempDir()
		restrictedDir := filepath.Join(tempDir, "no_access")

		// 创建目录
		err := os.Mkdir(restrictedDir, 0755)
		if err != nil {
			t.Fatalf("创建临时目录失败: %v", err)
		}

		// 剥夺该目录的一切读写执行权限 (Linux/macOS 适用)
		// 注意: Windows 下 Chmod 行为不同，可能会忽略，但测试主旨在于防御
		_ = os.Chmod(restrictedDir, 0000)
		defer os.Chmod(restrictedDir, 0755) // 测试结束恢复权限以便清理垃圾

		coverPath := filepath.Join(tempDir, "output_cover.png")
		success := extractBuiltinCoverFromLocalFile(restrictedDir, "test_prefix", coverPath, "TestAnchor")
		if success {
			t.Error("面对无权限目录预期提取失败，却返回了成功")
		}
	})
}
