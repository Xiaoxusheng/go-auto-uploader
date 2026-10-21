package recorder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ===== analyzeFlawFrame：纯色判定 =====

func solidFrame(r, g, b byte) []byte {
	f := make([]byte, flawProbeFSZ)
	for i := 0; i < flawProbeW*flawProbeH; i++ {
		f[i*3], f[i*3+1], f[i*3+2] = r, g, b
	}
	return f
}

func TestAnalyzeFlawFrameSolidGreen(t *testing.T) {
	g, m := analyzeFlawFrame(solidFrame(0, 255, 0))
	if g < 0.99 {
		t.Fatalf("整帧纯绿应判 green≈1.0，实际 %.3f", g)
	}
	if m != 0 {
		t.Fatalf("纯绿不应计入 magenta，实际 %.3f", m)
	}
}

func TestAnalyzeFlawFrameSolidMagenta(t *testing.T) {
	g, m := analyzeFlawFrame(solidFrame(255, 0, 255))
	if m < 0.99 {
		t.Fatalf("整帧纯紫应判 magenta≈1.0，实际 %.3f", m)
	}
	if g != 0 {
		t.Fatalf("纯紫不应计入 green，实际 %.3f", g)
	}
}

func TestAnalyzeFlawFrameNormalFrame(t *testing.T) {
	// 灰阶渐变：模拟正常画面，两个占比都应为 0
	f := make([]byte, flawProbeFSZ)
	for i := 0; i < flawProbeW*flawProbeH; i++ {
		v := byte((i * 3) % 256)
		f[i*3], f[i*3+1], f[i*3+2] = v, v, v
	}
	if g, m := analyzeFlawFrame(f); g != 0 || m != 0 {
		t.Fatalf("灰阶画面不应命中，实际 green=%.3f magenta=%.3f", g, m)
	}
}

func TestAnalyzeFlawFrameShortBuffer(t *testing.T) {
	if g, m := analyzeFlawFrame([]byte{1, 2, 3}); g != 0 || m != 0 {
		t.Fatalf("缓冲区不足应返回 0,0，实际 %.3f,%.3f", g, m)
	}
}

// ===== readTail =====

func TestReadTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readTail(p, 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "6789" {
		t.Fatalf("应取尾部 4 字节，实际 %q", got)
	}
	got, err = readTail(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "0123456789" {
		t.Fatalf("文件小于 max 时应读全量，实际 %q", got)
	}
}

func TestReadTailMissingFile(t *testing.T) {
	if _, err := readTail(filepath.Join(t.TempDir(), "nope"), 16); err == nil {
		t.Fatal("文件不存在应返回错误")
	}
}

// ===== latestBuiltinSegment =====

func TestLatestBuiltinSegment(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)

	write := func(p string, n int, mt time.Time) {
		t.Helper()
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	prefix := "a_20260101_000000"
	oldSeg := filepath.Join(dir, prefix+"_000.ts")
	newSeg := filepath.Join(dir, prefix+"_001.ts")
	write(oldSeg, 2048, base)
	write(newSeg, 2048, base.Add(time.Minute))

	// 以下三个都必须被排除
	write(filepath.Join(dir, prefix+"_002.ts.flaw"), 2048, base.Add(2*time.Minute)) // 坏分片后缀
	write(filepath.Join(dir, prefix+"_003.ts"), 100, base.Add(3*time.Minute))       // 小于 minSize
	write(filepath.Join(dir, "b_20260101_000000_000.ts"), 2048, base.Add(4*time.Minute))

	if got := latestBuiltinSegment(dir, prefix, 1024); got != newSeg {
		t.Fatalf("应返回 %s，实际 %s", newSeg, got)
	}
}

func TestLatestBuiltinSegmentEmptyDir(t *testing.T) {
	if got := latestBuiltinSegment(t.TempDir(), "x", 1); got != "" {
		t.Fatalf("空目录应返回空串，实际 %s", got)
	}
}

// ===== markBadSegment =====

func TestMarkBadSegment(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "seg.ts")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	markBadSegment(p)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("原文件应已改名")
	}
	if _, err := os.Stat(p + ".flaw"); err != nil {
		t.Fatalf("应生成 .flaw 文件: %v", err)
	}
	// 空路径与不存在的文件都必须静默返回，不能 panic
	markBadSegment("")
	markBadSegment(filepath.Join(dir, "nope.ts"))
}

// ===== 标记与单场计数 =====

func TestFlawRestartFlag(t *testing.T) {
	key := "Test_flawrestart"
	if clearFlawRestart(key) {
		t.Fatal("未标记时不应返回 true")
	}
	markFlawRestart(key)
	if !clearFlawRestart(key) {
		t.Fatal("标记后应返回 true")
	}
	if clearFlawRestart(key) {
		t.Fatal("标记已被消费，不应再次返回 true")
	}
}

func TestFlawCountLifecycle(t *testing.T) {
	key := "Test_flawcount"
	resetFlawCount(key)
	if n := flawCount(key); n != 0 {
		t.Fatalf("初始应为 0，实际 %d", n)
	}
	setFlawCount(key, 2)
	if n := flawCount(key); n != 2 {
		t.Fatalf("应为 2，实际 %d", n)
	}
	resetFlawCount(key)
	if n := flawCount(key); n != 0 {
		t.Fatalf("复位后应为 0，实际 %d", n)
	}
}

// ===== probeChromaFlaw 端到端（需要 ffmpeg）=====

// TestProbeChromaFlawEndToEnd 用真实 ffmpeg 生成样本，验证端到端判定。
// ffmpeg 路径优先取 FLAW_TEST_FFMPEG 环境变量，其次 PATH；都没有则跳过。
func TestProbeChromaFlawEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要 ffmpeg 的端到端测试")
	}
	ff := os.Getenv("FLAW_TEST_FFMPEG")
	if ff == "" {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ff = p
		}
	}
	if ff == "" {
		t.Skip("未找到 ffmpeg（可设 FLAW_TEST_FFMPEG 指定路径），跳过")
	}

	old := builtinFfmpegPath
	builtinFfmpegPath = ff
	defer func() { builtinFfmpegPath = old }()

	dir := t.TempDir()

	// 生成样本：加噪是为了让无损编码真的产出 >512KB 的分片，
	// 否则纯色画面会被 x264 压到几十 KB，触发 flawMinSegBytes 跳过逻辑。
	gen := func(name, src, vf string) string {
		t.Helper()
		out := filepath.Join(dir, name)
		args := []string{"-y", "-v", "error", "-f", "lavfi", "-i", src}
		if vf != "" {
			args = append(args, "-vf", vf)
		}
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "2M",
			"-pix_fmt", "yuv420p", out)
		if b, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
			t.Skipf("生成样本 %s 失败（ffmpeg 能力不足？）: %v | %s", name, err, b)
		}
		st, err := os.Stat(out)
		if err != nil {
			t.Skipf("样本 %s 不存在: %v", name, err)
		}
		if st.Size() < flawMinSegBytes {
			t.Skipf("样本 %s 只有 %d 字节，未达 %d 检测下限", name, st.Size(), flawMinSegBytes)
		}
		return out
	}

	// 1) 整屏绿屏 → 必须判花屏
	greenSeg := gen("green.ts", "color=c=0x00FF00:s=640x360:d=6", "noise=alls=20:allf=t")
	v := probeChromaFlaw(greenSeg, flawProbeFrames, 0.9)
	if v.Err != nil {
		t.Fatalf("绿屏样本探测出错: %v", v.Err)
	}
	if !v.Flawed {
		t.Fatalf("整屏绿屏应判花屏，实际 %s", v)
	}

	// 2) 正常画面（testsrc2 滚动图案）→ 不得误报
	normalSeg := gen("normal.ts", "testsrc2=s=640x360:d=6:rate=25", "")
	nv := probeChromaFlaw(normalSeg, flawProbeFrames, 0.9)
	if nv.Err != nil {
		t.Fatalf("正常样本探测出错: %v", nv.Err)
	}
	if nv.Flawed {
		t.Fatalf("正常画面不应误报花屏，实际 %s", nv)
	}

	// 3) 不存在的文件 → 返回错误而非 panic
	if r := probeChromaFlaw(filepath.Join(dir, "nope.ts"), 3, 0.9); r.Err == nil {
		t.Fatal("不存在的分片应返回错误")
	}
}

// ===== 录制链路集成：检测到花屏 → 打断 → 标记坏分片 =====

// TestRecordStreamRestartsOnChromaFlaw 直接驱动 RecordStream 拉一条本地限速的
// 「绿屏直播流」，验证整条链路：检测命中 → 设置重录标记 → 打断会话 → 坏分片改名。
//
// 这是唯一能覆盖「检测 ticker 是否真的触发」「坏分片是否在正确时机改名」的测试，
// 纯函数测试覆盖不到。需要 ffmpeg（可设 FLAW_TEST_FFMPEG），short 模式跳过。
func TestRecordStreamRestartsOnChromaFlaw(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过录制集成测试")
	}
	ff := os.Getenv("FLAW_TEST_FFMPEG")
	if ff == "" {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ff = p
		}
	}
	if ff == "" {
		t.Skip("未找到 ffmpeg（可设 FLAW_TEST_FFMPEG），跳过")
	}

	oldFF := builtinFfmpegPath
	builtinFfmpegPath = ff
	defer func() { builtinFfmpegPath = oldFF }()

	oldCfg := *Config()
	defer SetConfig(&oldCfg)

	dir := t.TempDir()

	// 生成 180 秒绿屏源，作为「一直花屏的直播流」。
	// 源要足够长：会话持续时间 = 源时长 ÷ 限速发送速度，检测每 10s 一轮，
	// 需要多轮机会才能在 CPU 被其他包测试抢占时仍然命中。
	src := filepath.Join(dir, "src_green.ts")
	genArgs := []string{"-y", "-v", "error",
		"-f", "lavfi", "-i", "color=c=0x00FF00:s=320x180:d=180",
		"-vf", "noise=alls=20:allf=t",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "1M",
		"-pix_fmt", "yuv420p", "-f", "mpegts", src}
	if b, err := exec.Command(ff, genArgs...).CombinedOutput(); err != nil {
		t.Skipf("生成源样本失败: %v | %s", err, b)
	}
	tsData, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	// 限速 HTTP 服务器：不限速的话 ffmpeg 几秒就把整段拉完，来不及检测。
	// 64KB/100ms ≈ 5Mbps，比真实直播略高，保证文件快速攒够检测下限。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		fl, _ := w.(http.Flusher)
		const chunk = 64 * 1024
		for i := 0; i < len(tsData); i += chunk {
			end := i + chunk
			if end > len(tsData) {
				end = len(tsData)
			}
			if _, werr := w.Write(tsData[i:end]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()

	recRoot := filepath.Join(dir, "rec")
	if err := os.MkdirAll(recRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// 检测间隔压到下限 10s，让测试在 ~15s 内出结果
	SetConfig(&BuiltinConfig{
		Quality:            "sd",
		SavePath:           recRoot,
		ScreenshotInterval: 20,
		FlawDetectEnable:   true,
		FlawDetectAction:   "restart",
		FlawDetectInterval: 10,
		FlawDetectRatio:    0.9,
		FlawMaxRestarts:    1,
	})

	const key = "TestFlawRoom_room1"
	resetFlawCount(key)
	_ = clearFlawRestart(key)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	flags := DefaultFlags()
	flags.Record = true
	flags.Screenshot = false

	done := make(chan struct{})
	start := time.Now()
	go func() {
		RecordStream(ctx, srv.URL+"/live.ts", "TestFlawRoom", "room1", "tester", "", "sd", 0, flags)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(110 * time.Second):
		cancel()
		<-done
		t.Fatal("RecordStream 未在 110 秒内返回（花屏检测没有触发打断）")
	}
	elapsed := time.Since(start)

	if !clearFlawRestart(key) {
		// 诊断：手动探测录制文件，区分「文件内容其实正常」与「探测链路失败」
		var seg string
		_ = filepath.Walk(recRoot, func(p string, info os.FileInfo, werr error) error {
			if werr == nil && !info.IsDir() && strings.HasSuffix(p, ".ts") {
				seg = p
			}
			return nil
		})
		if seg != "" {
			st, _ := os.Stat(seg)
			var size int64
			if st != nil {
				size = st.Size()
			}
			v := probeChromaFlaw(seg, flawProbeFrames, 0.9)
			t.Logf("诊断：录制文件 %s 大小=%d 手动探测=%s", filepath.Base(seg), size, v.String())
		} else {
			t.Logf("诊断：未找到录制文件（目录 %s）", recRoot)
		}
		t.Fatalf("应设置「花屏重录」标记（耗时 %s）", elapsed)
	}
	if n := flawCount(key); n != 1 {
		t.Fatalf("本场重录计数应为 1，实际 %d", n)
	}

	found := false
	_ = filepath.Walk(recRoot, func(p string, info os.FileInfo, werr error) error {
		if werr == nil && !info.IsDir() && strings.HasSuffix(p, ".ts.flaw") {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatalf("应存在被标记的 .ts.flaw 坏分片（耗时 %s）", elapsed)
	}
	t.Logf("链路验证通过：耗时 %s，坏分片已标记", elapsed)
}
