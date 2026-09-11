package main

import (
	"os"
	"path/filepath"
	"testing"

	"upload/internal/recorder"
)

func TestExtremeIOExceptions(t *testing.T) {
	t.Run("DiskFreeSpace_InvalidPath", func(t *testing.T) {
		invalidPath := "/path/that/absolutely/does/not/exist/in/the/universe"
		freeSpace := diskFreeSpace(invalidPath)
		if freeSpace < 0 {
			t.Errorf("底层空间探测返回了非法的负数: %d", freeSpace)
		}
	})

	t.Run("CoverExtract_EmptyDirectory", func(t *testing.T) {
		tempDir := t.TempDir()
		coverPath := filepath.Join(tempDir, "output_cover.png")
		if recorder.ExtractCoverFromLocalFile(tempDir, "test_prefix", coverPath, "TestAnchor") {
			t.Error("空目录下提取截帧预期应该失败，却返回了成功")
		}
	})

	t.Run("CoverExtract_NoPermissionDir", func(t *testing.T) {
		tempDir := t.TempDir()
		restrictedDir := filepath.Join(tempDir, "no_access")
		if err := os.Mkdir(restrictedDir, 0755); err != nil {
			t.Fatalf("创建临时目录失败: %v", err)
		}
		_ = os.Chmod(restrictedDir, 0000)
		defer os.Chmod(restrictedDir, 0755)
		coverPath := filepath.Join(tempDir, "output_cover.png")
		if recorder.ExtractCoverFromLocalFile(restrictedDir, "test_prefix", coverPath, "TestAnchor") {
			t.Error("面对无权限目录预期提取失败，却返回了成功")
		}
	})
}
