package recorder

import "testing"

func TestParseLineBilibili(t *testing.T) {
	isP, platform, room, _, _, fl := ParseLine("https://live.bilibili.com/23058,主播:某主播,录屏:1,截屏:1")
	if isP || platform != "Bilibili" || room != "23058" || fl.Record != true {
		t.Fatalf("bilibili parse: isP=%v platform=%q room=%q flags=%+v", isP, platform, room, fl)
	}

	// b23.tv 短链识别为 Bilibili 平台（分享码即房间号字段，添加入口再解析为真实房间）
	_, platform, room, _, _, _ = ParseLine("https://b23.tv/1Xy2zAb")
	if platform != "Bilibili" || room == "" {
		t.Fatalf("b23.tv parse: platform=%q room=%q", platform, room)
	}
}

func TestParseLineTwitch(t *testing.T) {
	_, platform, room, _, _, _ := ParseLine("https://www.twitch.tv/SomeChannel,主播:老外")
	if platform != "Twitch" || room != "SomeChannel" {
		t.Fatalf("twitch parse: platform=%q room=%q", platform, room)
	}
}

func TestPickBilibiliQN(t *testing.T) {
	full := []int{80, 150, 250, 400, 10000}
	cases := []struct {
		quality string
		want    int
	}{
		{"uhd", 10000},
		{"hd", 250},
		{"sd", 150},
	}
	for _, c := range cases {
		if got := pickBilibiliQN(full, c.quality); got != c.want {
			t.Fatalf("pickBilibiliQN(%v, %q)=%d want %d", full, c.quality, got, c.want)
		}
	}

	// 无原画时 uhd 就近取最高档；目标档缺失时 sd 就近取最低档
	noOriginal := []int{150, 250, 400}
	if got := pickBilibiliQN(noOriginal, "uhd"); got != 400 {
		t.Fatalf("uhd fallback=%d want 400", got)
	}
	noTarget := []int{400, 250, 80}
	if got := pickBilibiliQN(noTarget, "sd"); got != 80 {
		t.Fatalf("sd fallback=%d want 80", got)
	}
}

func TestParseTwitchMasterPlaylist(t *testing.T) {
	master := `#EXTM3U
#EXT-X-VERSION:5
#EXT-X-STREAM-INF:BANDWIDTH=6517113,RESOLUTION=1920x1080,CODECS="avc1.64002A,mp4a.40.2",FRAME-RATE=60.000
https://video-weaver.a.hls.ttvnw.net/v1/playlist/high.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1777965,RESOLUTION=1280x720,CODECS="avc1.64001F,mp4a.40.2",FRAME-RATE=60.000
https://video-weaver.b.hls.ttvnw.net/v1/playlist/mid.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=315972,RESOLUTION=640x360,CODECS="av01.0.04M.08,mp4a.40.2",FRAME-RATE=30.000
https://video-weaver.c.hls.ttvnw.net/v1/playlist/low.m3u8
`
	variants := parseTwitchMasterPlaylist(master)
	if len(variants) != 3 {
		t.Fatalf("variants=%d want 3", len(variants))
	}
	if variants[0].URL != "https://video-weaver.a.hls.ttvnw.net/v1/playlist/high.m3u8" {
		t.Fatalf("first variant=%q", variants[0].URL)
	}

	// pickTwitchVariant 只保留 h264 分档
	if got := pickTwitchVariant(variants, "uhd"); got != "https://video-weaver.a.hls.ttvnw.net/v1/playlist/high.m3u8" {
		t.Fatalf("uhd pick=%q", got)
	}
	if got := pickTwitchVariant(variants, "sd"); got != "https://video-weaver.b.hls.ttvnw.net/v1/playlist/mid.m3u8" {
		t.Fatalf("sd pick=%q (h264-only list)", got)
	}

	// 全部非 h264 时回退为原始列表
	hevcOnly := []twitchVariant{
		{Bandwidth: 6000000, Codecs: "hvc1.1.6.L150.90", URL: "https://weaver/h265-high.m3u8"},
		{Bandwidth: 1000000, Codecs: "hvc1.1.6.L90.90", URL: "https://weaver/h265-low.m3u8"},
	}
	if got := pickTwitchVariant(hevcOnly, "uhd"); got != "https://weaver/h265-high.m3u8" {
		t.Fatalf("hevc fallback pick=%q", got)
	}
}

func TestBilibiliNormalizeRoomID(t *testing.T) {
	if got := bilibiliNormalizeRoomID(" https://live.bilibili.com/h5/23058?spm=x "); got != "23058" {
		t.Fatalf("normalize full url=%q", got)
	}
	if got := bilibiliNormalizeRoomID("23058"); got != "23058" {
		t.Fatalf("normalize plain=%q", got)
	}
}

func TestTwitchAuthTokenExtraction(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		// 裸 OAuth Token
		{"95j2opi8z9ho69m1nglwt2z6b00s3o", "95j2opi8z9ho69m1nglwt2z6b00s3o"},
		// 整串 Cookie：自动提取 auth-token
		{
			"server_session_id=675bbbb6cc2b4f1d9a5525495f2781c8;auth-token=95j2opi8z9ho69m1nglwt2z6b00s3o;api_token=twilight.d4d8ec21e50147c09837d9ba0489ac04;unique_id=qJsm3Sgn4CZxywg2QSpCdbgOB4JmTg2B",
			"95j2opi8z9ho69m1nglwt2z6b00s3o",
		},
		// twilight-user JSON 里是 authToken（无连字符），不得误截
		{
			"twilight-user={%22authToken%22:%22aaabbb%22};auth-token=cccddd",
			"cccddd",
		},
		// k=v 形态但没有 auth-token：放弃登录态
		{"server_session_id=abc;experiment_overrides={}", ""},
	}

	for _, c := range cases {
		builtinCookies = &BuiltinCookieConfig{Twitch: c.in}
		if got := twitchAuthToken(); got != c.want {
			t.Fatalf("twitchAuthToken(%q)=%q want %q", c.in, got, c.want)
		}
	}
	builtinCookies = nil
}
