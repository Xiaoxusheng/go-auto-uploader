package recorder

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakePlatform 最小平台实现：GetStreamURL 恒返回错误，让监控协程停在
// "检测出错 → 定时等待"分支，便于验证停机能否把它叫醒并退出。
type fakePlatform struct{ name string }

func (f *fakePlatform) GetPlatformName() string { return f.name }

func (f *fakePlatform) GetStreamURL(roomID, quality string) (string, string, string, error) {
	return "", "", "", errors.New("test: platform offline")
}

func clearActive() {
	builtinActiveTasks.Range(func(k, _ interface{}) bool {
		builtinActiveTasks.Delete(k)
		return true
	})
}

func clearMap(m *sync.Map) {
	m.Range(func(k, _ interface{}) bool {
		m.Delete(k)
		return true
	})
}

// 停机后必须能等到活跃监控协程清空，否则 os.Exit 会把 ffmpeg 丢成孤儿。
func TestShutdownStopsMonitorGoroutine(t *testing.T) {
	clearActive()
	t.Cleanup(func() {
		clearActive()
		builtinShuttingDown.Store(false)
		clearMap(&builtinTaskStates)
		clearMap(&builtinCancels)
		clearMap(&builtinStatusMap)
	})

	key := "Fake_testroom"
	builtinTaskStates.Store(key, "running")
	wrapperStartMonitorIfNotRunning(&fakePlatform{name: "Fake"}, "testroom")

	// 等监控协程注册进活跃表，并等它把 cancel 放进 builtinCancels
	// （activeTasks 在 go 之前就 Store，比 cancel 注册更早，所以两步都要等）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n := 0
		builtinCancels.Range(func(_, _ interface{}) bool { n++; return true })
		if ActiveTaskCount() > 0 && n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := ActiveTaskCount(); n == 0 {
		t.Fatal("监控协程未启动（活跃表为空）")
	}

	if n := StopAllRecordings(); n == 0 {
		t.Error("StopAllRecordings 应至少通知 1 路会话")
	}
	if !WaitActiveTasks(5 * time.Second) {
		t.Fatalf("停机后监控协程应在超时前退出，当前仍活跃 %d 路", ActiveTaskCount())
	}
	if n := ActiveTaskCount(); n != 0 {
		t.Errorf("停机后活跃任务应为 0，得到 %d", n)
	}
}

func TestActiveTaskCountAndWait(t *testing.T) {
	clearActive()
	t.Cleanup(clearActive)

	if n := ActiveTaskCount(); n != 0 {
		t.Fatalf("初始应为 0，得到 %d", n)
	}
	builtinActiveTasks.Store("k1", true)
	builtinActiveTasks.Store("k2", true)
	if n := ActiveTaskCount(); n != 2 {
		t.Fatalf("应为 2，得到 %d", n)
	}

	// 仍有活跃任务 → 必须等到超时才返回 false
	start := time.Now()
	if WaitActiveTasks(300 * time.Millisecond) {
		t.Fatal("仍有活跃任务，不应返回 true")
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("应等满 timeout，实际只等了 %s", d)
	}

	// 清空后应立即返回 true
	clearActive()
	start = time.Now()
	if !WaitActiveTasks(2 * time.Second) {
		t.Fatal("已清空，应返回 true")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("清空后应立即返回，实际等了 %s", d)
	}
}

func TestStopAllRecordingsMarksAndCancels(t *testing.T) {
	clearActive()
	t.Cleanup(func() {
		clearActive()
		builtinShuttingDown.Store(false)
		clearMap(&builtinCancels)
	})

	if builtinShuttingDown.Load() {
		t.Fatal("前置：停机标记应为 false")
	}

	// 用真实的 context.WithCancel 产出的 CancelFunc（命名类型 context.CancelFunc），
	// 与生产代码 builtinCancels 里存的值同型。
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	builtinCancels.Store("s1", cancel1)
	builtinCancels.Store("s2", cancel2)

	if n := StopAllRecordings(); n != 2 {
		t.Errorf("应通知 2 路，得到 %d", n)
	}
	if !builtinShuttingDown.Load() {
		t.Error("停机标记必须置位——否则监控协程会在 RecordStream 收尾后重开录制")
	}
	if ctx1.Err() == nil || ctx2.Err() == nil {
		t.Errorf("两个会话的 ctx 都应被取消: ctx1=%v ctx2=%v", ctx1.Err(), ctx2.Err())
	}
}
