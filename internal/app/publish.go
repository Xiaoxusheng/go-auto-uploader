package app

// B 站自动投稿：落盘队列 + 限速 worker。
//
// 数据流（入口暂停，2026-09-27：analyzeClip 不再调用 publishEnqueueHighlight，
// 高光产物直接进上传队列；恢复接入即恢复调用）：publishEnqueueHighlight 入队
// （ID=产物绝对路径，天然去重）→ publishLoop 每 30s 醒来一次，按「最小间隔 + 每日上限」
// 取出一条 → 封面截帧 → bilibili.Client 上传视频 → add/v3 提交 → 记 aid/bvid。
//
// 可靠性口径：
//   - 队列整体落盘（dataDir/bili_publish.json），原子写（tmp+rename），崩溃后恢复；
//   - 进程崩溃时处于 uploading 的任务视为未完成，重启后回 pending 重投（B 站侧
//     只多一版未提交的分片，不产生脏稿件）；
//   - 失败退避重试（间隔随尝试次数翻倍），耗尽次数转 failed 终态；
//   - 失效清扫：源文件已被删除（手动清理高光等）的 pending/failed 任务自动出队，
//     启动时、每轮 tick、控制台手动「清理失效」三个入口共用同一把扫帚；
//   - 限速是全自动投稿的安全阀：B 站网页投稿接口有频控（code 601）。

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"upload/internal/bilibili"
	"upload/internal/config"
	"upload/internal/highlight"
	"upload/internal/procutil"
)

const (
	publishStatusName    = "bili_publish.json"
	publishTickInterval  = 30 * time.Second
	publishUploadTimeout = 2 * time.Hour
	// publishRetryBase 失败退避基数：第 n 次重试等 n*10 分钟。
	publishRetryBase = 10 * time.Minute
	// publishQueueMax 队列条目上限（含已完成）。超出先丢最老的已完结条目，
	// 没有可丢的才不裁——pending 丢不得。
	publishQueueMax = 500
	// publishCoverAtPct 封面截帧点：取视频时长的 30% 处。
	publishCoverAtPct = 0.3
)

// 投稿任务状态机：pending → uploading → done / failed（failed 可被手动重置回 pending）。
const (
	publishPending   = "pending"
	publishUploading = "uploading"
	publishDone      = "done"
	publishFailed    = "failed"
)

// publishJob 一个待投稿的高光。
type publishJob struct {
	ID       string  `json:"id"` // = 产物文件绝对路径
	File     string  `json:"file"`
	Streamer string  `json:"streamer"`
	ClipDate string  `json:"clip_date"` // 录制日期（YYYY-MM-DD，取自切片文件名）
	ClipTime string  `json:"clip_time"` // 录制时刻（HH-MM-SS，取自切片文件名）
	Segments int     `json:"segments"`
	StartSec int     `json:"start_sec,omitempty"`
	EndSec   int     `json:"end_sec,omitempty"`
	Duration int     `json:"duration,omitempty"` // EndSec-StartSec
	Score    float64 `json:"score,omitempty"`
	FileSize int64   `json:"file_size,omitempty"`

	Status string    `json:"status"`
	Tries  int       `json:"tries"`
	NextAt time.Time `json:"next_at,omitempty"` // 退避重试的最早尝试时刻
	Err    string    `json:"err,omitempty"`

	DoneAt    time.Time `json:"done_at,omitempty"`
	AID       int64     `json:"aid,omitempty"`
	BvID      string    `json:"bvid,omitempty"`
	Title     string    `json:"title,omitempty"`
	CoverURL  string    `json:"cover_url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// CoverAt 用户自选的封面截帧时间点（秒，相对本高光片段）；0 = 默认取 30% 处。
	CoverAt int `json:"cover_at,omitempty"`
}

var (
	publishMu     sync.Mutex
	publishQueue  []*publishJob
	publishLoaded bool
	publishPos    string

	// publishLastSubmit 上次成功提交时刻（worker 与 API 线程都可能读，锁内访问）。
	publishLastSubmit time.Time
	// publishLastWarn 空配置警告的节流戳。
	publishLastWarn int64
	// publishTriggerCh 手动操作（重试/入队）叫醒 worker 的信号，容量 1。
	publishTriggerCh = make(chan struct{}, 1)
)

// publishTrigger 非阻塞叫醒 worker，让它跳过剩余等待立刻跑一轮。
func publishTrigger() {
	select {
	case publishTriggerCh <- struct{}{}:
	default:
	}
}

// publishEnqueueHighlight 把一份裁切成功的高光加入投稿队列。
// 由 analyzeClip 在 Cut 成功后调用；总开关关闭或重复产物直接忽略。
func publishEnqueueHighlight(outPath string, segs []highlight.Segment) {
	cfg := AppCfg().Bilibili
	if !cfg.Enable {
		return
	}
	abs, err := filepath.Abs(outPath)
	if err != nil {
		abs = outPath
	}
	job := buildPublishJob(abs, segs)
	if job == nil {
		return
	}

	publishMu.Lock()
	defer publishMu.Unlock()
	publishLoadLocked()
	for _, j := range publishQueue {
		if j.ID == job.ID {
			return // 已在队列（理论上不会发生：ID 是去重产物路径）
		}
	}
	publishQueue = append(publishQueue, job)
	publishTrimLocked()
	publishSaveLocked()
	log.Printf("[BILI] 📥 高光进入投稿队列: %s（%s · %d 段）", job.Streamer, filepath.Base(job.File), job.Segments)
	BroadcastWS("biliQueue", publishStatsLocked())
}

// buildPublishJob 从产物路径解析投稿元数据。
//
// 产物路径形状：<根>/<主播>/<日期>/高光/<主播>_<2006-01-02_15-04-05>_NNN_highlight.mp4。
// 主播名可能含下划线，所以日期/时刻只从文件名**右侧**解析；
// 解析不出时间信息时仍入队（标题模板退化为无时间占位）。
func buildPublishJob(absPath string, segs []highlight.Segment) *publishJob {
	base := strings.TrimSuffix(filepath.Base(absPath), ".mp4")
	if !strings.HasSuffix(base, "_highlight") {
		return nil
	}
	rest := strings.TrimSuffix(base, "_highlight")
	job := &publishJob{
		ID:        absPath,
		File:      absPath,
		Status:    publishPending,
		CreatedAt: time.Now(),
		Segments:  len(segs),
	}
	// 右侧三段固定：…_YYYY-MM-DD_HH-MM-SS_NNN
	parts := strings.Split(rest, "_")
	if len(parts) >= 4 {
		date, timePart := parts[len(parts)-3], parts[len(parts)-2]
		if _, err := time.ParseInLocation("2006-01-02", date, time.Local); err == nil {
			job.ClipDate = date
		}
		if _, err := time.ParseInLocation("15-04-05", timePart, time.Local); err == nil {
			job.ClipTime = timePart
		}
	}
	// 主播名取目录（与录制落盘的清洗名一致，且不受文件名里的下划线干扰）。
	// 注意层级：absPath 的直接父目录是「高光」，往上依次是日期目录、主播目录。
	outDir := filepath.Dir(absPath)     // <主播>/<日期>/高光
	dayDir := filepath.Dir(outDir)      // <主播>/<日期>
	streamerDir := filepath.Dir(dayDir) // <主播>
	job.Streamer = filepath.Base(streamerDir)
	// 兜底：目录层级不合规（直接丢在根目录的产物）时退化为占位名。
	if job.Streamer == "." || job.Streamer == string(filepath.Separator) || job.Streamer == string(filepath.Separator)+string(filepath.Separator) {
		job.Streamer = "未知主播"
	}
	if len(segs) > 0 {
		job.StartSec = segs[0].Start
		job.EndSec = segs[len(segs)-1].End
		job.Duration = job.EndSec - job.StartSec
		top := segs[0]
		for _, s := range segs {
			if s.Score > top.Score {
				top = s
			}
		}
		job.Score = top.Score
	}
	if info, err := os.Stat(absPath); err == nil {
		job.FileSize = info.Size()
	}
	return job
}

// publishLoop 投稿 worker：低频醒来，串行处理；手动操作可立即叫醒。
func publishLoop() {
	t := time.NewTicker(publishTickInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-publishTriggerCh:
		}
		publishTick()
	}
}

// publishTick 处理一轮：清扫失效 → 取一条 → 上传投稿 → 落盘。
func publishTick() {
	publishMu.Lock()
	if n := publishSweepMissingLocked(); n > 0 {
		log.Printf("[BILI] 🧹 清扫失效投稿任务 %d 条（源文件已删除）", n)
		BroadcastWS("biliQueue", publishStatsLocked())
	}
	publishMu.Unlock()

	cfg := AppCfg().Bilibili
	if !cfg.Enable {
		return
	}
	if cfg.SessData == "" || cfg.BiliJct == "" {
		publishWarnMissingCookie()
		return
	}
	job := publishNextJob(cfg)
	if job == nil {
		return
	}
	publishProcess(job, cfg)
}

// publishNextJob 取出下一条可处理的任务（加锁内标记 uploading 并落盘）。
// 闸门顺序：退避时刻 → 最小间隔 → 每日上限。
func publishNextJob(cfg config.BilibiliSettings) *publishJob {
	now := time.Now()
	publishMu.Lock()
	defer publishMu.Unlock()
	publishLoadLocked()

	if now.Sub(publishLastSubmit) < time.Duration(cfg.MinIntervalMinutes)*time.Minute {
		return nil
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	today := 0
	for _, j := range publishQueue {
		if j.Status == publishDone && j.DoneAt.After(dayStart) {
			today++
		}
	}
	if today >= cfg.DailyLimit {
		return nil
	}
	for _, j := range publishQueue {
		if j.Status != publishPending {
			continue
		}
		if !j.NextAt.IsZero() && j.NextAt.After(now) {
			continue
		}
		j.Status = publishUploading
		publishSaveLocked()
		return j
	}
	return nil
}

// publishProcess 执行单个投稿任务（封面 → 视频 → 提交）。
func publishProcess(job *publishJob, cfg config.BilibiliSettings) {
	log.Printf("[BILI] 🚀 开始投稿 %s（第 %d 次尝试）", filepath.Base(job.File), job.Tries+1)
	ctx, cancel := context.WithTimeout(context.Background(), publishUploadTimeout)
	defer cancel()

	client := bilibili.NewClient(cfg.SessData, cfg.BiliJct, cfg.DedeUserID)

	// 封面：失败不阻断（B 站允许无封面投稿，顶多展示默认图）。
	// 注意不直接写 job 字段——上传期间任务对 API 只读快照可见，写字段必须持锁。
	coverURL := ""
	if cfg.CoverEnabled() {
		coverURL = publishTryCover(ctx, client, job)
	}

	pct := -1
	res, err := client.UploadVideo(ctx, job.File, func(done, total int64) {
		if total <= 0 {
			return
		}
		cur := int(done * 100 / total)
		if cur/25 > pct/25 {
			log.Printf("[BILI] ⬆️ %s 上传进度 %d%%", filepath.Base(job.File), cur)
		}
		pct = cur
	})
	if err != nil {
		publishFail(job, cfg, fmt.Errorf("视频上传: %w", err))
		return
	}

	title := publishRenderTemplate(cfg.TitleTemplate, job)
	desc := publishRenderTemplate(cfg.Desc, job)
	source := publishRenderTemplate(cfg.Source, job)
	dynamic := publishRenderTemplate(cfg.Dynamic, job)
	aid, bvid, err := client.SubmitArchive(ctx, bilibili.ArchiveParams{
		Title:         title,
		Desc:          desc,
		Tag:           cfg.Tag,
		Cover:         coverURL,
		Tid:           cfg.Tid,
		Copyright:     cfg.Copyright,
		Source:        source,
		Dynamic:       dynamic,
		NoReprint:     cfg.NoReprint,
		BizID:         res.BizID,
		VideoFilename: res.Filename,
	})
	if err != nil {
		publishFail(job, cfg, fmt.Errorf("提交稿件: %w", err))
		return
	}

	publishMu.Lock()
	job.Status = publishDone
	job.DoneAt = time.Now()
	job.AID = aid
	job.BvID = bvid
	job.Title = title
	job.CoverURL = coverURL
	job.Err = ""
	publishLastSubmit = time.Now()
	publishSaveLocked()
	stats := publishStatsLocked()
	publishMu.Unlock()

	AddLog("info", fmt.Sprintf("B站投稿成功：%s（%s）", title, bvid), "")
	log.Printf("[BILI] ✅ 投稿成功 %s → %s（aid=%d）", filepath.Base(job.File), bvid, aid)
	BroadcastWS("biliQueue", stats)
}

// publishExtractFrame 用 ffmpeg 抽取视频在 sec 秒处的单帧到 outPath（jpg）。
func publishExtractFrame(ffmpegBin, video string, sec int, outPath string) error {
	cmd := exec.Command(ffmpegBin,
		"-hide_banner", "-nostdin", "-y",
		"-ss", strconv.Itoa(sec),
		"-i", video,
		"-frames:v", "1", "-q:v", "3",
		"-f", "image2", outPath,
	)
	procutil.HideWindow(cmd)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v | %s", err, tailStr(stderr.String(), 200))
	}
	if info, err := os.Stat(outPath); err != nil || info.Size() == 0 {
		return fmt.Errorf("抽帧输出为空（时间点可能超出视频长度）")
	}
	return nil
}

// publishPosterDirName 预览帧缓存目录名（位于 dataDir 下）。
const publishPosterDirName = "bili_posters"

// publishPoster 提取视频在 sec 秒处的预览帧，返回缓存 jpg 路径。
// 命中缓存直接返回；时间点超出视频长度时回退到第 1 秒重抽一次。
func publishPoster(video string, sec int) (string, error) {
	if info, err := os.Stat(video); err != nil || info.IsDir() {
		return "", fmt.Errorf("视频文件不存在: %s", video)
	}
	ext := strings.ToLower(filepath.Ext(video))
	if ext != ".mp4" && ext != ".ts" {
		return "", fmt.Errorf("仅支持 mp4/ts 的预览")
	}
	if sec < 0 {
		sec = 0
	}

	ff := "ffmpeg"
	if FFmpegPathHook != nil {
		ff = FFmpegPathHook()
	}

	dir := filepath.Join(AppCfg().DataDirPath(), publishPosterDirName)
	key := fmt.Sprintf("%x_%d.jpg", sha1.Sum([]byte(video)), sec)
	pos := filepath.Join(dir, key)
	if _, err := os.Stat(pos); err == nil {
		return pos, nil // 缓存命中
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := pos + ".part"
	err := publishExtractFrame(ff, video, sec, tmp)
	if err != nil && sec > 1 {
		os.Remove(tmp)
		sec = 1
		err = publishExtractFrame(ff, video, sec, tmp)
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

// publishTryCover 截帧并上传封面，任何失败返回空串（不阻断主流程）。
// 截帧点：用户自选 job.CoverAt（秒），未选时取视频时长 30% 处。
func publishTryCover(ctx context.Context, client *bilibili.Client, job *publishJob) string {
	ff := "ffmpeg"
	if FFmpegPathHook != nil {
		ff = FFmpegPathHook()
	}
	frameAt := job.CoverAt
	if frameAt <= 0 {
		frameAt = int(float64(job.Duration) * publishCoverAtPct)
	}
	if frameAt < 1 {
		frameAt = 1
	}
	if job.Duration > 0 && frameAt > job.Duration-1 {
		frameAt = job.Duration - 1
	}
	tmpDir, err := os.MkdirTemp("", "bili-cover-")
	if err != nil {
		log.Printf("[BILI] ⚠️ 封面截帧失败（无临时目录）: %v", err)
		return ""
	}
	defer os.RemoveAll(tmpDir)
	cover := filepath.Join(tmpDir, "cover.jpg")
	if err := publishExtractFrame(ff, job.File, frameAt, cover); err != nil {
		log.Printf("[BILI] ⚠️ 封面截帧失败 %s: %v", filepath.Base(job.File), err)
		return ""
	}
	url, err := client.UploadCover(ctx, cover)
	if err != nil {
		log.Printf("[BILI] ⚠️ 封面上传失败 %s: %v", filepath.Base(job.File), err)
		return ""
	}
	return url
}

// publishFail 记录一次失败：还有余量则退避重试（pending），否则转 failed 终态。
func publishFail(job *publishJob, cfg config.BilibiliSettings, err error) {
	publishMu.Lock()
	defer publishMu.Unlock()
	job.Tries++
	job.Err = err.Error()
	if job.Tries >= cfg.MaxRetry {
		job.Status = publishFailed
		log.Printf("[BILI] ❌ 投稿失败 %s（已试 %d 次，放弃）: %v", filepath.Base(job.File), job.Tries, err)
	} else {
		job.Status = publishPending
		job.NextAt = time.Now().Add(time.Duration(job.Tries) * publishRetryBase)
		log.Printf("[BILI] ⚠️ 投稿失败 %s（第 %d 次，%s 后重试）: %v",
			filepath.Base(job.File), job.Tries, job.NextAt.Format("15:04:05"), err)
	}
	publishSaveLocked()
	BroadcastWS("biliQueue", publishStatsLocked())
}

// ---------- 模板渲染 ----------

// publishRenderTemplate 渲染标题/简介/来源/动态模板。
// 占位符：{name}(={streamer}) {date} {time} {segments} {duration} {start} {end} {score}。
func publishRenderTemplate(tpl string, job *publishJob) string {
	if tpl == "" {
		return ""
	}
	return strings.TrimSpace(strings.NewReplacer(
		"{name}", job.Streamer,
		"{streamer}", job.Streamer,
		"{date}", job.ClipDate,
		"{time}", job.ClipTime,
		"{segments}", strconv.Itoa(job.Segments),
		"{duration}", highlightFormatDur(job.Duration),
		"{start}", strconv.Itoa(job.StartSec),
		"{end}", strconv.Itoa(job.EndSec),
		"{score}", strconv.FormatFloat(job.Score, 'f', 2, 64),
	).Replace(tpl))
}

// ---------- 队列持久化与快照 ----------

// publishSaveLocked 原子落盘（调用方持锁）。
func publishSaveLocked() {
	if publishPos == "" {
		return
	}
	data, err := json.MarshalIndent(publishQueue, "", "  ")
	if err != nil {
		return
	}
	tmp := publishPos + ".tmp"
	if werr := os.WriteFile(tmp, data, 0o644); werr != nil {
		log.Printf("[BILI] ⚠️ 写入投稿队列失败: %v", werr)
		return
	}
	if rerr := os.Rename(tmp, publishPos); rerr != nil {
		log.Printf("[BILI] ⚠️ 提交投稿队列失败: %v", rerr)
	}
}

// publishLoadLocked 首次使用时载入；崩溃残留的 uploading 一律回 pending。
func publishLoadLocked() {
	if publishLoaded {
		return
	}
	publishLoaded = true
	if publishPos == "" {
		publishPos = filepath.Join(AppCfg().DataDirPath(), publishStatusName)
	}
	data, err := os.ReadFile(publishPos)
	if err != nil {
		return
	}
	var loaded []*publishJob
	if json.Unmarshal(data, &loaded) != nil {
		log.Printf("[BILI] ⚠️ 投稿队列文件损坏，已忽略: %s", publishPos)
		return
	}
	for _, j := range loaded {
		if j.Status == publishUploading {
			j.Status = publishPending // 上次进程中断，重投
		}
	}
	publishQueue = loaded
	// 启动清扫：中断期间被删掉源文件的任务，重投也是白投，直接出队。
	if n := publishSweepMissingLocked(); n > 0 {
		log.Printf("[BILI] 🧹 启动清扫：移除 %d 条源文件已删除的投稿任务", n)
	}
}

// publishSweepMissingLocked 清扫失效任务：源文件已被删除（手动清理高光等）的
// pending/failed 条目永远不可能再成功，直接出队（调用方持锁）。
// uploading 在途不扫；done 是发布历史且参与每日限额计数，一律保留。
// 只认 os.IsNotExist：网络盘暂不可达等临时错误不误删。
func publishSweepMissingLocked() int {
	removed := 0
	kept := publishQueue[:0]
	for _, j := range publishQueue {
		if j.Status == publishPending || j.Status == publishFailed {
			if _, err := os.Stat(j.File); err != nil && os.IsNotExist(err) {
				removed++
				continue
			}
		}
		kept = append(kept, j)
	}
	if removed > 0 {
		publishQueue = kept
		publishSaveLocked()
	}
	return removed
}

// publishTrimLocked 超上限时丢最老的已完结条目（调用方持锁）。
func publishTrimLocked() {
	for len(publishQueue) > publishQueueMax {
		idx := -1
		for i, j := range publishQueue {
			if j.Status == publishDone || j.Status == publishFailed {
				idx = i
				break
			}
		}
		if idx < 0 {
			return
		}
		publishQueue = append(publishQueue[:idx], publishQueue[idx+1:]...)
	}
}

// resetPublishState 数据目录变更时重置队列路径与内存缓存（由 ApplyDataDir 调用）。
func resetPublishState(dataDir string) {
	publishMu.Lock()
	defer publishMu.Unlock()
	publishPos = filepath.Join(dataDir, publishStatusName)
	publishQueue = nil
	publishLoaded = false
}

// publishStatsLocked 队列计数快照（调用方持锁）。
func publishStatsLocked() map[string]int {
	pending, uploading, done, failed := 0, 0, 0, 0
	for _, j := range publishQueue {
		switch j.Status {
		case publishPending:
			pending++
		case publishUploading:
			uploading++
		case publishDone:
			done++
		case publishFailed:
			failed++
		}
	}
	return map[string]int{"pending": pending, "uploading": uploading, "done": done, "failed": failed}
}

// publishRetry 手动重置一条任务为 pending 并立即叫醒 worker。
// pending 也允许：那是「退避等待中」的任务，用户点重试就是想跳过等待马上再试。
func publishRetry(id string) bool {
	publishMu.Lock()
	publishLoadLocked()
	hit := false
	for _, j := range publishQueue {
		if j.ID == id && j.Status != publishUploading {
			j.Status = publishPending
			j.Tries = 0
			j.NextAt = time.Time{}
			j.Err = ""
			publishSaveLocked()
			hit = true
			break
		}
	}
	publishMu.Unlock()
	if hit {
		publishTrigger()
	}
	return hit
}

// publishDelete 从队列移除一条任务（pending/failed 可删；进行中不允许）。
func publishDelete(id string) bool {
	publishMu.Lock()
	defer publishMu.Unlock()
	publishLoadLocked()
	for i, j := range publishQueue {
		if j.ID == id && j.Status != publishUploading {
			publishQueue = append(publishQueue[:i], publishQueue[i+1:]...)
			publishSaveLocked()
			return true
		}
	}
	return false
}

// publishMove 调整发布顺序：把一条任务与相邻任务交换位置（delta=-1 上移 / +1 下移）。
// 队列切片顺序即 worker 的取件顺序（只挑 pending），所以对非上传中的任务交换即可。
func publishMove(id string, delta int) bool {
	if delta != -1 && delta != 1 {
		return false
	}
	publishMu.Lock()
	defer publishMu.Unlock()
	publishLoadLocked()
	for i, j := range publishQueue {
		if j.ID == id && j.Status != publishUploading {
			dst := i + delta
			if dst < 0 || dst >= len(publishQueue) {
				return false
			}
			if publishQueue[dst].Status == publishUploading {
				return false
			}
			publishQueue[i], publishQueue[dst] = publishQueue[dst], publishQueue[i]
			publishSaveLocked()
			return true
		}
	}
	return false
}

// publishSetCover 设置一条任务的封面截帧时间点（秒）；0 = 恢复默认（30% 处）。
func publishSetCover(id string, sec int) bool {
	publishMu.Lock()
	defer publishMu.Unlock()
	publishLoadLocked()
	for _, j := range publishQueue {
		if j.ID == id && j.Status != publishUploading {
			if sec < 0 {
				sec = 0
			}
			if j.Duration > 0 && sec > j.Duration-1 {
				sec = j.Duration - 1
			}
			j.CoverAt = sec
			publishSaveLocked()
			return true
		}
	}
	return false
}

// publishAddCandidate 把一份在盘上的高光产物手动加入投稿队列（选视频入队）。
func publishAddCandidate(path string) (bool, string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if info, serr := os.Stat(abs); serr != nil || info.IsDir() {
		return false, "文件不存在或不可读"
	}
	job := buildPublishJob(abs, nil)
	if job == nil {
		return false, "不是有效的高光产物（*_highlight.mp4）"
	}
	publishMu.Lock()
	defer publishMu.Unlock()
	publishLoadLocked()
	for _, j := range publishQueue {
		if j.ID == job.ID {
			return false, "该视频已在投稿队列中"
		}
	}
	publishQueue = append(publishQueue, job)
	publishTrimLocked()
	publishSaveLocked()
	log.Printf("[BILI] 📥 手动加入投稿队列: %s（%s）", job.Streamer, filepath.Base(job.File))
	BroadcastWS("biliQueue", publishStatsLocked())
	publishTrigger()
	return true, ""
}

// ---------- 对外导出（api/http 控制台用） ----------

// PublishJobDTO 投稿任务的对外快照（API 契约，字段改动需同步 web/index.html）。
type PublishJobDTO struct {
	ID        string `json:"id"`
	File      string `json:"file"`
	FileName  string `json:"file_name"`
	Streamer  string `json:"streamer"`
	ClipDate  string `json:"clip_date"`
	ClipTime  string `json:"clip_time"`
	Segments  int    `json:"segments"`
	Duration  int    `json:"duration"`
	Status    string `json:"status"`
	Tries     int    `json:"tries"`
	Err       string `json:"err,omitempty"`
	NextAt    string `json:"next_at,omitempty"`
	DoneAt    string `json:"done_at,omitempty"`
	AID       int64  `json:"aid,omitempty"`
	BvID      string `json:"bvid,omitempty"`
	Title     string `json:"title,omitempty"`
	CoverURL  string `json:"cover_url,omitempty"`
	CoverAt   int    `json:"cover_at,omitempty"`
	FileSize  int64  `json:"file_size,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// PublishCandidateDTO 候选条目：已产出、在盘上、还没进投稿队列的高光。
type PublishCandidateDTO struct {
	ID       string `json:"id"`
	FileName string `json:"file_name"`
	Streamer string `json:"streamer"`
	ClipDate string `json:"clip_date"`
	ClipTime string `json:"clip_time"`
	FileSize int64  `json:"file_size"`
}

// dtoTime 零值时间输出空串（前端不需要 0001-01-01）。
func dtoTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

// PublishSnapshot 队列快照，**按处理顺序**（即队列切片顺序）返回：
// UI 从上到下就是发布顺序，「上移」= 更早被处理。
func PublishSnapshot() []PublishJobDTO {
	publishMu.Lock()
	defer publishMu.Unlock()
	publishLoadLocked()
	out := make([]PublishJobDTO, 0, len(publishQueue))
	for _, j := range publishQueue {
		out = append(out, PublishJobDTO{
			ID: j.ID, File: j.File, FileName: filepath.Base(j.File),
			Streamer: j.Streamer, ClipDate: j.ClipDate, ClipTime: j.ClipTime,
			Segments: j.Segments, Duration: j.Duration,
			Status: j.Status, Tries: j.Tries, Err: j.Err,
			NextAt: dtoTime(j.NextAt), DoneAt: dtoTime(j.DoneAt),
			AID: j.AID, BvID: j.BvID, Title: j.Title, CoverURL: j.CoverURL,
			CoverAt: j.CoverAt, FileSize: j.FileSize, CreatedAt: dtoTime(j.CreatedAt),
		})
	}
	return out
}

// PublishStats 队列计数与今日已完成数。
func PublishStats() (map[string]int, int) {
	publishMu.Lock()
	defer publishMu.Unlock()
	publishLoadLocked()
	stats := publishStatsLocked()
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	today := 0
	for _, j := range publishQueue {
		if j.Status == publishDone && j.DoneAt.After(dayStart) {
			today++
		}
	}
	return stats, today
}

// PublishQueueAction 队列操作分发。
//   - retry：重置为 pending 并立即叫醒 worker（等待中/失败/已完成均可）；
//   - delete：移除任务（进行中不允许）；
//   - move：调整顺序，dir = -1 上移 / +1 下移；
//   - cover：设置封面截帧秒数（sec，0 = 恢复默认 30%）；
//   - add：把候选文件（id = 产物绝对路径）加入队列；
//   - sweep：清扫源文件已删除的失效任务（id 不用传）；
//   - refresh：刷新入队，把盘上漏掉的新高光补进队列（id 不用传）。
//
// 返回（是否成功，失败原因/操作说明）。
func PublishQueueAction(action, id string, dir, sec int) (bool, string) {
	switch action {
	case "retry":
		return publishRetry(id), ""
	case "delete":
		return publishDelete(id), ""
	case "move":
		return publishMove(id, dir), ""
	case "cover":
		return publishSetCover(id, sec), ""
	case "add":
		return publishAddCandidate(id)
	case "sweep":
		if n := PublishSweepMissing(); n > 0 {
			return true, fmt.Sprintf("已移除 %d 条失效任务（源文件已删除）", n)
		}
		return true, "队列干净：没有源文件丢失的任务"
	case "refresh":
		_, msg := PublishEnqueueLatest()
		return true, msg
	}
	return false, "未知操作"
}

// PublishEnqueueLatest 「刷新入队」：扫描盘上未入队的高光，把漏掉的新文件补进队列。
// 范围口径：只入队文件修改时间**晚于队列里最新任务**的产物（追平漏入队的新文件）；
// 队列为空（或队列引用的文件都已不在盘上）时退化为每个主播只取最新一条，
// 避免一键把积压的几十个旧文件灌进队列。返回（入队条数，结果说明）。
func PublishEnqueueLatest() (int, string) {
	publishMu.Lock()
	publishLoadLocked()

	// 水位：队列任务引用文件的最新修改时间
	var watermark time.Time
	for _, j := range publishQueue {
		if info, err := os.Stat(j.File); err == nil && info.ModTime().After(watermark) {
			watermark = info.ModTime()
		}
	}

	inQueue := make(map[string]bool, len(publishQueue))
	for _, j := range publishQueue {
		inQueue[j.ID] = true
	}
	type cand struct {
		job *publishJob
		mod time.Time
	}
	cands := make([]cand, 0, 8)
	for _, path := range highlightOutputs() {
		if inQueue[path] {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		job := buildPublishJob(path, nil)
		if job == nil {
			continue
		}
		cands = append(cands, cand{job: job, mod: info.ModTime()})
	}

	picked := make([]cand, 0, len(cands))
	if watermark.IsZero() {
		// 队列为空：每个主播只取最新一条
		latest := make(map[string]cand, 4)
		for _, c := range cands {
			if cur, ok := latest[c.job.Streamer]; !ok || c.mod.After(cur.mod) {
				latest[c.job.Streamer] = c
			}
		}
		for _, c := range latest {
			picked = append(picked, c)
		}
	} else {
		for _, c := range cands {
			if c.mod.After(watermark) {
				picked = append(picked, c)
			}
		}
	}
	// 旧→新追加到队尾，保持「从上到下依次发布」的顺序语义
	sort.Slice(picked, func(i, k int) bool { return picked[i].mod.Before(picked[k].mod) })

	names := make([]string, 0, len(picked))
	for _, c := range picked {
		publishQueue = append(publishQueue, c.job)
		names = append(names, filepath.Base(c.job.File))
	}
	if len(names) > 0 {
		publishTrimLocked()
		publishSaveLocked()
	}
	stats := publishStatsLocked()
	publishMu.Unlock()

	if len(names) == 0 {
		return 0, "没有要补的新文件"
	}
	log.Printf("[BILI] 🔄 刷新入队：补入 %d 条新高光", len(names))
	BroadcastWS("biliQueue", stats)
	publishTrigger()
	summary := strings.Join(names, "、")
	if len(names) > 3 {
		summary = strings.Join(names[:2], "、") + fmt.Sprintf(" 等 %d 条", len(names))
	}
	return len(names), fmt.Sprintf("已补入 %d 条新高光：%s", len(names), summary)
}

// PublishSweepMissing 清扫失效任务并返回移除条数（控制台「清理失效」按钮，
// 以及每轮 tick 的自动清扫共用入口）。只动 pending/failed，见 publishSweepMissingLocked。
func PublishSweepMissing() int {
	publishMu.Lock()
	publishLoadLocked()
	n := publishSweepMissingLocked()
	stats := publishStatsLocked()
	publishMu.Unlock()
	if n > 0 {
		BroadcastWS("biliQueue", stats)
		publishTrigger() // 清完马上跑一轮，别空等 30s
	}
	return n
}

// PublishCandidates 列出可手动入队的候选：已产出、仍在盘上、尚未在队列里的高光。
func PublishCandidates() []PublishCandidateDTO {
	publishMu.Lock()
	publishLoadLocked()
	inQueue := make(map[string]bool, len(publishQueue))
	for _, j := range publishQueue {
		inQueue[j.ID] = true
	}
	publishMu.Unlock()

	out := make([]PublishCandidateDTO, 0, 8)
	for _, path := range highlightOutputs() {
		if inQueue[path] {
			continue
		}
		job := buildPublishJob(path, nil)
		if job == nil {
			continue
		}
		out = append(out, PublishCandidateDTO{
			ID: path, FileName: filepath.Base(path),
			Streamer: job.Streamer, ClipDate: job.ClipDate, ClipTime: job.ClipTime,
			FileSize: job.FileSize,
		})
	}
	return out
}

// PublishPosterFor 返回指定任务/候选视频在 sec 秒处的预览帧缓存文件。
// id 先按队列任务 ID 解析出真实文件；未命中时把 id 当作候选文件路径。
func PublishPosterFor(id string, sec int) (string, error) {
	publishMu.Lock()
	publishLoadLocked()
	file := ""
	for _, j := range publishQueue {
		if j.ID == id {
			file = j.File
			if sec <= 0 && j.CoverAt > 0 {
				sec = j.CoverAt
			}
			break
		}
	}
	publishMu.Unlock()
	if file == "" {
		file = id
	}
	return publishPoster(file, sec)
}

// publishWarnMissingCookie 开了投稿但没填 Cookie 的节流警告（10 分钟一条）。
func publishWarnMissingCookie() {
	now := time.Now().Unix()
	publishMu.Lock()
	last := publishLastWarn
	publishMu.Unlock()
	if now-last < 600 {
		return
	}
	publishMu.Lock()
	publishLastWarn = now
	publishMu.Unlock()
	log.Println("[BILI] ⚠️ B站投稿已开启，但 SESSDATA/bili_jct 未配置，队列暂停处理（控制台 → B站投稿 填写 Cookie）")
}

// tailStr 取字符串尾部 n 字符并压掉换行。
func tailStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return strings.ReplaceAll(s, "\n", " ")
	}
	return strings.ReplaceAll(s[len(s)-n:], "\n", " ")
}
