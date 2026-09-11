package recorder

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WatermarkStyle 截图/视频烧录水印样式（与 BuiltinConfig 字段对齐）。
type WatermarkStyle struct {
	Text      string
	Format    string
	Position  string
	FontSize  int
	FontColor string
}

// SanitizeName 剔除路径非法字符并修剪，空结果回退「未命名主播」。
func SanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\t", "")
	name = strings.ReplaceAll(name, " ", " ")
	for _, char := range []string{"\\", "/", ":", "*", "?", "\"", "<", ">", "|"} {
		name = strings.ReplaceAll(name, char, "")
	}
	name = strings.TrimSpace(name)
	name = strings.Trim(name, " ._-")
	if name == "" {
		return "未命名主播"
	}
	return name
}

// FormatDuration 将时长格式化为 X小时X分X秒 / X分X秒。
func FormatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%02d小时%02d分%02d秒", h, m, s)
	}
	return fmt.Sprintf("%02d分%02d秒", m, s)
}

// FormatBytes 将字节数格式化为可读规格。
func FormatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// NormalizeFontColor 将 #RRGGBB[AA] 归一为 FFmpeg 0x 形式。
func NormalizeFontColor(c string) string {
	c = strings.TrimSpace(c)
	if c == "" {
		return "white@0.95"
	}
	if strings.HasPrefix(c, "#") {
		return "0x" + strings.TrimPrefix(c, "#")
	}
	return c
}

// FindFontPath 按可执行文件目录 → 工作目录查找中文字体。
func FindFontPath() string {
	names := []string{"font.ttf", "FZSTK.TTF", "msyh.ttf", "simhei.ttf"}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, n := range names {
			p := filepath.Join(dir, n)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	for _, n := range names {
		if abs, err := filepath.Abs(n); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	return ""
}

// strftimeToGo 将常见 strftime 令牌映射为 Go time layout。
func strftimeToGo(format string) string {
	r := strings.NewReplacer(
		"%Y", "2006",
		"%y", "06",
		"%m", "01",
		"%d", "02",
		"%H", "15",
		"%M", "04",
		"%S", "05",
		"%b", "Jan",
		"%B", "January",
		"%p", "PM",
	)
	return r.Replace(format)
}

func watermarkPrefix(style WatermarkStyle, anchorName string) string {
	textStr := strings.ReplaceAll(style.Text, "'", "")
	if strings.TrimSpace(textStr) == "" {
		textStr = strings.ReplaceAll(anchorName, "'", "")
	}
	return strings.TrimSpace(textStr)
}

// BuildWatermarkText 组装静态时间戳（截图用，抽帧时刻固定）。
func BuildWatermarkText(style WatermarkStyle, anchorName string) string {
	formatStr := strings.ReplaceAll(style.Format, "'", "")
	if formatStr == "" {
		formatStr = "%Y-%m-%d %H:%M:%S"
	}
	fullText := watermarkPrefix(style, anchorName)
	if fullText != "" {
		fullText += " "
	}
	return fullText + time.Now().Format(strftimeToGo(formatStr))
}

// BuildWatermarkTextLive 组装视频烧录用文本。
// 配合 drawtext 的 expansion=strftime：整段文本按 strftime 每帧展开，
// 兼容 FFmpeg 3.4（它不会可靠地展开 %{localtime:fmt}）。
func BuildWatermarkTextLive(style WatermarkStyle, anchorName string) string {
	formatStr := strings.ReplaceAll(style.Format, "'", "")
	if formatStr == "" {
		formatStr = "%Y-%m-%d %H:%M:%S"
	}
	fullText := watermarkPrefix(style, anchorName)
	if fullText != "" {
		fullText += " "
	}
	return fullText + formatStr
}

// DrawtextPos 九宫格坐标。
func DrawtextPos(position string) string {
	switch position {
	case "top-left":
		return "x=20:y=20"
	case "top-right":
		return "x=w-tw-20:y=20"
	case "bottom-left":
		return "x=20:y=h-th-20"
	default:
		return "x=w-tw-20:y=h-th-20"
	}
}

// escapeFilterPath 将文件路径转为 FFmpeg filtergraph 可安全解析的形式。
// Windows 盘符（D:\...）里的冒号是 filtergraph 的参数分隔符，必须转义为 `\:`，
// 且反斜杠统一为 `/`，否则整条 drawtext 滤镜会被 FFmpeg 判定为语法错误而失败。
func escapeFilterPath(p string) string {
	p = filepath.ToSlash(p)
	return strings.ReplaceAll(p, ":", `\:`)
}

// PrepareDrawtextFilter 生成 drawtext 滤镜并落盘临时 textfile；调用方负责删除 textFile。
// live=true：expansion=strftime，时间每帧刷新；false：静态时刻。
func PrepareDrawtextFilter(style WatermarkStyle, anchorName, tag string, live bool) (filter string, textFile string, err error) {
	fontPath := FindFontPath()
	if fontPath == "" {
		return "", "", fmt.Errorf("未找到可用中文字体 (font.ttf)")
	}
	var fullText string
	if live {
		fullText = BuildWatermarkTextLive(style, anchorName)
	} else {
		fullText = BuildWatermarkText(style, anchorName)
	}
	textFileName := fmt.Sprintf("wm_%s_%d.txt", tag, time.Now().UnixNano())
	absTextFile, _ := filepath.Abs(textFileName)
	if werr := os.WriteFile(absTextFile, []byte(fullText), 0644); werr != nil {
		return "", "", werr
	}
	fontSize := style.FontSize
	if fontSize <= 0 {
		fontSize = 38
	}
	expansion := "none"
	if live {
		expansion = "strftime"
	}
	filter = fmt.Sprintf(
		"drawtext=fontfile='%s':textfile='%s':expansion=%s:fontcolor=%s:fontsize=%d:borderw=2:bordercolor=black@0.75:shadowcolor=black@0.5:shadowx=3:shadowy=3:%s",
		escapeFilterPath(fontPath), escapeFilterPath(absTextFile), expansion, NormalizeFontColor(style.FontColor), fontSize, DrawtextPos(style.Position),
	)
	return filter, absTextFile, nil
}

// CurrentWatermarkStyle 从全局 builtinConfig 派生样式（main 包装配后调用）。
func StyleFrom(text, format, position, color string, fontSize int) WatermarkStyle {
	return WatermarkStyle{
		Text:      text,
		Format:    format,
		Position:  position,
		FontSize:  fontSize,
		FontColor: color,
	}
}
