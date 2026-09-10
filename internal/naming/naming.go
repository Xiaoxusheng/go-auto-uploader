// Package naming 负责上传路径上的文件名清洗与主播名解析，与扫描/上传解耦。
package naming

import (
	"path/filepath"
	"strings"
)

// CleanFileName 剔除表情/非法符号，扩展名仅保留字母数字；空结果回退为 file。
func CleanFileName(name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)

	cleanExt := ext
	if ext != "" {
		var eb strings.Builder
		eb.WriteRune('.')
		for _, r := range strings.TrimPrefix(ext, ".") {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				eb.WriteRune(r)
			}
		}
		cleanExt = eb.String()
		if cleanExt == "." {
			cleanExt = ""
		}
	}

	var b strings.Builder
	lastDash := false
	for _, r := range base {
		if (r >= 0x4E00 && r <= 0x9FFF) || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}

	res := strings.Trim(b.String(), "-")
	if res == "" {
		res = "file"
	}
	return res + cleanExt
}

// DetectStreamer 从远端路径解析主播目录名。
// 兼容 `/_safe_uploads/streamer/file` 与 `/home/_safe_uploads/streamer/file`：
// 优先取 `_safe_uploads` 的下一段，否则回退为倒数第二段（文件父目录）。
func DetectStreamer(remote string) string {
	trimmed := strings.Trim(remote, "/")
	if trimmed == "" {
		return "未知"
	}
	parts := strings.Split(trimmed, "/")
	for i, p := range parts {
		if strings.Contains(p, "_safe_uploads") && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return "未知"
}
