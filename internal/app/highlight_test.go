package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"upload/internal/highlight"
)

func TestMatchHighlightOnlyPrefix(t *testing.T) {
	prefixes := []string{"D:/downloads/菜菜很忙"}
	const hlDir = "高光"

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"原片 ts 应跳过", "D:/downloads/菜菜很忙/2026-09-22/a_003.ts", true},
		{"原片 mp4 应跳过", "D:/downloads/菜菜很忙/2026-09-22/a_003.mp4", true},
		{"日期目录下的截图归档应放行", "D:/downloads/菜菜很忙/2026-09-22/Screenshots/菜菜很忙_2026-09-22_21-29-57_cover_0001.png", false},
		{"日期目录下的高光产物应放行", "D:/downloads/菜菜很忙/2026-09-22/高光/a_003_highlight.mp4", false},
		{"旧版主播级高光目录也放行", "D:/downloads/菜菜很忙/高光/a_003_highlight.mp4", false},
		{"其他主播不受影响", "D:/downloads/别的主播/2026-09-22/a.ts", false},
		{"前缀相似的另一主播不应误伤", "D:/downloads/菜菜很忙2/2026-09-22/a.ts", false},
		{"主播目录本身不是文件", "D:/downloads/菜菜很忙", false},
	}
	for _, c := range cases {
		if got := matchHighlightOnlyPrefix(c.path, prefixes, hlDir); got != c.want {
			t.Fatalf("%s: matchHighlightOnlyPrefix(%q) = %v，期望 %v", c.name, c.path, got, c.want)
		}
	}
}

func TestMatchHighlightOnlyPrefixNoTargets(t *testing.T) {
	if matchHighlightOnlyPrefix("D:/downloads/a/2026-09-22/x.ts", nil, "高光") {
		t.Fatal("没有「只传高光」主播时不应跳过任何文件")
	}
}

// 高光分析完的删源判定（2026-09-23 调整）：只要「确定不需要留了」就删，本地不留原片。
//   - 认领过的必删：pipeline 已上传完成并把它交给高光，不删没人删
//   - 「只传高光」主播的原片不进上传流程、网盘没有副本，只能靠「有无定论」判定
//   - 其余原片必须有上传凭证（哈希库命中）才敢删，否则删了就没得传
//   - 失败还能重试的一律留着，否则重试机制形同虚设
func TestHighlightShouldDeleteSource(t *testing.T) {
	cases := []struct {
		name       string
		claimed    bool
		concluded  bool
		uploaded   bool
		onlyTarget bool
		want       bool
	}{
		{"认领过的一律删", true, false, false, false, true},
		{"认领过且已定论的也删", true, true, false, false, true},
		{"只传高光 + 有定论 → 删（无需上传凭证）", false, true, false, true, true},
		{"只传高光但还能重试 → 留着", false, false, false, true, false},
		{"普通主播 + 有定论 + 已上传 → 删", false, true, true, false, true},
		{"普通主播 + 有定论但还没上传 → 留给 pipeline", false, true, false, false, false},
		{"普通主播 + 已上传但还能重试 → 留着", false, false, true, false, false},
		{"三样都没有 → 不删", false, false, false, false, false},
	}
	for _, c := range cases {
		if got := highlightShouldDeleteSource(c.claimed, c.concluded, c.uploaded, c.onlyTarget); got != c.want {
			t.Fatalf("%s: highlightShouldDeleteSource(claimed=%v, concluded=%v, uploaded=%v, onlyTarget=%v) = %v，期望 %v",
				c.name, c.claimed, c.concluded, c.uploaded, c.onlyTarget, got, c.want)
		}
	}
}

func TestHighlightOutputPathShape(t *testing.T) {
	outDir := "D:/downloads/菜菜很忙/高光"
	got := highlightOutputPath(outDir, "D:/downloads/菜菜很忙/2026-09-22/菜菜很忙_2026-09-22_15-48-48_003.ts")
	// 期望值用 filepath.Join 构造，避免在 Windows 上因分隔符差异误报
	want := filepath.Join(outDir, "菜菜很忙_2026-09-22_15-48-48_003_highlight.mp4")
	if got != want {
		t.Fatalf("高光产物路径错误:\n got %s\nwant %s", got, want)
	}
}

// 高光产物必须落在原片同一个日期目录下（<主播>/<日期>/高光/），
// 而不是 <主播>/高光/ —— 后者会把多天的高光混在一个目录里。
func TestHighlightOutDirFollowsClipDate(t *testing.T) {
	root := t.TempDir()
	resetHighlightState(t.TempDir())
	defer swapHighlightTargetDirs([]string{filepath.ToSlash(root)})()

	src := filepath.Join(root, "2026-09-22", "a_003.ts")
	got := highlightOutDirFor(src)
	want := filepath.Join(root, "2026-09-22", "高光")
	if got != want {
		t.Fatalf("高光产物目录应跟着原片的日期走:\n got %s\nwant %s", got, want)
	}

	// 产物自己不算原片，否则会被自我递归分析
	if got := highlightOutDirFor(filepath.Join(want, "a_003_highlight.mp4")); got != "" {
		t.Fatalf("高光产物自己不应被判为原片，实际返回 %q", got)
	}
}

func TestIsClipName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"a_003.ts", true},
		{"a_003.mp4", true},
		{"A_003.TS", true},
		{"a_003_highlight.mp4", false}, // 高光产物必须排除，否则会自我递归分析
		{"a_003_HIGHLIGHT.mp4", false},
		{"a_003.part", false},
		{"cover.png", false},
	}
	for _, c := range cases {
		if got := isClipName(c.name); got != c.want {
			t.Fatalf("isClipName(%q) = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// findStableClips 的三个判据必须同时生效：mtime 已稳定、体积够大、不是高光产物。
// 录制中的分片一直在写，过早分析会读到半截文件；高光产物若被当切片会自我递归。
func TestFindStableClips(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026-09-22")
	hlDir := filepath.Join(root, "高光")
	for _, d := range []string{day, hlDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-10 * time.Minute)

	stable := filepath.Join(day, "a_003.ts")
	writeSizedFile(t, stable, 2<<20)
	if err := os.Chtimes(stable, old, old); err != nil {
		t.Fatal(err)
	}

	// 刚写完的（仍在写入窗口内）
	writeSizedFile(t, filepath.Join(day, "a_004.ts"), 2<<20)

	// 太小的收尾碎片
	tiny := filepath.Join(day, "a_005.ts")
	writeSizedFile(t, tiny, 1024)
	if err := os.Chtimes(tiny, old, old); err != nil {
		t.Fatal(err)
	}

	// 高光产物本身
	prod := filepath.Join(hlDir, "a_003_highlight.mp4")
	writeSizedFile(t, prod, 2<<20)
	if err := os.Chtimes(prod, old, old); err != nil {
		t.Fatal(err)
	}

	got := findStableClips(root)
	if len(got) != 1 || got[0] != stable {
		t.Fatalf("应只返回已稳定的原片 %s，实际 %v", stable, got)
	}
}

func writeSizedFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAnalyzeClipIntegration 用真实素材验证 app 层单切片分析的完整流程：
// Probe → Select → Cut → 产物落到「高光」子目录 → 写入去重状态。
//
// 这是单测覆盖不到的部分（真实 ffmpeg、真实落盘、真实命名）。
// 需要设置 HIGHLIGHT_E2E_FFMPEG / HIGHLIGHT_E2E_SRC 才执行。
func TestAnalyzeClipIntegration(t *testing.T) {
	ffmpegBin := os.Getenv("HIGHLIGHT_E2E_FFMPEG")
	src := os.Getenv("HIGHLIGHT_E2E_SRC")
	if ffmpegBin == "" || src == "" {
		t.Skip("未设置 HIGHLIGHT_E2E_FFMPEG / HIGHLIGHT_E2E_SRC，跳过")
	}
	if _, err := os.Stat(src); err != nil {
		t.Skipf("素材不存在，跳过: %v", err)
	}

	// 状态文件落到临时目录，避免污染工作区的 data/
	resetHighlightState(t.TempDir())
	prevHook := FFmpegPathHook
	FFmpegPathHook = func() string { return ffmpegBin }
	defer func() { FFmpegPathHook = prevHook }()

	outDir := filepath.Join(t.TempDir(), "高光")
	analyzeClip(src, outDir, highlight.DefaultOptions())

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("高光目录不存在，说明 analyzeClip 没有产出: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("应产出 1 个高光文件，实际 %d 个", len(entries))
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, "_highlight.mp4") {
		t.Fatalf("产物命名不符: %s", name)
	}
	info, ierr := entries[0].Info()
	if ierr != nil || info.Size() == 0 {
		t.Fatalf("产物为空: %v", ierr)
	}
	t.Logf("产出 %s（%.1f MB）", name, float64(info.Size())/1024/1024)

	e, ok := highlightStateGet(src)
	if !ok {
		t.Fatal("分析完成后应记录去重状态")
	}
	if e.Output != name || e.Segments == 0 {
		t.Fatalf("状态记录不符: %+v", e)
	}
	t.Logf("去重状态: %d 段 / 输出 %s", e.Segments, e.Output)
}

// swapHighlightTargetDirs 直接替换目标目录缓存，返回恢复函数。
// 测试里不能走 highlightRefreshTargets —— 它会调 GetBuiltinRecorderTasks，
// 在无配置环境下拿不到东西。
func swapHighlightTargetDirs(dirs []string) func() {
	highlightTargetDirsMu.Lock()
	old := highlightTargetDirs
	highlightTargetDirs = dirs
	highlightTargetDirsMu.Unlock()
	return func() {
		highlightTargetDirsMu.Lock()
		highlightTargetDirs = old
		highlightTargetDirsMu.Unlock()
	}
}

// 回归：上传流程删源前必须被高光拦下。
//
// 曾经的故障：pipeline 上传/秒传成功后立刻 os.Remove(源)，而高光要等 3 分钟稳定期
// 才动手，于是每个切片都以「No such file or directory」收场，产出恒为 0。
func TestHighlightClaimBlocksRemovalOfPendingClip(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026-09-22")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	resetHighlightState(t.TempDir())
	defer swapHighlightTargetDirs([]string{filepath.ToSlash(root)})()

	src := filepath.Join(day, "a_003.mp4")
	writeSizedFile(t, src, 2<<20)

	// 未分析过的原片：必须拦下，交给高光接管
	if highlightClaim(src) {
		t.Fatal("未分析的原片不应放行删除")
	}
	if _, ok := highlightClaimed.Load(src); !ok {
		t.Fatal("认领后应登记在 highlightClaimed 里")
	}

	// 分析结束后必须被清理，否则文件会永远留在盘上
	highlightDisposeSource(src)
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("释放认领后源文件应被删除，stat err = %v", err)
	}
	if _, ok := highlightClaimed.Load(src); ok {
		t.Fatal("释放后认领记录应被清除")
	}
}

// 不该被拦的文件：非高光目标、以及高光产物自己（否则会自我递归）。
func TestHighlightClaimAllowsIrrelevantPaths(t *testing.T) {
	root := t.TempDir()
	resetHighlightState(t.TempDir())
	defer swapHighlightTargetDirs([]string{filepath.ToSlash(root)})()

	other := filepath.Join(t.TempDir(), "别的主播", "2026-09-22", "a.mp4")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSizedFile(t, other, 2<<20)
	if !highlightClaim(other) {
		t.Fatal("不属于高光目标的文件应放行删除")
	}

	prod := filepath.Join(root, "高光", "a_003_highlight.mp4")
	if err := os.MkdirAll(filepath.Dir(prod), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSizedFile(t, prod, 2<<20)
	if !highlightClaim(prod) {
		t.Fatal("高光产物本身应放行删除")
	}

	if !highlightClaim("") {
		t.Fatal("空路径应放行")
	}
}

// 截图、转换中的分片等非录像切片不该被认领 —— 它们也在同一个主播目录下，
// 但拿去做高光分析只会白跑一次 ffmpeg（"未解析到采样点"），
// 还会把 pipeline 的正常清理拖到分析结束之后。
func TestHighlightClaimIgnoresNonClips(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026-09-22")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	resetHighlightState(t.TempDir())
	defer swapHighlightTargetDirs([]string{filepath.ToSlash(root)})()

	for _, name := range []string{
		"主播_2026-09-22_21-29-57_cover_0001.png",
		"主播_2026-09-22_21-29-57_000.mp4.part",
		"主播_2026-09-22_21-29-57_000_highlight.mp4",
	} {
		p := filepath.Join(day, name)
		writeSizedFile(t, p, 2<<20)
		if !highlightClaim(p) {
			t.Fatalf("%s 不是录像切片，不该被认领", name)
		}
		if _, ok := highlightClaimed.Load(p); ok {
			t.Fatalf("%s 不该进入认领集合", name)
		}
	}

	// 对照：真正的切片必须被认领
	clip := filepath.Join(day, "主播_2026-09-22_21-29-57_000.ts")
	writeSizedFile(t, clip, 2<<20)
	if highlightClaim(clip) {
		t.Fatal("录像切片应被认领")
	}
	highlightDisposeSource(clip)
}

// 已有定论的切片不再认领（避免白占盘）：已产出高光、明确未检出、失败已达重试上限。
// 注意「失败但没到上限」仍要认领 —— 留着重试正是这次修的东西。
func TestHighlightClaimAllowsSettledClips(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026-09-22")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	resetHighlightState(t.TempDir())
	defer swapHighlightTargetDirs([]string{filepath.ToSlash(root)})()
	mk := func(name string) string {
		p := filepath.Join(day, name)
		writeSizedFile(t, p, 2<<20)
		return p
	}

	produced := mk("produced.mp4")
	highlightStateSet(produced, highlightEntry{Output: "produced_highlight.mp4", Segments: 2})
	if !highlightClaim(produced) {
		t.Fatal("已产出高光的切片不应再被认领")
	}

	flat := mk("flat.mp4")
	highlightStateSet(flat, highlightEntry{})
	if !highlightClaim(flat) {
		t.Fatal("明确未检出的切片不应再被认领")
	}

	// 失败但还没到上限 → 仍要认领，留着重试
	retryable := mk("retryable.mp4")
	highlightRecordFailure(retryable, errors.New("io"))
	if highlightClaim(retryable) {
		t.Fatal("失败未达上限的切片应被认领以便重试")
	}
	highlightDisposeSource(retryable)

	// 失败已达上限 → 放弃
	gaveUp := mk("gaveup.mp4")
	for i := 0; i < maxHighlightAttempts; i++ {
		highlightRecordFailure(gaveUp, errors.New("io"))
	}
	if !highlightClaim(gaveUp) {
		t.Fatal("失败已达上限的切片不应再被认领")
	}
}

// 认领超时必须被回收：高光循环若停摆，这些文件会一直占盘。
func TestHighlightReapStaleClaims(t *testing.T) {
	root := t.TempDir()
	resetHighlightState(t.TempDir())
	defer swapHighlightTargetDirs([]string{filepath.ToSlash(root)})()

	dir := filepath.Join(root, "2026-09-22")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "fresh.mp4")
	stale := filepath.Join(dir, "stale.mp4")
	for _, p := range []string{fresh, stale} {
		writeSizedFile(t, p, 2<<20)
	}
	highlightClaimed.Store(fresh, time.Now())
	highlightClaimed.Store(stale, time.Now().Add(-highlightClaimTTL-time.Minute))
	defer highlightClaimed.Delete(fresh)

	highlightReapStaleClaims()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("超时认领的文件应被清理，stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("未超时的认领不应被动到: %v", err)
	}
	if _, ok := highlightClaimed.Load(fresh); !ok {
		t.Fatal("未超时的认领记录应保留")
	}
}

// findStableClips 必须按 mtime 倒序：最新封口的切片才是还没被上传删掉的，
// 先处理它们才有机会拿到高光。
func TestFindStableClipsOldestFirst(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026-09-22")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	// 故意让文件名顺序与时间顺序相反：a_001 最旧、a_003 最新
	times := map[string]time.Duration{
		"a_001.ts": -30 * time.Minute,
		"a_002.ts": -20 * time.Minute,
		"a_003.ts": -10 * time.Minute,
	}
	for name, ago := range times {
		p := filepath.Join(day, name)
		writeSizedFile(t, p, 2<<20)
		ts := time.Now().Add(ago)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	got := findStableClips(root)
	// 正序：最老的排最前 —— 先录的先分析，才不会被新片插队饿死
	want := []string{"a_001.ts", "a_002.ts", "a_003.ts"}
	if len(got) != len(want) {
		t.Fatalf("应返回 %d 个切片，实际 %d 个: %v", len(want), len(got), got)
	}
	for i := range want {
		if filepath.Base(got[i]) != want[i] {
			t.Fatalf("第 %d 个应为 %s，实际 %s（完整结果 %v）", i, want[i], filepath.Base(got[i]), got)
		}
	}
}

// 失败不应是一次性死判：切片可能只是撞上了转换进程在读同一个文件、
// 或磁盘 IO 抖动，重试就能过。线上就有一个完全正常的切片因一次这样的失败被永久跳过。
func TestHighlightFailureRetriesThenGivesUp(t *testing.T) {
	resetHighlightState(t.TempDir())
	src := filepath.Join(t.TempDir(), "2026-09-22", "a_001.mp4")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSizedFile(t, src, 2<<20)

	if !highlightCanRetry(src) {
		t.Fatal("从未分析过的切片应可重试")
	}

	for i := 1; i <= maxHighlightAttempts; i++ {
		if n := highlightRecordFailure(src, errors.New("boom")); n != i {
			t.Fatalf("第 %d 次失败应记 Attempts=%d，实际 %d", i, i, n)
		}
		if i < maxHighlightAttempts && !highlightCanRetry(src) {
			t.Fatalf("第 %d 次失败后仍应可重试", i)
		}
	}
	if highlightCanRetry(src) {
		t.Fatalf("失败达到 %d 次后不应再重试", maxHighlightAttempts)
	}

	e, _ := highlightStateGet(src)
	if e.Attempts != maxHighlightAttempts {
		t.Fatalf("Attempts 应累计到 %d，实际 %d（被覆盖式写入吃掉了？）",
			maxHighlightAttempts, e.Attempts)
	}
	if e.Err == "" {
		t.Fatal("应保留失败原因")
	}
}

// 有定论的切片不再重试：已产出高光、明确未检出。
func TestHighlightCanRetrySemantics(t *testing.T) {
	resetHighlightState(t.TempDir())
	dir := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(dir, "2026-09-22", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		writeSizedFile(t, p, 2<<20)
		return p
	}

	produced := mk("ok.mp4")
	highlightStateSet(produced, highlightEntry{Output: "ok_highlight.mp4", Segments: 2})
	if highlightCanRetry(produced) {
		t.Fatal("已产出高光的切片不应重试")
	}

	flat := mk("flat.mp4")
	highlightStateSet(flat, highlightEntry{})
	if highlightCanRetry(flat) {
		t.Fatal("明确未检出的切片不应重试")
	}

	failed := mk("bad.mp4")
	highlightRecordFailure(failed, errors.New("io"))
	if !highlightCanRetry(failed) {
		t.Fatal("失败一次仍应可重试")
	}
}

// 兜底清理（highlight_source_retention_days）的边界：
// 只删超期的**源片**，未超期的、高光产物、截图归档一律不许动。
// 目录形状是三层：<根>/<主播>/<日期>/<切片>。
func TestHighlightSweepRoots(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "主播甲", "2026-09-01")
	holo := filepath.Join(day, "高光")
	shots := filepath.Join(day, "Screenshots")
	for _, d := range []string{holo, shots} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	mk := func(path string, age time.Duration) {
		if err := os.WriteFile(path, []byte("payload"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	expired := filepath.Join(day, "old_003.ts")
	fresh := filepath.Join(day, "new_003.mp4")
	product := filepath.Join(holo, "old_003_highlight.mp4")
	cover := filepath.Join(shots, "old_cover_0001.png")

	mk(expired, 10*24*time.Hour)
	mk(fresh, time.Hour)
	mk(product, 10*24*time.Hour)
	mk(cover, 10*24*time.Hour)

	removed, freed := highlightSweepRoots([]string{root}, time.Now().AddDate(0, 0, -7))

	if removed != 1 {
		t.Fatalf("期望只删 1 个超期源片，实际删了 %d 个", removed)
	}
	if freed <= 0 {
		t.Fatalf("释放字节数应大于 0，实际 %d", freed)
	}
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("超期源片应被删除: %s", expired)
	}
	for _, keep := range []string{fresh, product, cover} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("不该动这个文件 %s: %v", keep, err)
		}
	}
}

// 多层主播目录都要覆盖：sweep 传的是平台根，必须能走到第二层主播、第三层日期。
func TestHighlightSweepRootsCoversMultipleAnchors(t *testing.T) {
	root := t.TempDir()
	var targets []string
	for _, anchor := range []string{"主播甲", "主播乙"} {
		day := filepath.Join(root, anchor, "2026-08-01")
		if err := os.MkdirAll(day, 0o755); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(day, "a_001.ts")
		if err := os.WriteFile(f, []byte("xx"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-30 * 24 * time.Hour)
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, f)
	}

	removed, _ := highlightSweepRoots([]string{root}, time.Now().AddDate(0, 0, -7))
	if removed != 2 {
		t.Fatalf("两个主播目录下的超期源片都应被删，实际删了 %d 个", removed)
	}
	for _, f := range targets {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("应被删除: %s", f)
		}
	}
}

// 边界：cutoff 早于所有文件时什么都不该删 ——
// 也就是「保留期设得比现有文件都长」这种正常情形，绝不能误伤。
func TestHighlightSweepRootsKeepsFilesNewerThanCutoff(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "主播甲", "2026-09-01")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(day, "a_003.ts")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * 24 * time.Hour)
	if err := os.Chtimes(f, old, old); err != nil {
		t.Fatal(err)
	}

	// cutoff = 200 天前，文件只有 100 天 → 文件比 cutoff 新，必须保留
	removed, _ := highlightSweepRoots([]string{root}, time.Now().AddDate(0, 0, -200))
	if removed != 0 {
		t.Fatalf("cutoff 早于所有文件时不应删除任何文件，实际删了 %d 个", removed)
	}
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("文件不该被删除: %v", err)
	}
}
