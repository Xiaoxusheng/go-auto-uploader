package app

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"upload/internal/config"
	"upload/internal/hashstore"
	"upload/internal/highlight"
	"upload/internal/pose"
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
	// Attempts 累计失败次数，只在 Err 非空时有意义；达到上限进入冷却期。
	Attempts int `json:"attempts,omitempty"`
	// Rounds 冷却重试已开的轮数（见 highlightRetryCooldown）；达到上限彻底放弃。
	Rounds int `json:"rounds,omitempty"`
	// —— 投稿扩展字段（B 站自动投稿）：裁切成功时补记首段/末段秒数与最高段评分，
	// 供投稿队列渲染标题简介。旧状态文件没有这些字段，反序列化即零值，天然兼容。
	StartSec int     `json:"start_sec,omitempty"`
	EndSec   int     `json:"end_sec,omitempty"`
	Score    float64 `json:"score,omitempty"`
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
// 分析要解一遍码，与录制进程抢 CPU 和磁盘 IO，串行是历史默认（workers=1）；
// 吞吐追不上录制速度时（积压持续增长）可配 highlight_analyze_workers 开多路并行。
func highlightLoop() {
	// 先把目标目录缓存建起来，否则服务刚起的前两分钟内
	// highlightClaim 无从判断归属，会放行上传流程删源。
	highlightRefreshTargets()

	// worker 池：N 路并行分析（config 钳制 1-4）。worker 生命周期与进程一致；
	// 运行中调大并行路数由每轮 highlightEnsureWorkers 补齐，调小需重启进程。
	highlightEnsureWorkers(AppCfg().Builtin.HighlightAnalyzeWorkers)

	t := time.NewTicker(highlightScanInterval)
	defer t.Stop()
	for range t.C {
		if !IsRunning() {
			continue
		}
		// 兜底清理放在最前面：它处理的是「正常路径永远碰不到」的文件，
		// 且内部自带间隔控制，不会每轮都 walk。
		highlightSweepExpiredSources()
		highlightEnsureWorkers(AppCfg().Builtin.HighlightAnalyzeWorkers)
		highlightSchedulePass()
	}
}

// highlightTask 一次分析任务：源片路径 + 产物目录。
type highlightTask struct {
	src    string
	outDir string
}

// highlightTaskCh 调度协程每轮扫描出的任务队列，worker 协程消费。
// 缓冲 64 足够吸收多轮投递；满时调度侧丢弃 —— 目录本身就是积压队列，下轮重扫。
var highlightTaskCh = make(chan highlightTask, 64)

// highlightInFlight 已投递（含排队中）的源片：channel 里的任务最多要等
// batchSize×workers 片分析完才被消费，期间下一轮扫描会再次扫到同一片，
// 不查重就会让两个 worker 分析同一个文件。
var highlightInFlight sync.Map

// highlightWorkerCount 当前存活的 worker 数（只在 highlightLoop 调度协程里增、worker 启动时增）。
var highlightWorkerCount atomic.Int64

// highlightEnsureWorkers 按配置补齐 worker 数（运行中调大并行路数即时生效；
// 只增不减——多余的 worker 在队列为空时阻塞在 channel 上，无 CPU 消耗，调小需重启进程）。
func highlightEnsureWorkers(target int) {
	if target < 1 {
		target = 1
	}
	for {
		cur := highlightWorkerCount.Load()
		if cur >= int64(target) {
			return
		}
		if highlightWorkerCount.CompareAndSwap(cur, cur+1) {
			go highlightWorker()
		}
	}
}

// highlightWorker 消费任务队列逐片分析；暂停时取到的任务直接丢弃（下轮重投）。
func highlightWorker() {
	defer highlightWorkerCount.Add(-1)
	for task := range highlightTaskCh {
		if !IsRunning() {
			highlightInFlight.Delete(task.src)
			continue
		}
		analyzeClip(task.src, task.outDir, highlightOptions(AppCfg().Builtin))
		highlightInFlight.Delete(task.src)
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
// 顺序刻意如此：先投递被上传流程认领的文件 —— 它们已经上传完、正等着释放磁盘，
// 留得越久越占空间；再按常规扫描补漏。每轮最多投递 highlightBatchSize 个新任务
// （N 个 worker 并行消费），投递前查 highlightInFlight 防止同一片被重复分析。
func highlightSchedulePass() {
	hlLastPassMu.Lock()
	hlLastPass = time.Now()
	hlLastPassMu.Unlock()
	highlightReapStaleClaims()
	highlightRefreshTargets()

	cfg := AppCfg()
	targets := recorder.HighlightTargets(cfg.Builtin.HighlightEnable, cfg.Builtin.HighlightOnlyUpload)
	if len(targets) == 0 {
		return
	}
	onlyPrefixes := recorder.HighlightOnlyPrefixes(cfg.Builtin.HighlightOnlyUpload)

	budget := highlightBatchSize
	// 认领片优先入队；已在飞行中（排队/分析中）的占住名额但不再投递。
	highlightClaimed.Range(func(k, v interface{}) bool {
		if budget <= 0 {
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
		if e, ok := highlightStateGet(path); ok && highlightConcluded(e) {
			highlightDisposeSource(path)
			return true
		}
		// 冷却中的失败片：保留认领，等冷却期满（canRetry 恢复 true）再投递
		outDir := highlightOutDirFor(path)
		if outDir == "" {
			highlightDisposeSource(path)
			return true
		}
		if _, busy := highlightInFlight.LoadOrStore(path, true); busy {
			budget--
			return true
		}
		select {
		case highlightTaskCh <- highlightTask{src: path, outDir: outDir}:
			budget--
		default:
			highlightInFlight.Delete(path)
			return false // 队列满，本轮到此为止
		}
		return true
	})

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
			if _, busy := highlightInFlight.LoadOrStore(src, true); busy {
				continue
			}
			// 产物跟原片放同一个日期目录下（<主播>/<日期>/高光/），
			// 而不是全堆在 <主播>/高光/ —— 后者会把多天的高光混在一起。
			select {
			case highlightTaskCh <- highlightTask{src: src, outDir: filepath.Join(filepath.Dir(src), recorder.HighlightDirName)}:
				budget--
			default:
				highlightInFlight.Delete(src)
				return // 队列满，本轮到此为止
			}
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
//
// 判定用「真定论」（highlightConcluded）而不是 highlightCanRetry：
// 失败达上限但还在冷却期的片要留给自动重试，放行删除就没得重了。
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
	// 已有真定论的（已产出 / 未检出 / 重试轮数耗尽）没必要再留
	if e, ok := highlightStateGet(path); ok && highlightConcluded(e) {
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
// 「有定论」的两种情况不再重试：已产出高光、明确未检出（整段都很平，重跑结论一样）。
// 失败片走「冷却重试」：单轮内最多 maxHighlightAttempts 次；失败达上限进入
// highlightRetryCooldown 冷却期，期满自动复活开新一轮（最多 maxHighlightRetryRounds 轮）。
// 只有「从未分析过」「还有剩余次数」「冷却期满待复活」返回 true。
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
	if e.Rounds >= maxHighlightRetryRounds {
		return false // 重试轮数耗尽，彻底放弃
	}
	if e.Attempts < maxHighlightAttempts {
		return true // 本轮还有剩余次数
	}
	return !highlightCooldownActive(e) // 冷却期满 → 复活重试
}

// highlightConcluded 分析的「真定论」：产出、未检出、重试轮数耗尽。
// 与 highlightCanRetry=false 的差异：失败达上限但还在冷却期的片 canRetry=false
// 却不是定论 —— 删源/放行的判定必须用本函数，否则冷却片会被删掉没得重试。
func highlightConcluded(e highlightEntry) bool {
	if e.Output != "" {
		return true
	}
	if e.Err == "" && e.AnalyzedAt != "" {
		return true // 未检出
	}
	return e.Rounds >= maxHighlightRetryRounds
}

// highlightRetryCooldown 失败达上限后的冷却期：磁盘满/IO 抖动这类「时段性」故障
// 恢复后自动重试，而不是一次性判死（线上有一批片因换装重启撞上失败被永久跳过）。
const highlightRetryCooldown = 45 * time.Minute

// maxHighlightRetryRounds 冷却重试的最大轮数（每轮内最多 maxHighlightAttempts 次）。
// 真正损坏的文件每轮最多浪费 3 次快速失败，总上限 9 次后彻底放弃、源片照常处置。
const maxHighlightRetryRounds = 3

// highlightCooldownActive 失败片是否还在冷却期内（以最后一次失败时刻为基准）。
// AnalyzedAt 缺失或解析失败按「冷却中」处理（保守不复活，避免旧数据触发全量重跑）。
func highlightCooldownActive(e highlightEntry) bool {
	last, err := time.ParseInLocation("2006-01-02 15:04:05", e.AnalyzedAt, time.Local)
	if err != nil {
		return true
	}
	return time.Since(last) < highlightRetryCooldown
}

// highlightMaybeRevive 冷却期满时把失败片重置为可重试状态（attempts 清零、轮数+1）。
// 由 analyzeClip 入口调用：能走到这里的必然通过了 highlightCanRetry 的冷却判定。
func highlightMaybeRevive(src string) {
	e, ok := highlightStateGet(src)
	if !ok || e.Output != "" || e.Err == "" {
		return
	}
	if e.Attempts < maxHighlightAttempts || e.Rounds >= maxHighlightRetryRounds {
		return
	}
	if highlightCooldownActive(e) {
		return
	}
	e.Attempts = 0
	e.Rounds++
	highlightStateSet(src, e)
	log.Printf("[HIGHLIGHT] 🔁 %s 冷却期满，自动重试第 %d 轮", filepath.Base(src), e.Rounds)
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
	prev.StartSec = 0
	prev.EndSec = 0
	prev.Score = 0
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
	// 定论 = 已产出 / 未检出 / 重试轮数耗尽（highlightConcluded）。
	// 失败但还在冷却期的片不是定论——留着等自动重试，删了就没得重了。
	e, _ := highlightStateGet(src)
	concluded := highlightConcluded(e)
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
	// 未定论（含「失败达上限但还在冷却期、等自动重试」的片）一律保留——
	// 删了就没得重了。claimed 的失败片同样保留：冷却期满后由常规扫描重新发现并投递。
	if !concluded {
		return false
	}
	// 删除凭证：pipeline 已把它交给高光（claimed）/ 从不上传的只传高光原片 / 网盘已有副本。
	return claimed || onlyTarget || uploaded
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
	// 迟滞：仅 (0,1) 时启用；0 = 纯阈值（历史行为）。
	if b.HighlightExitRatio > 0 && b.HighlightExitRatio < 1 {
		o.ExitRatio = b.HighlightExitRatio
	}
	if b.HighlightMinAC1 > 0 && b.HighlightMinAC1 < 1 {
		o.MinAC1 = b.HighlightMinAC1
	}
	// 姿态语义门：enable=true 才启用；参数已在 config applyDefaults 回落定标区间。
	if g := b.HighlightPoseGate; g != nil && g.Enable {
		o.PoseGate = &highlight.PoseGateParams{
			Enabled:   true,
			DetMin:    g.DetMin,
			VisMin:    g.VisMin,
			FaceMax:   g.FaceMax,
			KeepRatio: g.KeepRatio,
			FPS:       g.FPS,
			DllPath:   g.OnnxDll,
			ModelPath: g.OnnxModel,

			HeadFilterEnable: g.HeadFilterEnable,
			HeadModel:        g.HeadModel,
			HeadFrac:         g.HeadFrac,
		}
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

// —— 高光队列可视化状态（GET /api/v1/highlight/live 的数据源）——
// analyzeClip 在 N 个 worker 协程并行执行，快照由 HTTP 协程并发读，全部经 hlCurMu 串行化。

type hlCurInfo struct {
	stage   string // probe=解码采样+双因子打分 / gate=姿态语义门 / cut=裁切
	started time.Time
}

var hlCurMu sync.Mutex
var hlCurState = map[string]hlCurInfo{} // src → 阶段信息

var hlLastPassMu sync.Mutex
var hlLastPass time.Time

// hlCurrentBegin 标记一段源片开始分析（默认进入 probe 阶段）。
func hlCurrentBegin(src string) {
	hlCurMu.Lock()
	defer hlCurMu.Unlock()
	hlCurState[src] = hlCurInfo{stage: "probe", started: time.Now()}
}

// hlCurrentStage 推进某片的阶段（仅对仍在分析的该片生效）。
func hlCurrentStage(src, stage string) {
	hlCurMu.Lock()
	defer hlCurMu.Unlock()
	if e, ok := hlCurState[src]; ok {
		e.stage = stage
		hlCurState[src] = e
	}
}

// hlCurrentEnd 清除某片的标记（分析结束）。
func hlCurrentEnd(src string) {
	hlCurMu.Lock()
	defer hlCurMu.Unlock()
	delete(hlCurState, src)
}

// analyzeClip 分析单个切片并裁出高光。
// 失败与「未检出」都会记入状态，避免每轮重复重试同一个坏文件。
func analyzeClip(src, outDir string, opts highlight.Options) {
	start := time.Now()
	base := filepath.Base(src)

	// 队列可视化：标记当前片与阶段；退出时先清标记再处置源文件。
	hlCurrentBegin(src)
	highlightMaybeRevive(src)
	defer func() {
		hlCurrentEnd(src)
		// 源文件处置：认领过的（pipeline 已摘出上传流程）必删；「只传高光」主播的
		// 原片在已有定论时删（从不进管线，没人替它删）；详见 highlightDisposeSource。
		highlightDisposeSource(src)
	}()

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

	segs := highlight.SelectWithBlocks(highlight.Score(series, opts), series.Motion, nil, opts)
	if len(segs) == 0 {
		log.Printf("[HIGHLIGHT] ⚪ %s 未检出高光段（整段都很平）", base)
		highlightStateSet(src, highlightEntry{})
		return
	}

	// 姿态语义门（§19）：砍「近景聊天/连麦/无人特效」类误检段。
	// 纯 Go 构建或未启用时 FilterSegments 原样放行；异常时放行全部段（不误杀）。
	if opts.PoseGate != nil && opts.PoseGate.Enabled {
		hlCurrentStage(src, "gate")
		spans := make([][2]int, len(segs))
		for i, sg := range segs {
			spans[i] = [2]int{sg.Start, sg.End}
		}
		kept, dropped, gerr := pose.FilterSegments(ffmpegBin, src, spans, pose.GateOptions{
			DetMin:    opts.PoseGate.DetMin,
			VisMin:    opts.PoseGate.VisMin,
			FaceMax:   opts.PoseGate.FaceMax,
			KeepRatio: opts.PoseGate.KeepRatio,

			HeadEnable:    opts.PoseGate.HeadFilterEnable,
			HeadModelPath: opts.PoseGate.HeadModel,
			HeadFrac:      opts.PoseGate.HeadFrac,
		}, opts.PoseGate.DllPath, opts.PoseGate.ModelPath)
		if gerr != nil {
			log.Printf("[HIGHLIGHT] ⚠️ %s 姿态门异常（放行全部段）: %v", base, gerr)
		} else {
			log.Printf("[HIGHLIGHT] 🧍 %s 姿态门: %d 段 → %d 段（砍 %d）", base, len(segs), len(kept), dropped)
			segs = segs[:0]
			for _, sp := range kept {
				segs = append(segs, highlight.Segment{Start: sp[0], End: sp[1]})
			}
		}
		if len(segs) == 0 {
			log.Printf("[HIGHLIGHT] ⚪ %s 姿态门后无残留段（判定为非舞蹈内容）", base)
			highlightStateSet(src, highlightEntry{})
			return
		}
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Printf("[HIGHLIGHT] ⚠️ 创建高光目录失败 %s: %v", outDir, err)
		highlightStateSet(src, highlightEntry{Segments: len(segs), Err: err.Error()})
		return
	}
	out := highlightOutputPath(outDir, src)
	hlCurrentStage(src, "cut")
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
	top := segs[0]
	for _, s := range segs {
		if s.Score > top.Score {
			top = s
		}
	}
	log.Printf("[HIGHLIGHT] ✨ %s → %s | %d 段 / 共 %s | 耗时 %s",
		base, filepath.Base(out), len(segs), highlightFormatDur(total), time.Since(start).Truncate(time.Second))
	highlightStateSet(src, highlightEntry{
		Segments: len(segs),
		Output:   filepath.Base(out),
		Size:     size,
		StartSec: segs[0].Start,
		EndSec:   segs[len(segs)-1].End,
		Score:    top.Score,
	})
	// 高光产物直接进上传队列，不等下一轮目录扫描（Cut 已收尾，文件是完整的；
	// 系统暂停时 worker 会丢弃任务，文件留在盘上由扫描兜底重新发现）。
	if TaskQueue.Enqueue(out) {
		atomic.AddInt64(&QueueCount, 1)
		log.Printf("[HIGHLIGHT] 📤 %s 已入上传队列", filepath.Base(out))
	}
	// B 站自动投稿暂不接入（2026-09-27）：恢复时调 publishEnqueueHighlight(out, segs)。
}

// highlightStateGet 查询某切片是否已分析过（含失败与未检出，避免反复重试）。
func highlightStateGet(path string) (highlightEntry, bool) {
	highlightStateLoad()
	highlightMu.Lock()
	defer highlightMu.Unlock()
	e, ok := highlightState[path]
	return e, ok
}

// highlightOutputs 列出所有「已产出且仍在盘上」的高光产物绝对路径（B 站投稿候选用）。
func highlightOutputs() []string {
	highlightStateLoad()
	highlightMu.Lock()
	paths := make([]string, 0, len(highlightState))
	for src, e := range highlightState {
		if e.Output == "" {
			continue
		}
		// 产物与源片同日期目录：<主播>/<日期>/高光/<原片名>_highlight.mp4
		paths = append(paths, filepath.Join(filepath.Dir(src), recorder.HighlightDirName, e.Output))
	}
	highlightMu.Unlock()
	existing := make([]string, 0, len(paths))
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			existing = append(existing, p)
		}
	}
	sort.Strings(existing)
	return existing
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

// —— 高光队列快照（控制台「高光判定 · 实时队列」卡的数据源）——

// HighlightQueueItem 队头待分析切片。
type HighlightQueueItem struct {
	Clip     string `json:"clip"`     // 源片文件名
	Streamer string `json:"streamer"` // 主播名（路径倒数第三段）
	AgeMin   int    `json:"age_min"`  // 距写完多少分钟（已过 3 分钟稳定期）
	Claimed  bool   `json:"claimed"`  // 已被上传流程认领，下轮优先分析
}

// HighlightCurrent 正在分析的切片与所处阶段。
type HighlightCurrent struct {
	Clip       string `json:"clip,omitempty"`
	Streamer   string `json:"streamer,omitempty"`
	Stage      string `json:"stage,omitempty"` // probe | gate | cut
	ElapsedSec int    `json:"elapsed_sec,omitempty"`
}

// HighlightDoneItem 最近一个有定论的切片（产出 / 未检出 / 失败）。
type HighlightDoneItem struct {
	Clip       string  `json:"clip"`
	Streamer   string  `json:"streamer"`
	Segments   int     `json:"segments"`
	Output     string  `json:"output,omitempty"` // 高光产物文件名；空 = 未产出
	SizeMB     float64 `json:"size_mb,omitempty"`
	Err        string  `json:"err,omitempty"`
	AnalyzedAt string  `json:"analyzed_at"`
}

// HighlightToday 当日判定统计（口径：highlight_status.json 里 AnalyzedAt 是今天的条目）。
type HighlightToday struct {
	Analyzed   int `json:"analyzed"`   // 已分析片数（含未检出与失败）
	Highlights int `json:"highlights"` // 产出高光的片数
	Segments   int `json:"segments"`   // 产出的高光段总数
	Failed     int `json:"failed"`     // 当前状态为失败的片数
}

// HighlightQueueSnapshot 一次快照的全部字段。
type HighlightQueueSnapshot struct {
	Enabled         bool                 `json:"enabled"`          // 高光总开关
	Running         bool                 `json:"running"`          // 系统运行中（暂停时不分析）
	GateEnabled     bool                 `json:"gate_enabled"`     // 姿态语义门是否启用
	Pending         int                  `json:"pending"`          // 待分析积压（含认领优先片）
	QueueHead       []HighlightQueueItem `json:"queue_head"`       // 队头预览（认领片在最前）
	Current         HighlightCurrent     `json:"current"`          // 正在分析（最早开始的一片）
	Currents        []HighlightCurrent   `json:"currents"`         // 全部正在分析的片（并行时 >1 条）
	Today           HighlightToday       `json:"today"`            // 今日统计
	RecentDone      []HighlightDoneItem  `json:"recent_done"`      // 最近判定（新→旧）
	Workers         int                  `json:"workers"`          // 并行路数
	LastPass        string               `json:"last_pass"`        // 上轮扫描时刻（HH:MM，跨天带日期）
	ScanIntervalMin int                  `json:"scan_interval_min"`
}

// highlightSnapTTL 快照缓存时长：队头要 walk 全部目标目录，
// 前端 4s 级轮询不能每次都打目录 IO；10s 陈旧度对展示无感。
const highlightSnapTTL = 10 * time.Second

var hlSnapMu sync.Mutex
var hlSnapCache struct {
	at   time.Time
	snap HighlightQueueSnapshot
}

// HighlightQueueSnapshot 返回当前高光分析队列快照（带 10s 缓存）。
func HighlightLiveStatus() HighlightQueueSnapshot {
	hlSnapMu.Lock()
	defer hlSnapMu.Unlock()
	if !hlSnapCache.at.IsZero() && time.Since(hlSnapCache.at) < highlightSnapTTL {
		return hlSnapCache.snap
	}
	snap := highlightQueueSnapshotCompute()
	hlSnapCache.at = time.Now()
	hlSnapCache.snap = snap
	return snap
}

// highlightQueueSnapshotCompute 真正算一份快照。
// 待分析队列与 highlightPass 的消费口径完全一致：认领片优先，其余按
// findStableClips（mtime 正序，先录先分析）——页面看到的顺序就是处理顺序。
func highlightQueueSnapshotCompute() HighlightQueueSnapshot {
	cfg := AppCfg()
	snap := HighlightQueueSnapshot{
		Enabled:         cfg.Builtin.HighlightEnable,
		Running:         IsRunning(),
		GateEnabled:     cfg.Builtin.HighlightPoseGate != nil && cfg.Builtin.HighlightPoseGate.Enable,
		ScanIntervalMin: int(highlightScanInterval / time.Minute),
		Workers:         cfg.Builtin.HighlightAnalyzeWorkers,
		QueueHead:       []HighlightQueueItem{},
		Currents:        []HighlightCurrent{},
		RecentDone:      []HighlightDoneItem{},
	}
	if snap.Enabled {
		snap.Pending, snap.QueueHead = highlightPendingList(highlightTargetDirsSnapshot(), 6)
	}
	snap.Currents = highlightCurrentsSnapshot()
	snap.Current = highlightCurrentSnapshot()
	state := highlightStateCopy()
	snap.Today = highlightTodayStats(state, time.Now())
	snap.RecentDone = highlightRecentDone(state, 6)

	hlLastPassMu.Lock()
	last := hlLastPass
	hlLastPassMu.Unlock()
	if !last.IsZero() {
		if last.Format("2006-01-02") == time.Now().Format("2006-01-02") {
			snap.LastPass = last.Format("15:04")
		} else {
			snap.LastPass = last.Format("01-02 15:04")
		}
	}
	return snap
}

// highlightCurrentSnapshot 取最早开始分析的那片（兼容单槽字段，无则零值结构）。
func highlightCurrentSnapshot() HighlightCurrent {
	items := highlightCurrentsSnapshot()
	if len(items) == 0 {
		return HighlightCurrent{}
	}
	return items[0]
}

// highlightCurrentsSnapshot 列出全部正在分析的片（started 升序，多路并行时 >1 条）。
func highlightCurrentsSnapshot() []HighlightCurrent {
	hlCurMu.Lock()
	items := make([]hlCurInfo, 0, len(hlCurState))
	srcs := make([]string, 0, len(hlCurState))
	for src, e := range hlCurState {
		srcs = append(srcs, src)
		items = append(items, e)
	}
	hlCurMu.Unlock()

	out := make([]HighlightCurrent, 0, len(srcs))
	now := time.Now()
	for i, src := range srcs {
		elapsed := 0
		if !items[i].started.IsZero() {
			elapsed = int(now.Sub(items[i].started).Seconds())
		}
		out = append(out, HighlightCurrent{
			Clip:       filepath.Base(src),
			Streamer:   highlightStreamerOf(src),
			Stage:      items[i].stage,
			ElapsedSec: elapsed,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ElapsedSec > out[j].ElapsedSec })
	return out
}

// highlightTargetDirsSnapshot 取「开了高光的主播录像目录」缓存；服务刚起还没建缓存时补建一次
// （highlightRefreshTargets 自带加锁，开销大但仅此一次，之后走缓存）。
func highlightTargetDirsSnapshot() []string {
	highlightTargetDirsMu.RLock()
	dirs := highlightTargetDirs
	highlightTargetDirsMu.RUnlock()
	if len(dirs) > 0 {
		return dirs
	}
	highlightRefreshTargets()
	highlightTargetDirsMu.RLock()
	dirs = highlightTargetDirs
	highlightTargetDirsMu.RUnlock()
	return dirs
}

// highlightPendingList 按消费口径列出待分析队列：认领片（上传完等分析）排最前，
// 其余为各目标目录里「已写完且还有分析价值」的切片（mtime 正序）。
// 返回积压总数与前 headCap 个队头预览。抽成不依赖目录缓存的函数便于单测。
func highlightPendingList(dirs []string, headCap int) (int, []HighlightQueueItem) {
	head := make([]HighlightQueueItem, 0, headCap)
	addHead := func(item HighlightQueueItem) {
		if len(head) < headCap {
			head = append(head, item)
		}
	}
	// 认领片：上传流程已处理完、专门留给高光分析，调度轮次里它们先入队。
	claimedSet := map[string]bool{}
	type claimedItem struct {
		path string
		at   time.Time
	}
	var claimed []claimedItem
	highlightClaimed.Range(func(k, v interface{}) bool {
		path, ok := k.(string)
		if !ok {
			return true
		}
		if _, err := os.Stat(path); err != nil {
			return true // 已不在盘上，循环自己会清
		}
		if !highlightCanRetry(path) {
			return true
		}
		ts, _ := v.(time.Time)
		claimed = append(claimed, claimedItem{path, ts})
		return true
	})
	sort.Slice(claimed, func(i, j int) bool { return claimed[i].at.Before(claimed[j].at) })
	pending := len(claimed)
	for _, c := range claimed {
		claimedSet[c.path] = true
		addHead(newHighlightQueueItem(c.path, true))
	}
	// 常规队列：与 highlightPass 相同的口径（稳定 + 可重试），mtime 正序。
	for _, dir := range dirs {
		for _, src := range findStableClips(dir) {
			if claimedSet[src] {
				continue
			}
			if !highlightCanRetry(src) {
				continue
			}
			pending++
			addHead(newHighlightQueueItem(src, false))
		}
	}
	return pending, head
}

// newHighlightQueueItem 由源片路径构造队头条目（age 取 mtime，仅队头几片会 stat）。
func newHighlightQueueItem(src string, claimed bool) HighlightQueueItem {
	age := 0
	if info, err := os.Stat(src); err == nil {
		age = int(time.Since(info.ModTime()).Minutes())
	}
	return HighlightQueueItem{
		Clip:     filepath.Base(src),
		Streamer: highlightStreamerOf(src),
		AgeMin:   age,
		Claimed:  claimed,
	}
}

// highlightStateCopy 复制一份分析状态（今日统计与最近判定在锁外算，避免占着锁遍历）。
func highlightStateCopy() map[string]highlightEntry {
	highlightStateLoad()
	highlightMu.Lock()
	defer highlightMu.Unlock()
	out := make(map[string]highlightEntry, len(highlightState))
	for k, v := range highlightState {
		out[k] = v
	}
	return out
}

// highlightTodayStats 统计今天的判定结论（纯函数，便于单测）。
func highlightTodayStats(state map[string]highlightEntry, now time.Time) HighlightToday {
	today := now.Format("2006-01-02")
	var st HighlightToday
	for _, e := range state {
		if !strings.HasPrefix(e.AnalyzedAt, today) {
			continue
		}
		st.Analyzed++
		if e.Err != "" && e.Output == "" {
			st.Failed++
		}
		if e.Output != "" {
			st.Highlights++
			st.Segments += e.Segments
		}
	}
	return st
}

// highlightRecentDone 取最近 n 个有定论的切片，按分析时间倒序（纯函数，便于单测）。
// AnalyzedAt 是固定格式 "2006-01-02 15:04:05"，字符串比较即时间比较。
func highlightRecentDone(state map[string]highlightEntry, n int) []HighlightDoneItem {
	type kv struct {
		path string
		e    highlightEntry
	}
	items := make([]kv, 0, len(state))
	for p, e := range state {
		if e.AnalyzedAt == "" {
			continue
		}
		items = append(items, kv{p, e})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].e.AnalyzedAt > items[j].e.AnalyzedAt })
	if len(items) > n {
		items = items[:n]
	}
	out := make([]HighlightDoneItem, 0, len(items))
	for _, it := range items {
		out = append(out, HighlightDoneItem{
			Clip:       filepath.Base(it.path),
			Streamer:   highlightStreamerOf(it.path),
			Segments:   it.e.Segments,
			Output:     it.e.Output,
			SizeMB:     float64(it.e.Size) / 1048576,
			Err:        it.e.Err,
			AnalyzedAt: it.e.AnalyzedAt,
		})
	}
	return out
}

// highlightStreamerOf 从源片绝对路径（…/<主播>/<日期>/<片>）取主播名。
// 目录形状见 findStableClips：主播目录下直接是日期目录。
func highlightStreamerOf(src string) string {
	streamer := filepath.Base(filepath.Dir(filepath.Dir(src)))
	if streamer == "" || streamer == "." || streamer == "/" || streamer == string(filepath.Separator) {
		return ""
	}
	return streamer
}

// —— 源片缩略图（高光判定卡的传送带小车 / 最近判定条用）——

// highlightThumbDirName 缩略图缓存目录名（位于 dataDir 下）。
const highlightThumbDirName = "hlq_thumbs"

// highlightThumbMu 缩略图抽取串行化：ffmpeg 抽帧是秒级开销，
// 并发请求同一片时第二方命中前方的缓存即可，不值得为它建 per-clip 锁。
var highlightThumbMu sync.Mutex

// HighlightThumb 返回源片缩略图（jpg 路径）：在开启高光的主播目录下查找同名源片，
// 抽取距开头 60s 处的一帧（失败回退第 1 秒）。命中磁盘缓存直接返回——
// 缓存命中在源片存在性检查之前，源片已删的片仍能出图（缓存即历史快照）。
func HighlightThumb(clip string) (string, error) {
	if clip == "" {
		return "", os.ErrNotExist
	}
	highlightThumbMu.Lock()
	defer highlightThumbMu.Unlock()

	// 查源片：固定两层（<主播目录>/<日期目录>/<片名>），与 findStableClips 的目录形状一致。
	var src string
	for _, dir := range highlightTargetDirsSnapshot() {
		days, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		found := false
		for _, day := range days {
			if !day.IsDir() {
				continue
			}
			p := filepath.Join(dir, day.Name(), clip)
			if info, serr := os.Stat(p); serr == nil && !info.IsDir() {
				src = p
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if src == "" {
		return "", os.ErrNotExist
	}

	dir := filepath.Join(AppCfg().DataDirPath(), highlightThumbDirName)
	pos := filepath.Join(dir, fmt.Sprintf("%x.jpg", sha1.Sum([]byte(src))))
	if _, err := os.Stat(pos); err == nil {
		return pos, nil // 缓存命中
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	ff := "ffmpeg"
	if FFmpegPathHook != nil {
		ff = FFmpegPathHook()
	}
	// 先取 60s 处（躲开开头可能的加载黑屏/转场）；片长不足 60s 时回退第 1 秒。
	tmp := pos + ".part"
	err := publishExtractFrame(ff, src, 60, tmp)
	if err != nil {
		os.Remove(tmp)
		tmp = pos + ".part"
		err = publishExtractFrame(ff, src, 1, tmp)
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if rerr := os.Rename(tmp, pos); rerr != nil {
		os.Remove(tmp)
		return "", rerr
	}
	return pos, nil
}
