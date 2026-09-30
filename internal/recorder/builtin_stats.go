// Package recorder — 录制统计：按「主播/日期」目录结构聚合磁盘占用、文件数与近 14 天趋势，
// 结果内存缓存 60 秒，避免大目录被前端频繁拖垮 IO。
package recorder

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// AnchorStat 单主播录制占用统计。
type AnchorStat struct {
	Anchor     string `json:"anchor"`      // 主播名（目录名）
	SizeBytes  int64  `json:"size_bytes"`  // 总字节数
	Size       string `json:"size"`        // 格式化大小
	Files      int    `json:"files"`       // 录像文件数（不含截图归档）
	Days       int    `json:"days"`        // 有录像的日期数
	LastRecord string `json:"last_record"` // 最近一次写入时间
}

// DailyStat 单日录制量。
type DailyStat struct {
	Date  string `json:"date"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
}

// RecorderStats 统计快照。
type RecorderStats struct {
	TotalSize   string       `json:"total_size"`
	TotalBytes  int64        `json:"total_bytes"`
	TotalFiles  int          `json:"total_files"`
	Anchors     []AnchorStat `json:"anchors"`
	Daily       []DailyStat  `json:"daily"` // 近 14 天（含当天），按日期倒序
	GeneratedAt string       `json:"generated_at"`
}

var (
	builtinStatsMu     sync.Mutex
	builtinStatsCache  *RecorderStats
	builtinStatsAt     time.Time
	builtinDateDirName = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// getBuiltinRecorderStats 返回统计快照（60 秒缓存窗口）。
func getBuiltinRecorderStats() *RecorderStats {
	builtinStatsMu.Lock()
	defer builtinStatsMu.Unlock()
	if builtinStatsCache != nil && time.Since(builtinStatsAt) < 60*time.Second {
		return builtinStatsCache
	}
	builtinStatsCache = computeRecorderStats()
	builtinStatsAt = time.Now()
	return builtinStatsCache
}

// computeRecorderStats 遍历保存根目录：根目录/主播/日期/文件（跳过 Screenshots 截图归档）。
// 日期目录名即归属日期；散落在主播目录下的历史文件只计入总量，不参与按日趋势。
func computeRecorderStats() *RecorderStats {
	stats := &RecorderStats{GeneratedAt: time.Now().Format("2006-01-02 15:04:05")}

	// 遍历全部落盘根目录（主 + 备选）：存储溢出护栏切换后，
	// 同一主播的录像可能分散在多个根目录，按主播名归并、日期目录名去重。
	dailyIndex := make(map[string]*DailyStat)
	anchorIndex := make(map[string]*AnchorStat)
	anchorLastMod := make(map[string]time.Time)
	anchorDates := make(map[string]map[string]bool)

	for _, base := range RecordRoots() {
		anchors, err := os.ReadDir(base)
		if err != nil {
			continue
		}

		for _, a := range anchors {
			if !a.IsDir() {
				continue
			}
			anchorStat := anchorIndex[a.Name()]
			if anchorStat == nil {
				anchorStat = &AnchorStat{Anchor: a.Name()}
				anchorIndex[a.Name()] = anchorStat
			}
			anchorDir := filepath.Join(base, a.Name())

			dates, err := os.ReadDir(anchorDir)
			if err != nil {
				continue
			}

			for _, d := range dates {
				if !d.IsDir() || !builtinDateDirName.MatchString(d.Name()) {
					continue
				}
				if anchorDates[a.Name()] == nil {
					anchorDates[a.Name()] = make(map[string]bool)
				}
				anchorDates[a.Name()][d.Name()] = true
				dateStat := dailyIndex[d.Name()]
				_ = filepath.WalkDir(filepath.Join(anchorDir, d.Name()), func(path string, entry os.DirEntry, err error) error {
					if err != nil || entry == nil {
						return nil
					}
					if entry.IsDir() {
						// Screenshots 目录为截图归档，不计入录制量统计
						if entry.Name() == "Screenshots" {
							return filepath.SkipDir
						}
						return nil
					}
					info, err := entry.Info()
					if err != nil || info.IsDir() {
						return nil
					}
					anchorStat.SizeBytes += info.Size()
					anchorStat.Files++
					if info.ModTime().After(anchorLastMod[a.Name()]) {
						anchorLastMod[a.Name()] = info.ModTime()
					}
					if dateStat == nil {
						dateStat = &DailyStat{Date: d.Name()}
						dailyIndex[d.Name()] = dateStat
					}
					dateStat.Bytes += info.Size()
					dateStat.Files++
					return nil
				})
			}
		}
	}

	for _, as := range anchorIndex {
		if !anchorLastMod[as.Anchor].IsZero() {
			as.LastRecord = anchorLastMod[as.Anchor].Format("2006-01-02 15:04")
		}
		as.Days = len(anchorDates[as.Anchor])
		if as.Files > 0 {
			as.Size = formatBuiltinBytes(as.SizeBytes)
			stats.Anchors = append(stats.Anchors, *as)
			stats.TotalBytes += as.SizeBytes
			stats.TotalFiles += as.Files
		}
	}

	// 近 14 天趋势（含今天），按日期倒序
	for _, ds := range dailyIndex {
		stats.Daily = append(stats.Daily, *ds)
	}
	sort.Slice(stats.Daily, func(i, j int) bool { return stats.Daily[i].Date > stats.Daily[j].Date })
	if len(stats.Daily) > 14 {
		stats.Daily = stats.Daily[:14]
	}
	sort.Slice(stats.Anchors, func(i, j int) bool { return stats.Anchors[i].SizeBytes > stats.Anchors[j].SizeBytes })
	stats.TotalSize = formatBuiltinBytes(stats.TotalBytes)
	return stats
}

// apiRecorderStats GET /api/v1/builtin_recorder/stats
func apiRecorderStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		hookJSONErr(w, r, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	hookJSONOK(w, r, getBuiltinRecorderStats())
}
