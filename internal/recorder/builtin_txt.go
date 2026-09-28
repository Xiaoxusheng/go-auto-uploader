package recorder

import (
	"os"
	"strings"
)

func parseBuiltinLine(line string) (isPaused bool, platform string, roomID string, customName string, rawURL string, flags BuiltinTaskFlags) {
	return ParseLine(line)
}

// rebuildBuiltinLineWithFlags 在名单行上写回/更新录屏截屏后缀
func rebuildBuiltinLineWithFlags(trimmedLine string, flags BuiltinTaskFlags) string {
	return RebuildLineWithFlags(trimmedLine, flags)
}

// syncBuiltinAnchorToTxt 依据前端指令对本地配置文件里的内容作增、删、改并落地
func syncBuiltinAnchorToTxt(action string, platform, roomID string, rawLine string) {
	builtinAnchorLinesMutex.Lock()
	defer builtinAnchorLinesMutex.Unlock()

	content, err := os.ReadFile("builtin_urls.txt")
	if err != nil && !os.IsNotExist(err) {
		// 文件存在但读取失败（被占用/权限异常等）时直接放弃，绝不写盘，
		// 否则会把整份名单覆盖成空文件，造成不可逆的数据丢失。
		return
	}
	var lines []string
	if len(content) > 0 {
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
			} else if action == "add" {
				// 名单里已有该主播（典型为暂停态）时，重复添加按「恢复」处理：
				// 若不加分支直接落空，这行会被整体丢弃——实测添加一个暂停中的主播
				// 会把名单行删掉，重启后主播凭空消失；且 API 层 add 已启动监控，
				// 名单去掉 # 才能与内存运行态一致，否则 3s 后热重载会把任务打回暂停。
				newLines = append(newLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "#")))
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
