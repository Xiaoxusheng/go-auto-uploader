package main

import (
	"os"
	"strings"
	"upload/internal/recorder"
)

func parseBuiltinLine(line string) (isPaused bool, platform string, roomID string, customName string, rawURL string, flags BuiltinTaskFlags) {
	return recorder.ParseLine(line)
}

// rebuildBuiltinLineWithFlags 在名单行上写回/更新录屏截屏后缀
func rebuildBuiltinLineWithFlags(trimmedLine string, flags BuiltinTaskFlags) string {
	return recorder.RebuildLineWithFlags(trimmedLine, flags)
}

// syncBuiltinAnchorToTxt 依据前端指令对本地配置文件里的内容作增、删、改并落地
func syncBuiltinAnchorToTxt(action string, platform, roomID string, rawLine string) {
	builtinAnchorLinesMutex.Lock()
	defer builtinAnchorLinesMutex.Unlock()

	content, err := os.ReadFile("builtin_urls.txt")
	var lines []string
	if err == nil {
		lines = strings.Split(string(content), "\n")
	}

	var newLines []string
	found := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		isP, p, rid, _, _, curFlags := parseBuiltinLine(trimmed)
		if p == platform && rid == roomID {
			found = true
			if action == "delete" {
				continue
			} else if action == "pause" {
				if !isP {
					newLines = append(newLines, rebuildBuiltinLineWithFlags(trimmed, curFlags))
					// rebuild 已保留 #；若原本未暂停需补上
					if !strings.HasPrefix(newLines[len(newLines)-1], "#") {
						newLines[len(newLines)-1] = "#" + newLines[len(newLines)-1]
					}
				} else {
					newLines = append(newLines, trimmed)
				}
			} else if action == "resume" {
				if isP {
					newLines = append(newLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "#")))
				} else {
					newLines = append(newLines, trimmed)
				}
			}
		} else {
			newLines = append(newLines, trimmed)
		}
	}

	if !found && action == "add" && rawLine != "" {
		newLines = append(newLines, strings.TrimSpace(rawLine))
	}

	os.WriteFile("builtin_urls.txt", []byte(strings.Join(newLines, "\n")+"\n"), 0644)
}

// persistBuiltinFlagsToTxt 将单主播录屏/截屏开关写回 builtin_urls.txt
func persistBuiltinFlagsToTxt(platform, roomID string, flags BuiltinTaskFlags) {
	builtinAnchorLinesMutex.Lock()
	defer builtinAnchorLinesMutex.Unlock()

	content, err := os.ReadFile("builtin_urls.txt")
	if err != nil {
		return
	}
	lines := strings.Split(string(content), "\n")
	changed := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		_, p, rid, _, _, _ := parseBuiltinLine(trimmed)
		if p == platform && rid == roomID {
			updated := rebuildBuiltinLineWithFlags(trimmed, flags)
			if updated != trimmed {
				lines[i] = updated
				changed = true
			}
		}
	}
	if changed {
		_ = os.WriteFile("builtin_urls.txt", []byte(strings.Join(lines, "\n")+"\n"), 0644)
	}
}

// ==========================================
// ✨ 新增：抖音短链接无头浏览器深度解析 + HTTP 保底
// ==========================================

// ExtractBuiltinDouyinLiveURL 针对用户输入的短链接，采取无头浏览器和原生HTTP双擎重定向拿取长连接
