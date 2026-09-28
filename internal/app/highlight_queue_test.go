package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 主播名取自路径倒数第三段（…/<主播>/<日期>/<片>）。
func TestHighlightStreamerOf(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"D:/downloads/抖音直播/菜菜很忙/2026-09-27/a_003.ts", "菜菜很忙"},
		{"D:/downloads/抖音直播/菜菜很忙2/2026-09-27/b_001.mp4", "菜菜很忙2"},
		{"a.ts", ""}, // 路径层级不足时给空串，前端回落显示片名
	}
	for _, c := range cases {
		if got := highlightStreamerOf(c.path); got != c.want {
			t.Fatalf("highlightStreamerOf(%q) = %q，期望 %q", c.path, got, c.want)
		}
	}
}

// 今日统计只认 AnalyzedAt 是今天的条目；产出/失败互斥归类。
func TestHighlightTodayStats(t *testing.T) {
	now := time.Date(2026, 9, 27, 18, 0, 0, 0, time.Local)
	state := map[string]highlightEntry{
		"a.ts": {AnalyzedAt: "2026-09-27 10:00:00", Output: "a_highlight.mp4", Segments: 2},
		"b.ts": {AnalyzedAt: "2026-09-27 11:00:00"}, // 未检出
		"c.ts": {AnalyzedAt: "2026-09-27 12:00:00", Err: "io 抖动", Attempts: 1},
		"d.ts": {AnalyzedAt: "2026-09-26 23:59:59", Output: "d_highlight.mp4", Segments: 5}, // 昨天
		"e.ts": {AnalyzedAt: ""},                                                            // 无时间戳（理论上不会出现）
	}
	got := highlightTodayStats(state, now)
	if got.Analyzed != 3 || got.Highlights != 1 || got.Segments != 2 || got.Failed != 1 {
		t.Fatalf("今日统计不符：analyzed=%d highlights=%d segments=%d failed=%d，期望 3/1/2/1",
			got.Analyzed, got.Highlights, got.Segments, got.Failed)
	}
}

// 最近判定按 AnalyzedAt 倒序，且受 n 截断。
func TestHighlightRecentDone(t *testing.T) {
	state := map[string]highlightEntry{
		"a.ts": {AnalyzedAt: "2026-09-27 10:00:00", Output: "a_highlight.mp4", Segments: 2, Size: 1048576},
		"b.ts": {AnalyzedAt: "2026-09-27 12:00:00"},
		"c.ts": {AnalyzedAt: "2026-09-27 11:00:00", Err: "fail"},
	}
	got := highlightRecentDone(state, 2)
	if len(got) != 2 {
		t.Fatalf("应取最近 2 条，实际 %d 条", len(got))
	}
	if got[0].Clip != "b.ts" || got[1].Clip != "c.ts" {
		t.Fatalf("应按时间倒序 b→c，实际 %s → %s", got[0].Clip, got[1].Clip)
	}
}

// 待分析队列口径：认领片优先入队头；小文件与已有定论的片不进队列；mtime 正序。
// 注意 findStableClips 的入参是单个主播目录（其下直接是日期目录）。
func TestHighlightPendingList(t *testing.T) {
	resetHighlightState(t.TempDir())
	rootA := t.TempDir() // 主播A 目录
	day := filepath.Join(rootA, "2026-09-27")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, ago time.Duration) string {
		p := filepath.Join(day, name)
		writeSizedFile(t, p, 2<<20)
		ts := time.Now().Add(-ago)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mk("a_001.ts", 30*time.Minute)
	mk("a_002.ts", 20*time.Minute)
	// <1MB 的收尾碎片：无分析价值，不进队列
	tiny := filepath.Join(day, "tiny.ts")
	writeSizedFile(t, tiny, 512<<10)
	if err := os.Chtimes(tiny, time.Now().Add(-10*time.Minute), time.Now().Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	done := mk("done.ts", 25*time.Minute)
	highlightStateSet(done, highlightEntry{Output: "done_highlight.mp4", Segments: 1}) // 已有定论

	// 认领片在另一个主播目录，应排队头最前
	rootB := t.TempDir()
	other := filepath.Join(rootB, "2026-09-27", "b_001.ts")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSizedFile(t, other, 2<<20)
	highlightClaimed.Store(other, time.Now())
	defer highlightClaimed.Delete(other)

	pending, head := highlightPendingList([]string{rootA, rootB}, 6)
	if pending != 3 {
		t.Fatalf("积压应为 3（认领 1 + 常规 2），实际 %d", pending)
	}
	if len(head) != 3 || head[0].Clip != "b_001.ts" || !head[0].Claimed {
		t.Fatalf("认领片应排队头最前，实际 %v", head)
	}
	if head[1].Clip != "a_001.ts" || head[2].Clip != "a_002.ts" {
		t.Fatalf("常规队列应按 mtime 正序，实际 %v", head)
	}
	if head[0].Streamer != filepath.Base(rootB) || head[1].Streamer != filepath.Base(rootA) {
		t.Fatalf("主播名解析不符：%v", head)
	}
	if _, head1 := highlightPendingList([]string{rootA}, 1); len(head1) != 1 {
		t.Fatalf("headCap=1 应只返回 1 条队头，实际 %d", len(head1))
	}
}

// 当前片阶段跟踪：begin → stage 推进 → end 清除；换片后旧片的 stage 更新不生效。
func TestHighlightCurrentStageTracking(t *testing.T) {
	src := "D:/downloads/抖音直播/主播A/2026-09-27/a_001.ts"
	hlCurrentBegin(src)
	cur := highlightCurrentSnapshot()
	if cur.Clip != "a_001.ts" || cur.Stage != "probe" || cur.Streamer != "主播A" {
		t.Fatalf("begin 后应为 probe： %+v", cur)
	}
	hlCurrentStage(src, "gate")
	if cur := highlightCurrentSnapshot(); cur.Stage != "gate" {
		t.Fatalf("gate 阶段未生效：%+v", cur)
	}
	hlCurrentStage("别的片.ts", "cut") // 不应对当前片生效
	if cur := highlightCurrentSnapshot(); cur.Stage != "gate" {
		t.Fatalf("他片 stage 更新不应生效：%+v", cur)
	}
	hlCurrentEnd(src)
	if cur := highlightCurrentSnapshot(); cur.Clip != "" {
		t.Fatalf("end 后应清空：%+v", cur)
	}
}

// 冷却自动重试：失败达上限 → 冷却期内不可重试；把最后失败时间拨回冷却期之前 →
// 可重试并 revive（attempts 清零、轮数+1）；轮数耗尽后彻底放弃且判定为定论。
func TestHighlightCooldownRetry(t *testing.T) {
	resetHighlightState(t.TempDir())
	src := "D:/downloads/抖音直播/主播A/2026-09-27/a_001.ts"
	for i := 0; i < maxHighlightAttempts; i++ {
		highlightRecordFailure(src, errors.New("boom"))
	}
	if highlightCanRetry(src) {
		t.Fatal("冷却期内不应可重试")
	}
	if e, _ := highlightStateGet(src); highlightConcluded(e) {
		t.Fatal("冷却期内的失败片不是定论")
	}

	// 拨回冷却期之前（模拟时间流逝；highlightStateSet 会强制刷新时间戳，须绕行直写）
	old := time.Now().Add(-highlightRetryCooldown - time.Minute).Format("2006-01-02 15:04:05")
	highlightStateLoad()
	highlightMu.Lock()
	e, _ := highlightState[src]
	e.AnalyzedAt = old
	highlightState[src] = e
	highlightMu.Unlock()
	if !highlightCanRetry(src) {
		t.Fatal("冷却期满应恢复可重试")
	}

	highlightMaybeRevive(src)
	e, _ = highlightStateGet(src)
	if e.Attempts != 0 || e.Rounds != 1 {
		t.Fatalf("revive 应清零 attempts 并进入第 1 轮，实际 attempts=%d rounds=%d", e.Attempts, e.Rounds)
	}
	if !highlightCanRetry(src) {
		t.Fatal("revive 后（次数未用）应可重试")
	}

	// 后续轮次直接把 rounds 推到上限 → 彻底放弃
	e.Rounds = maxHighlightRetryRounds
	e.Attempts = maxHighlightAttempts
	highlightStateSet(src, e)
	if highlightCanRetry(src) {
		t.Fatal("轮数耗尽后不应再重试")
	}
	if e, _ := highlightStateGet(src); !highlightConcluded(e) {
		t.Fatal("轮数耗尽应为定论（可处置源片）")
	}
}

// 并行分析的阶段跟踪：两片同时在分析时各自推进互不干扰，快照返回两条。
func TestHighlightCurrentMultiClipTracking(t *testing.T) {
	a := "D:/downloads/抖音直播/主播A/2026-09-27/a_001.ts"
	b := "D:/downloads/抖音直播/主播B/2026-09-27/b_001.ts"
	hlCurrentBegin(a)
	hlCurrentBegin(b)
	hlCurrentStage(b, "gate")
	items := highlightCurrentsSnapshot()
	if len(items) != 2 {
		t.Fatalf("应同时跟踪 2 片，实际 %d", len(items))
	}
	var gate *HighlightCurrent
	for i := range items {
		if items[i].Stage == "gate" {
			gate = &items[i]
		}
	}
	if gate == nil || gate.Clip != "b_001.ts" {
		t.Fatalf("快照应含 b 片的 gate 阶段，实际 %v", items)
	}
	hlCurrentEnd(a)
	if cur := highlightCurrentSnapshot(); cur.Clip != "b_001.ts" {
		t.Fatalf("end(a) 后应只剩 b 片，实际 %+v", cur)
	}
	hlCurrentEnd(b)
	if items := highlightCurrentsSnapshot(); len(items) != 0 {
		t.Fatalf("全部 end 后应为空，实际 %v", items)
	}
}
