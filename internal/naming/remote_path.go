package naming

import (
	"path/filepath"
	"strings"
)

// DetectRoot 判断 path 是否落在 roots 之下，返回所属 root；无匹配返回空串。
func DetectRoot(path string, roots []string) string {
	path = filepath.Clean(path)
	for _, d := range roots {
		root := filepath.Clean(strings.TrimSpace(d))
		rel, err := filepath.Rel(root, path)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return root
		}
	}
	return ""
}

// CleanRemoteDir 清洗相对目录：去掉前导 -_.，空段回退 streamer_dir。
func CleanRemoteDir(relDir string) string {
	parts := strings.Split(filepath.ToSlash(relDir), "/")
	for i, part := range parts {
		cleanPart := strings.TrimLeft(part, "-_.")
		if cleanPart == "" {
			cleanPart = "streamer_dir"
		}
		parts[i] = cleanPart
	}
	return strings.Join(parts, "/")
}

// BuildRemotePath 组装 OpenList 目标路径 safeBase/cleanDir/cleanName。
func BuildRemotePath(safeBase, relDir, baseName string) string {
	name := CleanFileName(baseName)
	return filepath.ToSlash(filepath.Join(safeBase, CleanRemoteDir(relDir), name))
}
