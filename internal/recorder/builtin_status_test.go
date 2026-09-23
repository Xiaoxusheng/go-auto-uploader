package recorder

import (
	"testing"
	"time"
)

// builtinDurationOf 从任务快照里取出某房间下发给前端的 duration 字段。
func builtinDurationOf(t *testing.T, platform, roomID string) string {
	t.Helper()
	for _, task := range GetBuiltinRecorderTasks() {
		if task.Platform == platform && task.RoomID == roomID {
			return task.Duration
		}
	}
	t.Fatalf("任务快照里没有 %s_%s", platform, roomID)
	return ""
}

// 回归：处于「录制中」的任务必须始终带得出时长。
//
// 曾经的故障链：updateBuiltinStatus 命中 3 分钟开播防抖时直接沿用
// oldTask.startTime，而条目若刚被删过重建（热重载剔除后重加、删除后重加），
// 旧快照的 startTime 就是零值 → 零值被一路继承 → GetBuiltinRecorderTasks
// 判定 startTime 为零 → 下发 duration="-" → 前端「REC · 时长」恒显示 --:--，
// 只有重启进程（状态表是内存态）才恢复。
func TestLiveStatusKeepsStartTimeAcrossReentry(t *testing.T) {
	const platform, room = "Douyin", "regression_reentry"
	key := platform + "_" + room

	cleanup := func() {
		builtinStatusMap.Delete(key)
		builtinTaskStates.Delete(key)
		clearBuiltinDebounce(key)
	}
	cleanup()
	defer cleanup()

	builtinTaskStates.Store(key, "running")

	// 1) 首次开录：起点应当被记录下来
	updateBuiltinStatus(platform, room, "回归主播", "", "uhd", "监控中")
	updateBuiltinStatus(platform, room, "回归主播", "", "uhd", "录制中")
	if d := builtinDurationOf(t, platform, room); d == "-" {
		t.Fatalf("首次开录就没拿到时长，duration=%q", d)
	}

	// 2) 关键场景：条目被删掉重建，且防抖记录仍在 3 分钟窗口内。
	//    这里刻意不走删除入口，直接构造出「零值起点 + 残留防抖」的旧状态。
	builtinStatusMap.Delete(key)
	updateBuiltinStatus(platform, room, "回归主播", "", "uhd", "监控中")
	builtinNotifyDebounce.Store("live_"+key, time.Now())

	// 3) 3 分钟内重新开录：防抖命中，走的是「静默恢复」分支
	updateBuiltinStatus(platform, room, "回归主播", "", "uhd", "录制中")

	snap, ok := builtinStatusMap.Load(key)
	if !ok {
		t.Fatal("状态条目应当存在")
	}
	if snap.(*BuiltinTaskStatus).startTime.IsZero() {
		t.Error("静默恢复分支把零值起点继承了下来：startTime 仍为零值")
	}
	if d := builtinDurationOf(t, platform, room); d == "-" {
		t.Errorf("录制中却下发了 duration=%q，前端会显示 --:--", d)
	}
}

// 回归：恢复监控不得覆盖「正在录制」的真实状态。
//
// 曾经的故障链：resume / resume_all / 热重载取消 # 三处都无条件把状态写成「监控中」，
// 而 wrapperStartMonitorIfNotRunning 看到监控协程已在运行会直接 return（不重启），
// 于是 ffmpeg 还在录、本场 TS 还在涨，控制台却显示 IDLE、时长掉成 --:--，
// 并且因为 RecordStream 只在启动录制时写一次状态，这个错误会一直卡到本场录制结束。
func TestResumeKeepsLiveStatus(t *testing.T) {
	cases := []struct{ cur, want string }{
		{"已暂停", "监控中"},
		{"", "监控中"},
		{"录制中", "录制中"},
		{"截屏中", "截屏中"},
		{"未开播等待中", "未开播等待中"},
	}
	for _, c := range cases {
		if got := resumeStatusAfterUnpause(c.cur); got != c.want {
			t.Errorf("resumeStatusAfterUnpause(%q) = %q, 期望 %q", c.cur, got, c.want)
		}
	}
}

// 端到端：一条正在录制的任务被 resume 之后，状态与时长都必须保持有效。
func TestResumeOnRecordingTaskKeepsDuration(t *testing.T) {
	const platform, room = "Douyin", "regression_resume"
	key := platform + "_" + room

	cleanup := func() {
		builtinStatusMap.Delete(key)
		builtinTaskStates.Delete(key)
		clearBuiltinDebounce(key)
	}
	cleanup()
	defer cleanup()

	builtinTaskStates.Store(key, "running")
	updateBuiltinStatus(platform, room, "回归主播", "", "uhd", "监控中")
	updateBuiltinStatus(platform, room, "回归主播", "", "uhd", "录制中")
	if d := builtinDurationOf(t, platform, room); d == "-" {
		t.Fatalf("录制中却没拿到时长，duration=%q", d)
	}

	// 复现 resume 分支的写法：值拷贝 → 状态回落 → 回存
	existing, ok := builtinStatusMap.Load(key)
	if !ok {
		t.Fatal("状态条目应当存在")
	}
	task := *(existing.(*BuiltinTaskStatus))
	task.IsPaused = false
	task.Status = resumeStatusAfterUnpause(task.Status)
	builtinStatusMap.Store(key, &task)

	snap, _ := builtinStatusMap.Load(key)
	if got := snap.(*BuiltinTaskStatus).Status; got != "录制中" {
		t.Errorf("resume 之后状态变成了 %q，应当保持「录制中」", got)
	}
	if d := builtinDurationOf(t, platform, room); d == "-" {
		t.Errorf("resume 之后下发了 duration=%q，前端会显示 --:--", d)
	}
}

// 回归：任务条目被移除时必须顺手清掉上下播防抖记录，
// 否则同一主播在 3 分钟内被重新加入并开播，会被误判成同一次直播的静默重连。
func TestClearBuiltinDebounceOnTaskRemoval(t *testing.T) {
	const platform, room = "Douyin", "regression_debounce"
	key := platform + "_" + room

	builtinNotifyDebounce.Store("live_"+key, time.Now())
	builtinNotifyDebounce.Store("offline_"+key, time.Now())
	defer clearBuiltinDebounce(key)

	clearBuiltinDebounce(key)

	if _, ok := builtinNotifyDebounce.Load("live_" + key); ok {
		t.Error("live_ 防抖记录应当被清理")
	}
	if _, ok := builtinNotifyDebounce.Load("offline_" + key); ok {
		t.Error("offline_ 防抖记录应当被清理")
	}
	// 不应误伤其他任务的记录
	otherKey := platform + "_other_room"
	builtinNotifyDebounce.Store("live_"+otherKey, time.Now())
	defer builtinNotifyDebounce.Delete("live_" + otherKey)
	clearBuiltinDebounce(key)
	if _, ok := builtinNotifyDebounce.Load("live_" + otherKey); !ok {
		t.Error("清理不应影响其他任务的防抖记录")
	}
}
