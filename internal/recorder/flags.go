// Package recorder — 名单行解析与录屏/截屏开关（纯函数，无全局状态）。
package recorder

import (
	"fmt"
	"net/url"
	"strings"
)

// TaskFlags 单主播「录屏 / 截屏」独立开关。
type TaskFlags struct {
	Record     bool
	Screenshot bool
}

// DefaultFlags 缺省双开，兼容旧名单行。
func DefaultFlags() TaskFlags {
	return TaskFlags{Record: true, Screenshot: true}
}

// IsLiveStatus 判定是否处于已接管推流状态。
func IsLiveStatus(s string) bool {
	return s == "录制中" || s == "截屏中"
}

// StripFlagSuffixes 从行尾剥离 ,录屏:x / ,截屏:y。
func StripFlagSuffixes(line string) string {
	line = strings.TrimSpace(line)
	for {
		idx := strings.LastIndex(line, ",")
		if idx < 0 {
			return line
		}
		tail := strings.TrimSpace(line[idx+1:])
		if strings.HasPrefix(tail, "录屏:") || strings.HasPrefix(tail, "截屏:") {
			line = strings.TrimSpace(line[:idx])
			continue
		}
		return line
	}
}

// ParseFlagsFromLine 解析行内开关；缺省双开。
func ParseFlagsFromLine(line string) TaskFlags {
	flags := DefaultFlags()
	for _, part := range strings.Split(line, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "录屏:") {
			v := strings.TrimSpace(strings.TrimPrefix(part, "录屏:"))
			flags.Record = v != "0" && !strings.EqualFold(v, "off") && !strings.EqualFold(v, "false")
		} else if strings.HasPrefix(part, "截屏:") {
			v := strings.TrimSpace(strings.TrimPrefix(part, "截屏:"))
			flags.Screenshot = v != "0" && !strings.EqualFold(v, "off") && !strings.EqualFold(v, "false")
		}
	}
	return flags
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ExtractRoomID 从各类直播间 URL 提取统一房间 ID。
func ExtractRoomID(input string) string {
	input = strings.TrimSpace(input)
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		u, err := url.Parse(input)
		if err == nil {
			path := strings.Trim(u.Path, "/")
			segments := strings.Split(path, "/")

			if strings.Contains(u.Host, "sooplive.co.kr") || strings.Contains(u.Host, "afreecatv.com") || strings.Contains(u.Host, "sooplive.com") {
				if len(segments) > 0 {
					return segments[0]
				}
			}

			if len(segments) > 0 {
				return segments[len(segments)-1]
			}
		}
	}
	return input
}

// ParseLine 解析名单一行：暂停前缀、平台、房间号、主播名、URL、开关。
func ParseLine(line string) (isPaused bool, platform string, roomID string, customName string, rawURL string, flags TaskFlags) {
	line = strings.TrimSpace(line)
	flags = DefaultFlags()
	if line == "" {
		return
	}

	if strings.HasPrefix(line, "#") {
		isPaused = true
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
	}

	flags = ParseFlagsFromLine(line)
	line = StripFlagSuffixes(line)

	if idx := strings.LastIndex(line, ",主播:"); idx != -1 {
		customName = strings.TrimSpace(line[idx+len(",主播:"):])
		rawURL = strings.TrimSpace(line[:idx])
	} else if idx := strings.LastIndex(line, ", 主播:"); idx != -1 {
		customName = strings.TrimSpace(line[idx+len(", 主播:"):])
		rawURL = strings.TrimSpace(line[:idx])
	} else if idx := strings.LastIndex(line, ","); idx != -1 {
		customName = strings.TrimSpace(line[idx+1:])
		rawURL = strings.TrimSpace(line[:idx])
	} else {
		rawURL = line
	}

	if strings.Contains(rawURL, "douyin.com") || strings.Contains(rawURL, "amemv.com") || strings.Contains(rawURL, "iesdouyin.com") || strings.Contains(rawURL, "douyin") {
		platform = "Douyin"
	} else if strings.Contains(rawURL, "kuaishou.com") || strings.Contains(rawURL, "chenzhongtech.com") {
		platform = "Kuaishou"
	} else if strings.Contains(rawURL, "sooplive.co.kr") || strings.Contains(rawURL, "afreecatv.com") || strings.Contains(rawURL, "sooplive.com") {
		platform = "Soop"
	}

	roomID = ExtractRoomID(rawURL)
	return
}

// RebuildLineWithFlags 在名单行上写回开关后缀，保持主播名与暂停前缀。
func RebuildLineWithFlags(trimmedLine string, flags TaskFlags) string {
	isPaused, _, _, customName, rawURL, _ := ParseLine(trimmedLine)
	if rawURL == "" {
		return trimmedLine
	}
	prefix := ""
	if isPaused {
		prefix = "#"
	}
	out := rawURL
	if customName != "" {
		out += ",主播:" + customName
	}
	out += fmt.Sprintf(",录屏:%d,截屏:%d", boolToInt(flags.Record), boolToInt(flags.Screenshot))
	return prefix + out
}
