// Package convert 在上传前将直播 TS 切片无损 remux 为 MP4。
package convert

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// IsTS 判断是否为 .ts 直播切片（大小写不敏感）。
func IsTS(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".ts")
}

// IsArtifact 中间产物（.part/.tmp）不得进入上传链路。
func IsArtifact(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".tmp")
}

// TSToMP4 将 tsPath 无损封装为同名 .mp4。
// ffmpegBin 为空时走 PATH。先写 .mp4.part 再原子改名。
func TSToMP4(tsPath, ffmpegBin string) (string, error) {
	if ffmpegBin == "" || ffmpegBin == "ffmpeg" {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegBin = p
		}
	}
	if ffmpegBin == "" {
		return "", fmt.Errorf("未找到 ffmpeg")
	}

	ext := filepath.Ext(tsPath)
	base := strings.TrimSuffix(tsPath, ext)
	mp4Path := base + ".mp4"
	partPath := mp4Path + ".part"

	attempts := [][]string{
		{"-y", "-i", tsPath, "-c", "copy", "-bsf:a", "aac_adtstoasc", "-movflags", "+faststart", partPath},
		{"-y", "-i", tsPath, "-c", "copy", "-movflags", "+faststart", partPath},
	}

	var lastErr error
	for i, args := range attempts {
		start := time.Now()
		cmd := exec.Command(ffmpegBin, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			lastErr = fmt.Errorf("尝试#%d失败: %v / %s", i+1, err, strings.TrimSpace(stderr.String()))
			_ = os.Remove(partPath)
			continue
		}
		info, err := os.Stat(partPath)
		if err != nil || info.Size() == 0 {
			lastErr = fmt.Errorf("输出为空: %v", err)
			_ = os.Remove(partPath)
			continue
		}
		if err := os.Rename(partPath, mp4Path); err != nil {
			lastErr = err
			_ = os.Remove(partPath)
			continue
		}
		log.Printf("[CONVERT] ✅ TS→MP4 成功: %s → %s (%.2f MB, 耗时 %s)",
			filepath.Base(tsPath), filepath.Base(mp4Path), float64(info.Size())/1024/1024, time.Since(start).Truncate(time.Millisecond))
		return mp4Path, nil
	}
	return "", fmt.Errorf("TS 转 MP4 失败: %v", lastErr)
}
