package recorder

import "testing"

func TestParseFlagsFromLine(t *testing.T) {
	cases := []struct {
		in       string
		rec      bool
		shot     bool
		interval int
	}{
		{"https://live.douyin.com/1,主播:甲", true, true, 0},
		{"https://live.douyin.com/1,主播:乙,录屏:0,截屏:1", false, true, 0},
		{"#https://live.douyin.com/1,主播:丙,录屏:1,截屏:0", true, false, 0},
		{"https://live.douyin.com/1,别名,录屏:0,截屏:1", false, true, 0},
		{"https://live.douyin.com/1,主播:丁,录屏:1,截屏:1,截图间隔:45", true, true, 45},
		{"https://live.douyin.com/1,截图间隔:abc", true, true, 0},
		{"https://live.douyin.com/1,截图间隔:-5", true, true, 0},
	}
	for _, c := range cases {
		_, _, room, name, _, fl := ParseLine(c.in)
		if fl.Record != c.rec || fl.Screenshot != c.shot || fl.ShotInterval != c.interval {
			t.Fatalf("%q flags=%+v", c.in, fl)
		}
		if fl.Watermark != 0 {
			t.Fatalf("%q watermark=%d, want 0(跟随全局)", c.in, fl.Watermark)
		}
		if room == "" {
			t.Fatalf("%q empty room", c.in)
		}
		_ = name
	}
}

func TestParseWatermarkFlag(t *testing.T) {
	cases := []struct {
		in string
		wm int
	}{
		{"https://live.douyin.com/1,主播:甲", 0},
		{"https://live.douyin.com/1,水印:1", 1},
		{"https://live.douyin.com/1,水印:0", 2},
		{"https://live.douyin.com/1,水印:2", 0},
		{"https://live.douyin.com/1,水印:abc", 0},
		{"#https://live.douyin.com/1,主播:乙,录屏:0,截屏:1,水印:0,截图间隔:30", 2},
	}
	for _, c := range cases {
		_, _, room, _, _, fl := ParseLine(c.in)
		if fl.Watermark != c.wm {
			t.Fatalf("%q watermark=%d, want %d", c.in, fl.Watermark, c.wm)
		}
		if room == "" {
			t.Fatalf("%q empty room", c.in)
		}
	}
}

func TestRebuildWatermarkFlag(t *testing.T) {
	in := "https://live.douyin.com/444,主播:丁,录屏:1,截屏:1"

	// 强制开写回
	out1 := RebuildLineWithFlags(in, TaskFlags{Record: true, Screenshot: true, Watermark: 1})
	if out1 != "https://live.douyin.com/444,主播:丁,录屏:1,截屏:1,水印:1" {
		t.Fatalf("rebuild wm1=%q", out1)
	}
	// 强制关写回（内部 2 对应行内 水印:0）
	out0 := RebuildLineWithFlags(in, TaskFlags{Record: true, Screenshot: true, Watermark: 2})
	if out0 != "https://live.douyin.com/444,主播:丁,录屏:1,截屏:1,水印:0" {
		t.Fatalf("rebuild wm0=%q", out0)
	}
	// 跟随全局时去掉旧后缀；零值 TaskFlags（未显式构造）同样安全
	in2 := "https://live.douyin.com/444,主播:丁,录屏:1,截屏:1,水印:0"
	out2 := RebuildLineWithFlags(in2, TaskFlags{Record: true, Screenshot: true})
	if out2 != "https://live.douyin.com/444,主播:丁,录屏:1,截屏:1" {
		t.Fatalf("rebuild wmFollow=%q", out2)
	}
}

func TestRebuildLineWithFlags(t *testing.T) {
	in := "#https://live.douyin.com/444,主播:丁,录屏:1,截屏:0"
	out := RebuildLineWithFlags(in, TaskFlags{Record: false, Screenshot: true})
	if out != "#https://live.douyin.com/444,主播:丁,录屏:0,截屏:1" {
		t.Fatalf("rebuild=%q", out)
	}

	// 显式间隔写回后缀
	out2 := RebuildLineWithFlags(in, TaskFlags{Record: false, Screenshot: true, ShotInterval: 45})
	if out2 != "#https://live.douyin.com/444,主播:丁,录屏:0,截屏:1,截图间隔:45" {
		t.Fatalf("rebuild2=%q", out2)
	}

	// 间隔归零（跟随全局）时应去掉旧后缀
	in3 := "https://live.douyin.com/444,主播:丁,录屏:1,截屏:1,截图间隔:45"
	out3 := RebuildLineWithFlags(in3, TaskFlags{Record: true, Screenshot: true})
	if out3 != "https://live.douyin.com/444,主播:丁,录屏:1,截屏:1" {
		t.Fatalf("rebuild3=%q", out3)
	}
}

func TestParseQualityAndMaxDurationFlags(t *testing.T) {
	cases := []struct {
		in          string
		quality     string
		maxDuration int
	}{
		{"https://live.douyin.com/1,主播:甲", "", 0},
		{"https://live.douyin.com/1,画质:uhd", "uhd", 0},
		{"https://live.douyin.com/1,画质:hd,录制时长:120", "hd", 120},
		{"https://live.douyin.com/1,主播:乙,录制时长:30,画质:sd", "sd", 30},
		{"https://live.douyin.com/1,画质:4k", "", 0},    // 非法档位忽略
		{"https://live.douyin.com/1,录制时长:abc", "", 0}, // 非法数值忽略
		{"https://live.douyin.com/1,录制时长:-5", "", 0},  // 负数忽略
		{"https://live.douyin.com/1,录制时长:0", "", 0},   // 0 = 不限制
		{"#https://live.douyin.com/1,录屏:0,画质:hd,录制时长:60", "hd", 60},
	}
	for _, c := range cases {
		_, _, room, _, _, fl := ParseLine(c.in)
		if fl.Quality != c.quality || fl.MaxDuration != c.maxDuration {
			t.Fatalf("%q quality=%q maxDuration=%d, want %q/%d", c.in, fl.Quality, fl.MaxDuration, c.quality, c.maxDuration)
		}
		if room == "" {
			t.Fatalf("%q empty room", c.in)
		}
	}
}

func TestRebuildQualityAndMaxDurationFlags(t *testing.T) {
	in := "https://live.douyin.com/444,主播:丁,录屏:1,截屏:1"

	// 覆盖画质 + 时长写回
	out1 := RebuildLineWithFlags(in, TaskFlags{Record: true, Screenshot: true, Quality: "hd", MaxDuration: 90})
	if out1 != "https://live.douyin.com/444,主播:丁,录屏:1,截屏:1,画质:hd,录制时长:90" {
		t.Fatalf("rebuild q+md=%q", out1)
	}

	// 回到跟随全局/不限制时旧后缀应被剥掉
	in2 := "https://live.douyin.com/444,主播:丁,录屏:1,截屏:1,画质:hd,录制时长:90"
	out2 := RebuildLineWithFlags(in2, TaskFlags{Record: true, Screenshot: true})
	if out2 != "https://live.douyin.com/444,主播:丁,录屏:1,截屏:1" {
		t.Fatalf("rebuild follow=%q", out2)
	}

	// 带暂停前缀与截图间隔时写回位置正确
	in3 := "#https://live.douyin.com/444,主播:丁,截图间隔:30"
	out3 := RebuildLineWithFlags(in3, TaskFlags{Record: true, Screenshot: true, ShotInterval: 30, Quality: "sd", MaxDuration: 45})
	if out3 != "#https://live.douyin.com/444,主播:丁,录屏:1,截屏:1,截图间隔:30,画质:sd,录制时长:45" {
		t.Fatalf("rebuild3=%q", out3)
	}

	// 写回结果必须能原样解析回同样的 flags（往返一致）
	_, _, _, _, _, fl := ParseLine(out3)
	if fl.Quality != "sd" || fl.MaxDuration != 45 || fl.ShotInterval != 30 || !fl.Record || !fl.Screenshot {
		t.Fatalf("roundtrip flags=%+v", fl)
	}
}

func TestExtractRoomID(t *testing.T) {
	if ExtractRoomID("https://live.douyin.com/12345") != "12345" {
		t.Fatal("douyin room")
	}
	if ExtractRoomID("https://play.sooplive.co.kr/foo/0") != "foo" {
		t.Fatal("soop channel")
	}
}

func TestIsLiveStatus(t *testing.T) {
	if !IsLiveStatus("录制中") || !IsLiveStatus("截屏中") || IsLiveStatus("监控中") {
		t.Fatal("live status")
	}
}
