// Package recorder — 名单行解析与录屏/截屏开关（纯函数，无全局状态）。
package recorder

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TaskFlags 单主播「录屏 / 截屏」独立开关与截图间隔、水印、画质、时长覆盖。
// ShotInterval 为该主播专属的截图间隔（秒）；0 表示跟随全局设置。
// Watermark 为该主播的水印三态：0=跟随全局（零值安全），1=强制开，2=强制关，
// 同时作用于截图水印与视频烧录水印；名单行内仍写作直观的 水印:1 / 水印:0。
// Quality 为该主播专属画质（uhd/hd/sd）；空串表示跟随全局设置。
// MaxDuration 为该主播单场直播的最长录制时长（分钟）；0 表示不限制，
// 录满后自动停止且本场不续录，主播下播后自动恢复常规监控。
// SegmentTime 为该主播专属的切片时长（分钟）：录满该时长自动切分为下一个文件，
// 录制不中断、不丢帧；0 表示跟随全局「自动分片时长」。
// Window 为该主播的录制时段（"HH:MM-HH:MM"，结束允许 24:00，可跨午夜）；空串表示全天可录。
// 窗口外只探测不拉流；录制中途跨出窗口会优雅收尾，窗口再次打开后自动续录。
type TaskFlags struct {
	Record       bool
	Screenshot   bool
	ShotInterval int
	Watermark    int
	Quality      string
	MaxDuration  int
	SegmentTime  int
	Window       string
	// Highlight 为该主播的高光切片三态：0=跟随全局（零值安全），1=强制开，2=强制关；
	// 名单行内仍写作直观的 高光:1 / 高光:0。
	Highlight int
	// HighlightOnly 为该主播「只上传高光」三态：0=跟随全局，1=只传高光（原片不上传），
	// 2=原片与高光都传；名单行内写作 只传高光:1 / 只传高光:0。
	HighlightOnly int
}

// 全局画质档位：与各平台解析器的就近降档逻辑及引擎设置下拉保持一致。
var builtinQualityCodes = map[string]bool{"uhd": true, "hd": true, "sd": true}

// parseClockMinutes 解析 "HH:MM" 为当日分钟数；结束侧允许 24:00（=1440）。
func parseClockMinutes(s string) (int, bool) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || m < 0 || m > 59 {
		return 0, false
	}
	if h == 24 && m == 0 {
		return 1440, true
	}
	if h < 0 || h > 23 {
		return 0, false
	}
	return h*60 + m, true
}

// parseRecordWindow 解析 "HH:MM-HH:MM" 时段；起止相同视为无效（不设窗口）。
func parseRecordWindow(w string) (start, end int, ok bool) {
	parts := strings.SplitN(w, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	s, ok1 := parseClockMinutes(strings.TrimSpace(parts[0]))
	e, ok2 := parseClockMinutes(strings.TrimSpace(parts[1]))
	if !ok1 || !ok2 || s == e {
		return 0, 0, false
	}
	return s, e, true
}

// formatRecordWindow 将分钟区间格式化为 "HH:MM-HH:MM"（1440 → "24:00"）。
func formatRecordWindow(start, end int) string {
	clock := func(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }
	return clock(start) + "-" + clock(end)
}

// inRecordingWindow 判定 now 是否落在录制时段内。
// 窗口为空或格式非法时按「无限制」处理（fail-open，不影响录制）。
func inRecordingWindow(window string, now time.Time) bool {
	window = strings.TrimSpace(window)
	if window == "" {
		return true
	}
	start, end, ok := parseRecordWindow(window)
	if !ok {
		return true
	}
	cur := now.Hour()*60 + now.Minute()
	if start < end {
		return cur >= start && cur < end
	}
	// 跨午夜窗口（如 20:00-02:00）
	return cur >= start || cur < end
}

// DefaultFlags 缺省双开、间隔与水印跟随全局，兼容旧名单行。
func DefaultFlags() TaskFlags {
	return TaskFlags{Record: true, Screenshot: true, ShotInterval: 0, Watermark: 0}
}

// IsLiveStatus 判定是否处于已接管推流状态。
func IsLiveStatus(s string) bool {
	return s == "录制中" || s == "截屏中"
}

// StripFlagSuffixes 从行尾剥离 ,录屏:x / ,截屏:y / ,截图间隔:n / ,水印:x / ,画质:x / ,录制时长:n / ,切片:n / ,时段:HH:MM-HH:MM。
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
			strings.HasPrefix(tail, "画质:") || strings.HasPrefix(tail, "录制时长:") ||
			strings.HasPrefix(tail, "切片:") || strings.HasPrefix(tail, "时段:") ||
			strings.HasPrefix(tail, "高光:") || strings.HasPrefix(tail, "只传高光:") {
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
		} else if strings.HasPrefix(part, "切片:") {
			v := strings.TrimSpace(strings.TrimPrefix(part, "切片:"))
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				flags.SegmentTime = n
			}
		} else if strings.HasPrefix(part, "时段:") {
			v := strings.TrimSpace(strings.TrimPrefix(part, "时段:"))
			if s, e, ok := parseRecordWindow(v); ok {
				flags.Window = formatRecordWindow(s, e)
			}
		} else if strings.HasPrefix(part, "高光:") {
			v := strings.TrimSpace(strings.TrimPrefix(part, "高光:"))
			switch v {
			case "1":
				flags.Highlight = 1
			case "0":
				flags.Highlight = 2
			}
		} else if strings.HasPrefix(part, "只传高光:") {
			v := strings.TrimSpace(strings.TrimPrefix(part, "只传高光:"))
			switch v {
			case "1":
				flags.HighlightOnly = 1
			case "0":
				flags.HighlightOnly = 2
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
	// 单主播切片时长仅在显式覆盖时写回（分钟），0 = 跟随全局「自动分片时长」
	if flags.SegmentTime > 0 {
		out += fmt.Sprintf(",切片:%d", flags.SegmentTime)
	}
	// 单主播录制时段仅在显式设置且格式合法时写回（空 = 全天可录）
	if _, _, ok := parseRecordWindow(strings.TrimSpace(flags.Window)); ok {
		out += ",时段:" + strings.TrimSpace(flags.Window)
	}
	// 单主播高光三态仅在强制开/关时写回；0（跟随全局）不写。内部 2 对应行内直观的 高光:0
	if flags.Highlight == 1 || flags.Highlight == 2 {
		out += fmt.Sprintf(",高光:%d", flags.Highlight%2)
	}
	// 单主播「只传高光」三态，同上
	if flags.HighlightOnly == 1 || flags.HighlightOnly == 2 {
		out += fmt.Sprintf(",只传高光:%d", flags.HighlightOnly%2)
	}
	return prefix + out
}
