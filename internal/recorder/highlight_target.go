package recorder

import "path/filepath"

// HighlightDirName 是高光产物存放的子目录名。
// 与原片目录平行：<savePath>/<主播>/<日期>/*.ts 是原片，
// <savePath>/<主播>/高光/*.mp4 是高光产物。
const HighlightDirName = "高光"

// ScreenshotDirName 是旁路截图归档的子目录名（<主播>/<日期>/Screenshots/）。
// 截图不属于原片：「只传高光」的上传过滤必须放行它，否则截图永远到不了云端。
// 路径版式常量集中放在这里，与录制落盘（builtin_ffmpeg.go）共用同一来源。
const ScreenshotDirName = "Screenshots"

// HighlightTarget 描述一个需要做高光切片分析的主播及其录像目录。
type HighlightTarget struct {
	Platform   string
	RoomID     string
	AnchorName string
	// SaveDir 该主播的录像根目录（savePath/清洗后主播名），其下按日期分目录存放原片。
	SaveDir string
	// OnlyHighlight 为 true 表示该主播只上传高光、原片不上传。
	OnlyHighlight bool
}

// HighlightTargets 返回开启了高光切片的主播列表。
//
// globalOn 是全局「高光」开关，onlyGlobal 是全局「只传高光」开关；
// 单主播三态（0=跟随全局 / 1=强制开 / 2=强制关）优先于对应的全局值。
// 供离线后处理调度使用：调用方拿到目录后自行扫描切片文件。
func HighlightTargets(globalOn, onlyGlobal bool) []HighlightTarget {
	baseDir := getBuiltinSavePath()
	if baseDir == "" {
		return nil
	}
	var out []HighlightTarget
	for _, t := range GetBuiltinRecorderTasks() {
		if !triStateOn(t.Highlight, globalOn) {
			continue
		}
		// 目录名必须与录制落盘时用同一套清洗规则，否则会指到不存在的目录。
		safe := sanitizeBuiltinFileName(t.AnchorName)
		if safe == "" {
			safe = t.RoomID
		}
		out = append(out, HighlightTarget{
			Platform:      t.Platform,
			RoomID:        t.RoomID,
			AnchorName:    t.AnchorName,
			SaveDir:       filepath.Join(baseDir, safe),
			OnlyHighlight: triStateOn(t.HighlightOnly, onlyGlobal),
		})
	}
	return out
}

// HighlightOnlyPrefixes 返回开启了「只上传高光」的主播录像目录前缀。
//
// 上传管线用它判断：属于这些目录、但不在其「高光」子目录下的文件应当跳过上传。
// 刻意不复用 GetBuiltinRecorderTasks——那个会遍历目录统计大小，而本函数位于上传
// 热路径上会被频繁调用，这里只读内存状态。
func HighlightOnlyPrefixes(globalOnly bool) []string {
	baseDir := getBuiltinSavePath()
	if baseDir == "" {
		return nil
	}
	var out []string
	builtinStatusMap.Range(func(_, value interface{}) bool {
		task := *value.(*BuiltinTaskStatus)
		f := getBuiltinTaskFlags(task.Platform, task.RoomID)
		if !triStateOn(f.HighlightOnly, globalOnly) {
			return true
		}
		safe := sanitizeBuiltinFileName(task.AnchorName)
		if safe == "" {
			safe = task.RoomID
		}
		out = append(out, filepath.Join(baseDir, safe))
		return true
	})
	return out
}

// triStateOn 解析「0=跟随全局 / 1=强制开 / 2=强制关」三态。
func triStateOn(v int, global bool) bool {
	switch v {
	case 1:
		return true
	case 2:
		return false
	}
	return global
}
