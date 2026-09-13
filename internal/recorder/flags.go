// Package recorder — 名单行解析与录屏/截屏开关（纯函数，无全局状态）。
package recorder

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// TaskFlags 单主播「录屏 / 截屏」独立开关与截图间隔、水印、画质、时长覆盖。
// ShotInterval 为该主播专属的截图间隔（秒）；0 表示跟随全局设置。
// Watermark 为该主播的水印三态：0=跟随全局（零值安全），1=强制开，2=强制关，
// 同时作用于截图水印与视频烧录水印；名单行内仍写作直观的 水印:1 / 水印:0。
// Quality 为该主播专属画质（uhd/hd/sd）；空串表示跟随全局设置。
// MaxDuration 为该主播单场直播的最长录制时长（分钟）；0 表示不限制，
// 录满后自动停止且本场不续录，主播下播后自动恢复常规监控。
type TaskFlags struct {
	Record       bool
	Screenshot   bool
	ShotInterval int
	Watermark    int
	Quality      string
	MaxDuration  int
}

// 全局画质档位：与各平台解析器的就近降档逻辑及引擎设置下拉保持一致。
var builtinQualityCodes = map[string]bool{"uhd": true, "hd": true, "sd": true}

// DefaultFlags 缺省双开、间隔与水印跟随全局，兼容旧名单行。
func DefaultFlags() TaskFlags {
	return TaskFlags{Record: true, Screenshot: true, ShotInterval: 0, Watermark: 0}
}

// IsLiveStatus 判定是否处于已接管推流状态。
func IsLiveStatus(s string) bool {
	return s == "录制中" || s == "截屏中"
}

// StripFlagSuffixes 从行尾剥离 ,录屏:x / ,截屏:y / ,截图间隔:n / ,水印:x / ,画质:x / ,录制时长:n。
func StripFlagSuffixes(line string) string {
	line = strings.TrimSpace(line)
	for {
		idx := strings.LastIndex(line, ",")
		if idx < 0 {
			return line
		}
		tail := strings.TrimSpace(line[idx+1:])
		if strings.HasPrefix(tail, "录屏:") || strings.HasPrefix(tail, "截屏:") ||
			strings.HasPrefix(tail, "截图间隔:") || strings.HasPrefix(tail, "水印:") ||
			strings.HasPrefix(tail, "画质:") || strings.HasPrefix(tail, "录制时长:") {
			line = strings.TrimSpace(line[:idx])
			continue
		}
		return line
	}
}

// ParseFlagsFromLine 解析行内开关与截图间隔、水印覆盖；
// 开关缺省双开，间隔缺省 0（跟随全局），水印缺省 -1（跟随全局）。
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
		} else if strings.HasPrefix(part, "截图间隔:") {
			v := strings.TrimSpace(strings.TrimPrefix(part, "截图间隔:"))
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				flags.ShotInterval = n
			}
		} else if strings.HasPrefix(part, "水印:") {
			v := strings.TrimSpace(strings.TrimPrefix(part, "水印:"))
			switch v {
			case "1":
				flags.Watermark = 1
			case "0":
				flags.Watermark = 2
			}
		} else if strings.HasPrefix(part, "画质:") {
			v := strings.TrimSpace(strings.TrimPrefix(part, "画质:"))
			if builtinQualityCodes[v] {
				flags.Quality = v
			}
		} else if strings.HasPrefix(part, "录制时长:") {
			v := strings.TrimSpace(strings.TrimPrefix(part, "录制时长:"))
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				flags.MaxDuration = n
			}
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
	} else if strings.Contains(rawURL, "bilibili.com") || strings.Contains(rawURL, "b23.tv") {
		platform = "Bilibili"
	} else if strings.Contains(rawURL, "twitch.tv") {
		platform = "Twitch"
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
	// 单主播截图间隔仅在显式设置时写回，缺省保持行简洁（运行时跟随全局）
	if flags.ShotInterval > 0 {
		out += fmt.Sprintf(",截图间隔:%d", flags.ShotInterval)
	}
	// 水印仅在强制开/关时写回；0（跟随全局）不写。内部 2 对应行内直观的 水印:0
	if flags.Watermark == 1 || flags.Watermark == 2 {
		out += fmt.Sprintf(",水印:%d", flags.Watermark%2)
	}
	// 单主播画质仅在显式覆盖时写回，缺省保持行简洁（运行时跟随全局）
	if builtinQualityCodes[flags.Quality] {
		out += ",画质:" + flags.Quality
	}
	// 单主播最长录制时长仅在显式设置时写回（分钟），0 = 不限制
	if flags.MaxDuration > 0 {
		out += fmt.Sprintf(",录制时长:%d", flags.MaxDuration)
	}
	return prefix + out
}
