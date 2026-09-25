package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"upload/internal/config"
	"upload/internal/highlight"
)

// resetPublishForTest 把投稿队列指到临时文件并阻断自动加载（不动全局 CfgStore）。
func resetPublishForTest(t *testing.T) {
	t.Helper()
	publishMu.Lock()
	defer publishMu.Unlock()
	publishQueue = nil
	publishLoaded = true // 阻止 publishLoadLocked 回读 AppCfg
	publishPos = filepath.Join(t.TempDir(), "bili_publish.json")
	t.Cleanup(func() {
		publishMu.Lock()
		defer publishMu.Unlock()
		publishQueue = nil
		publishLoaded = false
		publishPos = ""
	})
}

// makeHighlightFixture 造一份 <主播>/<日期>/高光/<主播>_<日期>_<时刻>_000_highlight.mp4 产物。
func makeHighlightFixture(t *testing.T, root, streamer, date, clock string) string {
	t.Helper()
	dir := filepath.Join(root, streamer, date, "高光")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, streamer+"_"+date+"_"+clock+"_000_highlight.mp4")
	if err := os.WriteFile(p, []byte("fake video"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildPublishJobParsesPath(t *testing.T) {
	root := t.TempDir()
	out := makeHighlightFixture(t, root, "菜菜_很忙", "2026-09-25", "21-30-05")
	job := buildPublishJob(out, []highlight.Segment{
		{Start: 10, End: 40, Score: 2.5},
		{Start: 50, End: 90, Score: 4.1},
	})
	if job == nil {
		t.Fatal("job 不应为 nil")
	}
	if job.ID != out || job.File != out {
		t.Errorf("ID/File = %s/%s", job.ID, job.File)
	}
	if job.Streamer != "菜菜_很忙" {
		t.Errorf("Streamer = %q（主播名含下划线不能解析错）", job.Streamer)
	}
	if job.ClipDate != "2026-09-25" || job.ClipTime != "21-30-05" {
		t.Errorf("ClipDate/ClipTime = %q/%q（日期时间应从文件名右侧解析）", job.ClipDate, job.ClipTime)
	}
	if job.Segments != 2 || job.StartSec != 10 || job.EndSec != 90 || job.Duration != 80 {
		t.Errorf("段信息 = %d/%d/%d/%d", job.Segments, job.StartSec, job.EndSec, job.Duration)
	}
	if job.Score != 4.1 {
		t.Errorf("Score = %v, 期望取最高段 4.1", job.Score)
	}
	if job.Status != publishPending || job.CreatedAt.IsZero() {
		t.Errorf("初始状态异常: %s %v", job.Status, job.CreatedAt)
	}
	if job.FileSize != int64(len("fake video")) {
		t.Errorf("FileSize = %d", job.FileSize)
	}
}

func TestBuildPublishJobRejectsNonHighlight(t *testing.T) {
	if buildPublishJob(filepath.Join(t.TempDir(), "a.ts"), nil) != nil {
		t.Error("非 _highlight 产物应返回 nil")
	}
}

func TestBuildPublishJobToleratesMissingTimestamp(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "某主播", "2026-09-25", "高光")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "某主播_highlight.mp4")
	os.WriteFile(p, []byte("x"), 0o644)

	job := buildPublishJob(p, []highlight.Segment{{Start: 0, End: 10, Score: 1}})
	if job == nil {
		t.Fatal("时间信息缺失也应入队")
	}
	if job.ClipDate != "" || job.ClipTime != "" {
		t.Errorf("解析不出时间应留空: %q/%q", job.ClipDate, job.ClipTime)
	}
	if job.Streamer != "某主播" {
		t.Errorf("Streamer = %q", job.Streamer)
	}
}

func TestPublishRenderTemplate(t *testing.T) {
	job := &publishJob{
		Streamer: "菜菜", ClipDate: "2026-09-25", ClipTime: "21-30-05",
		Segments: 2, StartSec: 10, EndSec: 70, Duration: 60, Score: 3.25,
	}
	got := publishRenderTemplate("【{streamer}】{date} {time} | {segments}段 {duration} ({start}-{end}s score={score})", job)
	want := "【菜菜】2026-09-25 21-30-05 | 2段 1m00s (10-70s score=3.25)"
	if got != want {
		t.Errorf("render = %q, want %q", got, want)
	}
	if alias := publishRenderTemplate("({name}) {time} 直播录像回放", job); alias != "(菜菜) 21-30-05 直播录像回放" {
		t.Errorf("{name} 别名渲染 = %q", alias)
	}
	if publishRenderTemplate("", job) != "" {
		t.Error("空模板应返回空")
	}
}

func TestPublishQueuePersistenceAndRecovery(t *testing.T) {
	resetPublishForTest(t)
	root := t.TempDir()
	out := makeHighlightFixture(t, root, "主播A", "2026-09-25", "10-00-00")

	publishMu.Lock()
	job := buildPublishJob(out, []highlight.Segment{{Start: 0, End: 5, Score: 1}})
	publishQueue = append(publishQueue, job)
	job.Status = publishUploading // 模拟上传中途崩溃
	publishSaveLocked()
	publishMu.Unlock()

	// 重新载入：uploading 应回 pending（崩溃恢复）
	publishMu.Lock()
	publishQueue = nil
	publishLoaded = false
	publishLoadLocked()
	defer publishMu.Unlock()
	if len(publishQueue) != 1 {
		t.Fatalf("载入条数 = %d", len(publishQueue))
	}
	if publishQueue[0].Status != publishPending {
		t.Errorf("崩溃恢复状态 = %s, 期望 pending", publishQueue[0].Status)
	}
	// 落盘文件确实存在
	if _, err := os.Stat(publishPos); err != nil {
		t.Errorf("队列文件未落盘: %v", err)
	}
}

func TestPublishRetryAndDelete(t *testing.T) {
	resetPublishForTest(t)
	root := t.TempDir()
	done := buildPublishJob(makeHighlightFixture(t, root, "主播A", "2026-09-25", "10-00-00"), nil)
	fail := buildPublishJob(makeHighlightFixture(t, root, "主播B", "2026-09-25", "11-00-00"), nil)
	done.Status = publishDone
	done.DoneAt = time.Now()
	fail.Status = publishFailed
	fail.Tries = 3
	fail.Err = "boom"
	publishMu.Lock()
	publishQueue = append(publishQueue, done, fail)
	publishMu.Unlock()

	if !publishRetry(fail.ID) {
		t.Fatal("failed 任务应可重试")
	}
	publishMu.Lock()
	if fail.Status != publishPending || fail.Tries != 0 || fail.Err != "" || !fail.NextAt.IsZero() {
		t.Errorf("重试后状态异常: %+v", fail)
	}
	publishMu.Unlock()

	if !publishDelete(done.ID) {
		t.Fatal("done 任务应可删除")
	}
	publishMu.Lock()
	n := len(publishQueue)
	publishMu.Unlock()
	if n != 1 {
		t.Errorf("删除后剩余 %d 条", n)
	}
}

func TestPublishTrimDropsOldestFinished(t *testing.T) {
	resetPublishForTest(t)
	publishMu.Lock()
	defer publishMu.Unlock()
	// 501 条全部 done（含一条 pending 在最前，验证 pending 不被裁）
	pend := &publishJob{ID: "p", Status: publishPending}
	publishQueue = append(publishQueue, pend)
	for i := 0; i < publishQueueMax; i++ {
		publishQueue = append(publishQueue, &publishJob{ID: string(rune('a'+i%26)) + string(rune(i)), Status: publishDone})
	}
	publishTrimLocked()
	if len(publishQueue) != publishQueueMax {
		t.Errorf("裁剪后 = %d, 期望 %d", len(publishQueue), publishQueueMax)
	}
	if publishQueue[0].ID != "p" {
		t.Errorf("pending 被误裁")
	}
}

func TestPublishEnqueueDedupsAndHonorsSwitch(t *testing.T) {
	resetPublishForTest(t)
	root := t.TempDir()
	out := makeHighlightFixture(t, root, "主播A", "2026-09-25", "10-00-00")
	segs := []highlight.Segment{{Start: 0, End: 5, Score: 1}}

	// 开关关闭：不入队
	cfgSnapshot := CfgStore.Get()
	cfg := cfgSnapshot
	cfg.Bilibili.Enable = false
	CfgStore.Replace(cfg)
	publishEnqueueHighlight(out, segs)
	publishMu.Lock()
	n := len(publishQueue)
	publishMu.Unlock()
	if n != 0 {
		t.Fatalf("总开关关闭不应入队, got %d", n)
	}

	// 开关打开：入队一次；重复触发去重
	cfg.Bilibili.Enable = true
	CfgStore.Replace(cfg)
	publishEnqueueHighlight(out, segs)
	publishEnqueueHighlight(out, segs)
	publishMu.Lock()
	n = len(publishQueue)
	publishMu.Unlock()
	if n != 1 {
		t.Errorf("重复产物应去重, got %d", n)
	}
}

func TestPublishNextJobGates(t *testing.T) {
	resetPublishForTest(t)
	cfg := CfgStore.Get()
	cfg.Bilibili.Enable = true
	cfg.Bilibili.MinIntervalMinutes = 10
	cfg.Bilibili.DailyLimit = 1
	cfg.Bilibili.MaxRetry = 3
	CfgStore.Replace(cfg)

	root := t.TempDir()
	a := buildPublishJob(makeHighlightFixture(t, root, "主播A", "2026-09-25", "10-00-00"), nil)
	b := buildPublishJob(makeHighlightFixture(t, root, "主播B", "2026-09-25", "11-00-00"), nil)
	publishMu.Lock()
	publishQueue = append(publishQueue, a, b)
	publishMu.Unlock()

	// 首次取出：无间隔限制（lastSubmit 零值）
	got := publishNextJob(cfg.Bilibili)
	if got == nil || got.ID != a.ID {
		t.Fatalf("首次应取出 a, got %+v", got)
	}
	if got.Status != publishUploading {
		t.Errorf("取出后状态 = %s", got.Status)
	}
	// 模拟刚提交过一篇 → 间隔闸生效
	publishMu.Lock()
	publishLastSubmit = time.Now()
	publishMu.Unlock()
	if publishNextJob(cfg.Bilibili) != nil {
		t.Error("最小间隔内不应取任务")
	}
	// 间隔过后，今日上限（1）闸生效：把 a 标记为今日已完成
	publishMu.Lock()
	publishLastSubmit = time.Time{}
	a.Status = publishDone
	a.DoneAt = time.Now()
	publishMu.Unlock()
	if publishNextJob(cfg.Bilibili) != nil {
		t.Error("达每日上限后不应取任务")
	}
}

func TestPublishFailBackoffAndTerminal(t *testing.T) {
	resetPublishForTest(t)
	cfg := configForPublishTest(2) // MaxRetry=2
	root := t.TempDir()
	job := buildPublishJob(makeHighlightFixture(t, root, "主播A", "2026-09-25", "10-00-00"), nil)
	publishMu.Lock()
	publishQueue = append(publishQueue, job)
	publishMu.Unlock()

	publishFail(job, cfg, os.ErrNotExist)
	if job.Status != publishPending || job.Tries != 1 || job.NextAt.IsZero() {
		t.Errorf("首次失败应退避重试: %s tries=%d", job.Status, job.Tries)
	}
	publishFail(job, cfg, os.ErrNotExist)
	if job.Status != publishFailed {
		t.Errorf("重试耗尽应转 failed, got %s", job.Status)
	}
	if !strings.Contains(job.Err, "file does not exist") && job.Err == "" {
		t.Errorf("失败原因未记录")
	}
}

func configForPublishTest(maxRetry int) config.BilibiliSettings {
	return config.BilibiliSettings{
		Enable: true, SessData: "s", BiliJct: "j",
		MinIntervalMinutes: 10, DailyLimit: 20, MaxRetry: maxRetry,
	}
}
