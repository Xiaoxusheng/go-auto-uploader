package recorder

// PrepareVideoDrawtextFilter 视频烧录用 drawtext（textfile 内不转义冒号）。
func PrepareVideoDrawtextFilter(style WatermarkStyle, anchorName string) (filter string, textFile string, err error) {
	return PrepareDrawtextFilter(style, anchorName, "vid")
}

// WatermarkWallClockNote 视频烧录时间源说明。
const WatermarkWallClockNote = "时间源=编码时系统时钟；直播若略慢于时钟，画面会显得略快"
