package recorder

import "testing"

func TestParseFlagsFromLine(t *testing.T) {
	cases := []struct {
		in   string
		rec  bool
		shot bool
	}{
		{"https://live.douyin.com/1,主播:甲", true, true},
		{"https://live.douyin.com/1,主播:乙,录屏:0,截屏:1", false, true},
		{"#https://live.douyin.com/1,主播:丙,录屏:1,截屏:0", true, false},
		{"https://live.douyin.com/1,别名,录屏:0,截屏:1", false, true},
	}
	for _, c := range cases {
		_, _, room, name, _, fl := ParseLine(c.in)
		if fl.Record != c.rec || fl.Screenshot != c.shot {
			t.Fatalf("%q flags=%+v", c.in, fl)
		}
		if room == "" {
			t.Fatalf("%q empty room", c.in)
		}
		_ = name
	}
}

func TestRebuildLineWithFlags(t *testing.T) {
	in := "#https://live.douyin.com/444,主播:丁,录屏:1,截屏:0"
	out := RebuildLineWithFlags(in, TaskFlags{Record: false, Screenshot: true})
	if out != "#https://live.douyin.com/444,主播:丁,录屏:0,截屏:1" {
		t.Fatalf("rebuild=%q", out)
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
