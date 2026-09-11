package recorder

// PrepareVideoDrawtextFilter 视频烧录用 drawtext，时间每帧由 FFmpeg 展开。
func PrepareVideoDrawtextFilter(style WatermarkStyle, anchorName string) (filter string, textFile string, err error) {
	return PrepareDrawtextFilter(style, anchorName, "vid", true)
}

// WatermarkWallClockNote 视频烧录时间源说明。
const WatermarkWallClockNote = "时间每帧实时刷新（expansion=strftime）"
