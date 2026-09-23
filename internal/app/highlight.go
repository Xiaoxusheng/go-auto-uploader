package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"upload/internal/config"
	"upload/internal/hashstore"
	"upload/internal/highlight"
	"upload/internal/recorder"
)

// 高光分析的调度节奏与稳定性判据。
const (
	// highlightScanInterval 扫描周期。切片落盘后不必急着分析。
	highlightScanInterval = 2 * time.Minute
	// highlightStableAge 切片 mtime 超过此值才认为「已写完」。
	// 录制中的分片一直在写，过早分析会读到半截文件。
	highlightStableAge = 3 * time.Minute
	// highlightMinClipSize 小于此大小的分片没有分析价值（多为会话收尾碎片）。
	highlightMinClipSize = 1 << 20 // 1MB
	highlightStatusName  = "highlight_status.json"
	highlightTimeout     = 60 * time.Minute
	// highlightBatchSize 每轮最多分析的切片数。一轮只做一个的话，积压永远追不上
	// 上传流程删除源文件的速度；做太多又会长时间占住调度协程、和录制抢 IO。
	highlightBatchSize = 3
	// highlightClaimTTL 认领后的最长保留时间。认领的前提是高光循环会来处理它，
	// 若循环停摆（服务暂停、协程卡住）这些文件会一直占盘，所以超时即放弃认领。
	highlightClaimTTL = 2 * time.Hour
	// maxHighlightAttempts 同一个切片最多尝试分析几次。
	// 失败不再是一次性死判：切片可能只是赶上了转换进程正在读同一个文件、
	// 或磁盘 IO 抖动 —— 线上就有一个完全正常的切片因一次这样的失败被永久跳过。
	maxHighlightAttempts = 3
)

// highlightEntry 是单个切片的分析结果记录，用于避免重复分析。
type highlightEntry struct {
	AnalyzedAt string `json:"analyzed_at"`
	Segments   int    `json:"segments"`
	Output     string `json:"output,omitempty"` // 产出的高光文件名；空 = 未检出
	Size       int64  `json:"size,omitempty"`
	Err        string `json:"err,omitempty"`
	// Attempts 累计失败次数，只在 Err 非空时有意义；达到上限才彻底放弃。
	Attempts int `json:"attempts,omitempty"`
}

var (
	highlightMu       sync.Mutex
	highlightState    = map[string]highlightEntry{}
	highlightStatePos string
	highlightLoaded   bool

	// highlightClaimed 记录「上传流程已处理完、但高光还没分析」的源文件（path → 认领时刻）。
	// uploader.Pipeline 删源前通过 highlightClaim 询问，认领成功就不删，
	// 改由高光模块分析完（无论成败）自行清理。
	highlightClaimed sync.Map

	// highlightTargetDirs 是「开了高光的主播录像目录」前缀缓存（统一正斜杠）。
	// 认领判断不能每次都去算 HighlightTargets —— 它会调 GetBuiltinRecorderTasks，
	// 而后者要 walk 每个主播目录统计 FileSize，代价极高。
	highlightTargetDirsMu sync.RWMutex
	highlightTargetDirs   []string
)

// highlightLoop 周期性扫描录像目录，对已写完的切片做高光分析。
//
// 串行执行（一轮按 highlightBatchSize 逐个分析，不并发）：
// 分析要解一遍码，与录制进程抢 CPU 和磁盘 IO，宁可慢也不要影响录制。
func highlightLoop() {
	// 先把目标目录缓存建起来，否则服务刚起的前两分钟内
	// highlightClaim 无从判断归属，会放行上传流程删源。
	highlightRefreshTargets()

	t := time.NewTicker(highlightScanInterval)
	defer t.Stop()
	for range t.C {
		if !IsRunning() {
			continue
		}
		// 兜底清理放在最前面：它处理的是「正常路径永远碰不到」的文件，
		// 且内部自带间隔控制，不会每轮都 walk。
		highlightSweepExpiredSources()
		highlightPass()
	}
}

// highlightSweepInterval 兜底清理的执行间隔。
// walk 一遍录像根目录有实打实的 IO 开销，没必要每轮（2 分钟）都做。
const highlightSweepInterval = time.Hour

// highlightLastSweep 上次兜底清理时刻；只在 highlightLoop 这一个协程里读写。
var highlightLastSweep time.Time

// highlightSweepExpiredSources 按 highlight_source_retention_days 兜底清理超期源片。
//
// 为什么需要它：正常的两条删除路径（上传完成即删 / 高光分析完即删）都要求
// 「流程摸到过这个文件」，而下面三类文件永远不会被摸到：
//   - 上传反复失败、重试耗尽后被 pipeline 直接 return 的
//   - 高光分析队列排不上队的（分析吞吐低于录制速度时会积压，详见 findStableClips 的正序说明）
//   - 主播已从名单移除后留下的孤儿目录
//
// 它们会一直占盘，直到把磁盘吃满。这里是最后一道闸：**最长保留 N 天**，
// 到点一律放行删除 —— 宁可漏掉一个还没传出去的源片，也不让磁盘被吃满
// （与 highlightClaimTTL 同一取舍）。显式配 0 或负值可关闭本清理。
func highlightSweepExpiredSources() {
	days := AppCfg().Builtin.SourceRetentionDays()
	if days <= 0 {
		return
	}
	now := time.Now()
	if !highlightLastSweep.IsZero() && now.Sub(highlightLastSweep) < highlightSweepInterval {
		return
	}
	highlightLastSweep = now

	removed, freed := highlightSweepRoots(AppCfg().Dirs, now.AddDate(0, 0, -days))
	if removed > 0 {
		log.Printf("[HIGHLIGHT] 🧹 兜底清理：删除 %d 个超过 %d 天的源片，释放 %.1f MB",
			removed, days, float64(freed)/1048576)
	}
}

// highlightSweepRoots 在每个根目录下删除 mtime 早于 cutoff 的切片，
// 返回「删除数量」与「释放字节数」。
//
// 抽成不依赖全局配置的纯函数，是为了能直接单测 —— 这是会真删文件的逻辑。
//
// 目录形状必须与录制落盘完全一致：**<根>/<主播>/<日期>/<切片>**（三层）。
// 注意这里的 root 是平台根（如 …/抖音直播），比 findStableClips 的入参
// （那是单个主播目录）多一层，别写少。
func highlightSweepRoots(roots []string, cutoff time.Time) (int, int64) {
	removed, freed := 0, int64(0)
	for _, root := range roots {
		anchors, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, anchor := range anchors {
			if !anchor.IsDir() {
				continue
			}
			anchorDir := filepath.Join(root, anchor.Name())
			days, derr := os.ReadDir(anchorDir)
			if derr != nil {
				continue
			}
			for _, day := range days {
				if !day.IsDir() {
					continue
				}
				dir := filepath.Join(anchorDir, day.Name())
				entries, rerr := os.ReadDir(dir)
				if rerr != nil {
					continue
				}
				for _, e := range entries {
					// 「高光」是子目录，被 IsDir 挡掉；_highlight 产物被 isClipName 挡掉。
					if e.IsDir() || !isClipName(e.Name()) {
						continue
					}
					info, ierr := e.Info()
					if ierr != nil || !info.ModTime().Before(cutoff) {
						continue
					}
					full := filepath.Join(dir, e.Name())
					size := info.Size()
					if err := os.Remove(full); err != nil {
						continue
					}
					removed++
					freed += size
				}
			}
		}
	}
	return removed, freed
}

// highlightPass 执行一轮扫描。
//
// 顺序刻意如此：先处理被上传流程认领的文件 —— 它们已经上传完、正等着释放磁盘，
// 留得越久越占空间；再按常规扫描补漏。总量受 highlightBatchSize 限制，
// 避免长时间占住调度协程。
func highlightPass() {
	highlightReapStaleClaims()
	highlightRefreshTargets()

	cfg := AppCfg()
	targets := recorder.HighlightTargets(cfg.Builtin.HighlightEnable, cfg.Builtin.HighlightOnlyUpload)
	if len(targets) == 0 {
		return
	}
	opts := highlightOptions(cfg.Builtin)
	onlyPrefixes := recorder.HighlightOnlyPrefixes(cfg.Builtin.HighlightOnlyUpload)

	budget := highlightBatchSize - highlightPassClaimed(opts, highlightBatchSize)

	for _, tgt := range targets {
		if budget <= 0 {
			return
		}
		for _, src := range findStableClips(tgt.SaveDir) {
			if budget <= 0 {
				return
			}
			if !highlightCanRetry(src) {
				// 已有定论但还躺在盘上的。这类文件这辈子不会再被任何删除路径碰到 ——
				// analyzeClip 早就跑过、dispose 也早就做过，而常规扫描每轮又会撞到它。
				// 这里补一刀：「只传高光」的原片没有上传凭证这道闸（本来就不上传），
				// 有定论即清；其余的留给 pipeline 和兜底清理，不在这里耗 IO 查哈希。
				if matchHighlightOnlyPrefix(src, onlyPrefixes, recorder.HighlightDirName) {
					highlightDisposeSource(src)
				}
				continue
			}
			// 产物跟原片放同一个日期目录下（<主播>/<日期>/高光/），
			// 而不是全堆在 <主播>/高光/ —— 后者会把多天的高光混在一起。
			analyzeClip(src, filepath.Join(filepath.Dir(src), recorder.HighlightDirName), opts)
			budget--
		}
	}
}

// highlightRefreshTargets 刷新「开了高光的主播录像目录」前缀缓存。
// 由 highlightLoop 每轮调用；highlightClaim 走缓存是为了避开
// GetBuiltinRecorderTasks 里 walk 每个主播目录统计 FileSize 的开销。
func highlightRefreshTargets() {
	cfg := AppCfg()
	targets := recorder.HighlightTargets(cfg.Builtin.HighlightEnable, cfg.Builtin.HighlightOnlyUpload)
	dirs := make([]string, 0, len(targets))
	for _, tgt := range targets {
		dirs = append(dirs, filepath.ToSlash(filepath.Clean(tgt.SaveDir)))
	}
	highlightTargetDirsMu.Lock()
	highlightTargetDirs = dirs
	highlightTargetDirsMu.Unlock()
}

// highlightClaim 供 uploader.Pipeline.BeforeRemove 调用：判断这个源文件能不能删。
//
// 返回 true = 放行删除；false = 高光接管，pipeline 不要删。
// 只有「落在开了高光的主播目录下、且还没分析过」的源文件才拦下来 ——
// 上传流程在切片封口后一两分钟内就完成转换+上传+删除，而高光的稳定期是 3 分钟，
// 不拦的话高光永远读不到文件（这正是此前 0 产出的原因）。
func highlightClaim(path string) bool {
	if path == "" {
		return true
	}
	// 只认领真正的录像切片。截图（cover_*.png）、转换中的 .part 等也在同一个
	// 主播目录下，但拿去做高光分析只会白跑一次 ffmpeg（"未解析到采样点"），
	// 还会把 pipeline 的正常清理拖到分析结束之后。
	if !isClipName(filepath.Base(path)) {
		return true
	}
	// 已有定论的（已产出 / 未检出 / 失败已到重试上限）没必要再留
	if !highlightCanRetry(path) {
		return true
	}
	if highlightOutDirFor(path) == "" {
		return true
	}
	highlightClaimed.Store(path, time.Now())
	return false
}

// highlightCanRetry 判断某个切片是否还值得再分析一次。
//
// 三种「有定论」的情况都不再重试：已产出高光、明确未检出（整段都很平，重跑结论一样）、
// 失败次数已达上限。只有「从未分析过」和「失败但没到上限」返回 true ——
// 后者是关键：失败往往只是撞上了转换进程在读同一个文件、或磁盘 IO 抖动，重试就能过。
func highlightCanRetry(path string) bool {
	e, ok := highlightStateGet(path)
	if !ok {
		return true // 从未分析过
	}
	if e.Output != "" {
		return false // 已产出
	}
	if e.Err == "" {
		return false // 未检出，是有效结论
	}
	return e.Attempts < maxHighlightAttempts
}

// highlightRecordFailure 记录一次分析失败并累计尝试次数，返回累计次数。
//
// 必须「读旧值 → 改 → 写回」，不能直接覆盖整个 entry ——
// 那样 Attempts 永远是 1，重试就没了上限。
func highlightRecordFailure(src string, err error) int {
	prev, _ := highlightStateGet(src)
	prev.Segments = 0
	prev.Output = ""
	prev.Size = 0
	prev.Err = err.Error()
	prev.Attempts++
	highlightStateSet(src, prev)
	return prev.Attempts
}

// highlightLogFailure 记录失败并打日志，把「还会不会重试」说清楚。
func highlightLogFailure(stage, base, src string, err error) {
	n := highlightRecordFailure(src, err)
	if n >= maxHighlightAttempts {
		log.Printf("[HIGHLIGHT] ❌ %s失败 %s: %v（已试 %d 次，放弃重试）", stage, base, err, n)
		return
	}
	log.Printf("[HIGHLIGHT] ⚠️ %s失败 %s: %v（第 %d 次，稍后重试）", stage, base, err, n)
}

// highlightOutDirFor 判断路径是否落在某个高光目标的录像目录下，
// 是则返回该切片所在日期目录下的高光产物目录，否则返回空串。
func highlightOutDirFor(path string) string {
	p := filepath.ToSlash(filepath.Clean(path))
	highlightTargetDirsMu.RLock()
	dirs := highlightTargetDirs
	highlightTargetDirsMu.RUnlock()
	for _, dir := range dirs {
		if !strings.HasPrefix(p, dir+"/") {
			continue
		}
		// 高光产物自己（…/<日期>/高光/xxx_highlight.mp4）不算原片。
		// 只在主播目录之后的相对路径里找「高光/」，避免主播名本身叫「高光」时误判。
		if strings.Contains(p[len(dir)+1:], recorder.HighlightDirName+"/") {
			return ""
		}
		return filepath.Join(filepath.Dir(path), recorder.HighlightDirName)
	}
	return ""
}

// highlightDisposeSource 分析结束后的源文件处置。
//
// 判定的硬前提只有一个：**已经不需要这份原片了**。见 highlightShouldDeleteSource。
func highlightDisposeSource(src string) {
	_, claimed := highlightClaimed.LoadAndDelete(src)
	// 定论 = 已产出 / 未检出 / 失败已达上限，即 highlightCanRetry 为 false。
	concluded := !highlightCanRetry(src)
	// 「只传高光」主播的原片从不进入上传流程，哈希库里永远不会有它的记录，
	// 所以这类文件只能靠「自己是否在名单里」判定；其余原片必须拿出上传凭证才敢删。
	onlyTarget := matchHighlightOnlyPrefix(src,
		recorder.HighlightOnlyPrefixes(AppCfg().Builtin.HighlightOnlyUpload), recorder.HighlightDirName)
	// 只有走到「未认领、有定论、又不是只传高光」这一步才值得查哈希库 —— FileHash 要通读整个文件。
	uploaded := false
	if !claimed && concluded && !onlyTarget {
		uploaded = highlightSourceUploaded(src)
	}
	if !highlightShouldDeleteSource(claimed, concluded, uploaded, onlyTarget) {
		return
	}
	if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
		log.Printf("[HIGHLIGHT] ⚠️ 清理高光源文件失败 %s: %v", filepath.Base(src), err)
	}
}

// highlightSourceUploaded 判断源文件是否已经成功上传过（秒传哈希库里有记录）。
//
// 这是删源的一道保险：高光的常规扫描会先于上传流程处理到新切片（切片封口后
// 3 分钟就算「稳定」，而 pipeline 可能还在排队），此时删掉原片就永远没得上传了。
// 哈希是上传成功那一刻写进库的，命中即代表网盘已有副本，本地可以不留。
func highlightSourceUploaded(src string) bool {
	h := hashstore.FileHash(src)
	if h == "" || HashDB == nil {
		return false
	}
	return HashDB.Exists(h)
}

// highlightShouldDeleteSource 是删源判定的纯函数形式，便于单测。
//
// 口径（2026-09-23 调整）：不再要求主播必须在「只传高光」名单里才删 ——
// 只要「确定已经不需要留了」就删，本地不留原片：
//   - claimed：pipeline 已上传完成、把它从「上传后删除」流程里摘出来交给高光 → 必删
//   - onlyTarget：「只传高光」主播的原片压根不进上传流程，网盘上没有它的副本，
//     判定依据只能是「分析是否已有定论」，不能苛求上传凭证（否则永远删不掉）
//   - 其余：有定论 + 已上传（网盘确有副本）→ 删
//   - 还会重试 / 没传上去 → 保留，删了就没得传
func highlightShouldDeleteSource(claimed, concluded, uploaded, onlyTarget bool) bool {
	if claimed {
		return true
	}
	if !concluded {
		return false
	}
	return onlyTarget || uploaded
}

// highlightReapStaleClaims 回收超时未被处理的认领文件。
// 认领的前提是高光循环会来处理；若循环停摆，宁可漏一个高光也不要吃满磁盘。
func highlightReapStaleClaims() {
	now := time.Now()
	highlightClaimed.Range(func(k, v interface{}) bool {
		path, ok := k.(string)
		if !ok {
			highlightClaimed.Delete(k)
			return true
		}
		ts, _ := v.(time.Time)
		if now.Sub(ts) < highlightClaimTTL {
			return true
		}
		highlightClaimed.Delete(path)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[HIGHLIGHT] ⚠️ 清理超时认领文件失败 %s: %v", filepath.Base(path), err)
			return true
		}
		log.Printf("[HIGHLIGHT] 🧹 认领超时未处理，已清理 %s", filepath.Base(path))
		return true
	})
}

// highlightPassClaimed 优先分析被 pipeline 认领的文件，返回实际处理数。
// 这些文件上传已结束、写入早已停止，所以不受 highlightStableAge 限制。
func highlightPassClaimed(opts highlight.Options, budget int) int {
	if budget <= 0 {
		return 0
	}
	n := 0
	highlightClaimed.Range(func(k, v interface{}) bool {
		if n >= budget {
			return false
		}
		path, ok := k.(string)
		if !ok {
			highlightClaimed.Delete(k)
			return true
		}
		if _, err := os.Stat(path); err != nil {
			// 文件已不在（别处删了），清掉认领记录即可
			highlightClaimed.Delete(path)
			return true
		}
		if !highlightCanRetry(path) {
			highlightDisposeSource(path)
			return true
		}
		outDir := highlightOutDirFor(path)
		if outDir == "" {
			highlightDisposeSource(path)
			return true
		}
		analyzeClip(path, outDir, opts) // 内部 defer 会释放认领并处置源文件
		n++
		return true
	})
	return n
}

// highlightOptions 把全局配置映射成分析参数。
func highlightOptions(b config.BuiltinSettings) highlight.Options {
	o := highlight.DefaultOptions()
	o.MotionWeight = b.HighlightMotionW
	o.AudioWeight = b.HighlightAudioW
	o.Threshold = b.HighlightThreshold
	o.MinDuration = b.HighlightMinDur
	o.MaxDuration = b.HighlightMaxDur
	o.MaxPerClip = b.HighlightPerClip
	o.MergeGap = b.HighlightMergeGap
	// 平滑窗口：仅 >0 时覆盖，否则沿用 DefaultOptions 的 5（与改动前行为一致）。
	// 调大它会压掉礼物特效/切场景那种几秒的孤立尖峰，但阈值必须同步调高，见 config.go 的注释。
	if b.HighlightSmoothWindow > 0 {
		o.SmoothWindow = b.HighlightSmoothWindow
	}
	return o
}

// findStableClips 列出目录下已写完的录像切片（savePath/主播/日期/*.ts），按修改时间**正序**。
//
// 正序（2026-09-23 起）：最老的切片排最前，先录的先分析。
//
// 此前是倒序，理由是「最新封口的才还没被上传流程删掉，先处理才有机会拿到高光」。
// 那个顾虑其实不成立 —— 本函数是实时列表，文件不在就不会被列出来；而真正的麻烦
// 出在另一头：倒序下每轮都从最新扫起，单轮预算又只有 highlightBatchSize 个，
// 于是只要录制速度 ≥ 分析吞吐，老切片就被新片无限插队、永远轮不到，
// 最终被兜底清理当垃圾删掉（线上实测因此积压 20+ GB 而高光几乎零产出）。
// 正序掐断这条无限积压：先录的先处理，队列不会越拖越长。
func findStableClips(root string) []string {
	days, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	now := time.Now()
	type clip struct {
		path string
		mod  time.Time
	}
	var clips []clip
	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		dir := filepath.Join(root, day.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !isClipName(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.Size() < highlightMinClipSize {
				continue
			}
			if now.Sub(info.ModTime()) < highlightStableAge {
				continue // 可能仍在写入
			}
			clips = append(clips, clip{path: filepath.Join(dir, e.Name()), mod: info.ModTime()})
		}
	}
	// 按 mtime **正序**排列：最老的切片排最前，优先被分析。
	//
	// 2026-09-23 由倒序改为正序。原因：倒序时每一轮都从最新的一片开始扫，而单轮预算
	// 只有 highlightBatchSize 个，于是只要「录制速度 ≥ 分析吞吐」，老切片就会被新片
	// 持续插队、永远轮不到 —— 线上实测因此积压了 20+ GB，那些片子最终被兜底清理
	// 当作垃圾删掉，高光一次都没提取过（分析吞吐约 13 片/小时，而单片要 4.5 分钟）。
	// 正序保证「先录的先分析」，积压不再无限增长。
	//
	// 代价：新切片排在队尾，积压多时高光产出会滞后。这是刻意选的取舍 ——
	// 宁可让高光晚一点出来，也不能让录制文件因为排不上队而被白删。
	sort.Slice(clips, func(i, j int) bool { return clips[i].mod.Before(clips[j].mod) })
	out := make([]string, len(clips))
	for i, c := range clips {
		out[i] = c.path
	}
	return out
}

// isClipName 判定是否为可分析的录像切片。
// 带 _highlight 的是本功能的产物，必须排除，否则会自我递归分析。
func isClipName(name string) bool {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "_highlight") {
		return false
	}
	return strings.HasSuffix(lower, ".ts") || strings.HasSuffix(lower, ".mp4")
}

// highlightOutputPath 生成高光产物路径：<原片所在目录>/高光/<原片名>_highlight.mp4。
//
// 放在原片同一个日期目录下（<主播>/<日期>/高光/）而不是 <主播>/高光/，
// 是为了让产物和原片一一对应，不把多天的高光混在一个目录里；
// 单独开一个「高光」子目录则是为了「只传高光」能按路径干净地做上传过滤。
func highlightOutputPath(outDir, src string) string {
	name := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	return filepath.Join(outDir, name+"_highlight.mp4")
}

// shouldSkipUpload 判定文件是否因「只传高光」而应当跳过上传。
//
// 放在 HandleFile 入口而不是扫描器里，是因为上传任务的来源不止扫描一处
// （录制结束、手动触发都会入队），入口收口才不会漏。
//
// ⚠️ 「跳过上传」有个硬前提：高光分析必须真的在跑。若高光总开关关闭，
// 原片既不会被分析、也进不了上传管线 —— 没有任何一方会删它，只能无限占盘。
// 线上就出现过这种情况：单个主播因此堆积 14GB。总开关关闭时退化为正常上传，
// 交给 pipeline 的「上传成功即删源」兜住。
func shouldSkipUpload(path string) bool {
	if !AppCfg().Builtin.HighlightEnable {
		return false
	}
	prefixes := recorder.HighlightOnlyPrefixes(AppCfg().Builtin.HighlightOnlyUpload)
	return matchHighlightOnlyPrefix(path, prefixes, recorder.HighlightDirName)
}

// matchHighlightOnlyPrefix 是「只传高光」判定的纯函数形式，便于单测。
//
// 规则：文件属于某个「只传高光」的主播目录，且不在其高光产物 / 截图归档目录下 → 跳过。
// 前缀比较统一转正斜杠并补一个分隔符，避免「菜菜很忙」误匹配到「菜菜很忙2」。
func matchHighlightOnlyPrefix(path string, prefixes []string, hlDir string) bool {
	if len(prefixes) == 0 {
		return false
	}
	p := filepath.ToSlash(filepath.Clean(path))
	for _, prefix := range prefixes {
		base := filepath.ToSlash(filepath.Clean(prefix))
		if !strings.HasPrefix(p, base+"/") {
			continue
		}
		rest := p[len(base)+1:]
		// 截图归档（<主播>/<日期>/Screenshots/*.png）不是原片，照常上传 ——
		// 只传高光跳的是原片，不能把截屏功能一起跳没了。
		if strings.Contains(rest, recorder.ScreenshotDirName+"/") {
			return false
		}
		// 高光产物（<主播>/<日期>/高光/xxx）照常上传，其余（原片）一律跳过。
		// 只在主播目录之后的相对路径里找「高光/」—— 产物多了一层日期目录，
		// 所以不能再用 base+"/高光/" 这样的固定前缀判定。
		return !strings.Contains(rest, hlDir+"/")
	}
	return false
}

// analyzeClip 分析单个切片并裁出高光。
// 失败与「未检出」都会记入状态，避免每轮重复重试同一个坏文件。
func analyzeClip(src, outDir string, opts highlight.Options) {
	start := time.Now()
	base := filepath.Base(src)

	// 源文件处置：认领过的（pipeline 已摘出上传流程）必删；「只传高光」主播的
	// 原片在已有定论时删（从不进管线，没人替它删）；详见 highlightDisposeSource。
	defer highlightDisposeSource(src)

	ctx, cancel := context.WithTimeout(context.Background(), highlightTimeout)
	defer cancel()

	ffmpegBin := "ffmpeg"
	if FFmpegPathHook != nil {
		ffmpegBin = FFmpegPathHook()
	}

	series, err := highlight.Probe(ctx, ffmpegBin, src, opts.Threads)
	if err != nil {
		highlightLogFailure("分析", base, src, err)
		return
	}

	segs := highlight.Select(highlight.Score(series, opts), opts)
	if len(segs) == 0 {
		log.Printf("[HIGHLIGHT] ⚪ %s 未检出高光段（整段都很平）", base)
		highlightStateSet(src, highlightEntry{})
		return
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Printf("[HIGHLIGHT] ⚠️ 创建高光目录失败 %s: %v", outDir, err)
		highlightStateSet(src, highlightEntry{Segments: len(segs), Err: err.Error()})
		return
	}
	out := highlightOutputPath(outDir, src)
	if err := highlight.Cut(ctx, ffmpegBin, src, out, segs); err != nil {
		highlightLogFailure("裁切", base, src, err)
		return
	}

	var size int64
	if info, serr := os.Stat(out); serr == nil {
		size = info.Size()
	}
	total := 0
	for _, s := range segs {
		total += s.Duration()
	}
	log.Printf("[HIGHLIGHT] ✨ %s → %s | %d 段 / 共 %s | 耗时 %s",
		base, filepath.Base(out), len(segs), highlightFormatDur(total), time.Since(start).Truncate(time.Second))
	highlightStateSet(src, highlightEntry{Segments: len(segs), Output: filepath.Base(out), Size: size})
}

// highlightStateGet 查询某切片是否已分析过（含失败与未检出，避免反复重试）。
func highlightStateGet(path string) (highlightEntry, bool) {
	highlightStateLoad()
	highlightMu.Lock()
	defer highlightMu.Unlock()
	e, ok := highlightState[path]
	return e, ok
}

// highlightStateSet 记录分析结果并落盘。
func highlightStateSet(path string, e highlightEntry) {
	highlightStateLoad()
	e.AnalyzedAt = time.Now().Format("2006-01-02 15:04:05")

	highlightMu.Lock()
	highlightState[path] = e
	data, err := json.MarshalIndent(highlightState, "", "  ")
	pos := highlightStatePos
	highlightMu.Unlock()

	if err != nil || pos == "" {
		return
	}
	// 临时文件 + 改名：半写会让状态文件损坏，导致全部切片被重新分析一遍。
	tmp := pos + ".tmp"
	if werr := os.WriteFile(tmp, data, 0o644); werr != nil {
		log.Printf("[HIGHLIGHT] ⚠️ 写入高光状态失败: %v", werr)
		return
	}
	if rerr := os.Rename(tmp, pos); rerr != nil {
		log.Printf("[HIGHLIGHT] ⚠️ 提交高光状态失败: %v", rerr)
	}
}

// highlightStateLoad 首次使用时从磁盘载入状态。
func highlightStateLoad() {
	highlightMu.Lock()
	if highlightLoaded {
		highlightMu.Unlock()
		return
	}
	highlightLoaded = true
	if highlightStatePos == "" {
		highlightStatePos = filepath.Join(AppCfg().DataDirPath(), highlightStatusName)
	}
	pos := highlightStatePos
	highlightMu.Unlock()

	data, err := os.ReadFile(pos)
	if err != nil {
		return
	}
	loaded := map[string]highlightEntry{}
	if json.Unmarshal(data, &loaded) != nil {
		return
	}
	highlightMu.Lock()
	for k, v := range loaded {
		highlightState[k] = v
	}
	highlightMu.Unlock()
}

// resetHighlightState 在数据目录变更时重置状态路径与内存缓存。
// 由 ApplyDataDir 调用，避免状态写到旧目录。
func resetHighlightState(dataDir string) {
	highlightMu.Lock()
	defer highlightMu.Unlock()
	highlightStatePos = filepath.Join(dataDir, highlightStatusName)
	highlightState = map[string]highlightEntry{}
	highlightLoaded = false
}

// highlightFormatDur 把秒数格式化为人类可读时长。
func highlightFormatDur(sec int) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
}
