package recorder

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---- __INITIAL_STATE__ 提取与解析 ----

const ksStatePrefix = `<html><script>window.__INITIAL_STATE__=`

func TestParseKuaishouPageState_Offline(t *testing.T) {
	html := ksStatePrefix + `{"liveroom":{"playList":[{"liveStream":{},"author":{},"gameInfo":{},"isLiving":false}]},"pcConfig":{}};(function(){var s;})();`
	state, err := parseKuaishouPageState(html)
	if err != nil {
		t.Fatalf("未开播页面解析失败: %v", err)
	}
	if len(state.Liveroom.PlayList) != 1 {
		t.Fatalf("playList 数量 = %d, 期望 1", len(state.Liveroom.PlayList))
	}
	if state.Liveroom.PlayList[0].IsLiving {
		t.Fatalf("isLiving = true, 期望 false")
	}
}

func TestParseKuaishouPageState_RateLimited(t *testing.T) {
	html := ksStatePrefix + `{"liveroom":{"playList":[{"liveStream":{},"author":{},"isLiving":false,"errorType":{"type":2,"title":"请求过快，请稍后重试","content":"浏览其他内容","url":"/"}}]}};(function(){var s;})();`
	state, err := parseKuaishouPageState(html)
	if err != nil {
		t.Fatalf("限频页面解析失败: %v", err)
	}
	item := state.Liveroom.PlayList[0]
	if item.ErrorType == nil {
		t.Fatalf("errorType 缺失，限频语义丢失")
	}
	if !strings.Contains(item.ErrorType.Title, "请求过快") {
		t.Fatalf("errorType.Title = %q", item.ErrorType.Title)
	}
}

func TestParseKuaishouPageState_Live(t *testing.T) {
	reps := `[{"qualityLevel":1,"qualityType":"ORIGIN","qualityLabel":"蓝光","status":1,"bitrate":4000,"url":"https://flv.example/x_4000.flv?sign=a"},{"qualityLevel":3,"qualityType":"HD1","qualityLabel":"高清","status":1,"bitrate":1000,"url":"https://flv.example/x_1000.flv?sign=b"},{"qualityLevel":4,"qualityType":"SD1","qualityLabel":"标清","status":1,"bitrate":600,"url":"https://flv.example/x_600.flv?sign=c"}]`
	html := ksStatePrefix + `{"liveroom":{"playList":[{"liveStream":{"living":true,"poster":"https:\/\/cover.example\/p.jpg","playUrls":{"h264":{"adaptationSet":{"representation":` + reps + `}}}},"author":{"name":"测试主播","headUrl":"https:\/\/avatar.example\/a.jpg"},"isLiving":true}]}};(function(){var s;})();`
	state, err := parseKuaishouPageState(html)
	if err != nil {
		t.Fatalf("开播页面解析失败: %v", err)
	}
	item := state.Liveroom.PlayList[0]
	if item.Author.Name != "测试主播" {
		t.Fatalf("author.name = %q", item.Author.Name)
	}
	if item.LiveStream.Poster != "https://cover.example/p.jpg" {
		t.Fatalf("poster 转义还原失败: %q", item.LiveStream.Poster)
	}
	repsGot := parseKuaishouPlayUrls(item.LiveStream.PlayUrls)
	if len(repsGot) != 3 {
		t.Fatalf("representation 数量 = %d, 期望 3", len(repsGot))
	}
	if got := kuaishouPickRepresentation(repsGot, "uhd"); got != "https://flv.example/x_4000.flv?sign=a" {
		t.Fatalf("uhd 选路 = %q", got)
	}
	if got := kuaishouPickRepresentation(repsGot, "hd"); got != "https://flv.example/x_1000.flv?sign=b" {
		t.Fatalf("hd 选路 = %q", got)
	}
	if got := kuaishouPickRepresentation(repsGot, "sd"); got != "https://flv.example/x_600.flv?sign=c" {
		t.Fatalf("sd 选路 = %q", got)
	}
}

func TestParseKuaishouPageState_NoState(t *testing.T) {
	if _, err := parseKuaishouPageState("<html>验证码页面</html>"); err == nil {
		t.Fatalf("无 INITIAL_STATE 时应报错")
	}
}

// 真实服务器页面会在状态对象里输出 JS 字面量（"authToken":undefined），必须清洗
func TestParseKuaishouPageState_JSLiterals(t *testing.T) {
	html := ksStatePrefix + `{"liveroom":{"playList":[{"liveStream":{},"authToken":undefined,"isLiving":false,"x":[NaN,Infinity,-Infinity]},{"note":"字符串里的 undefined 不该被动"}]}};(function(){var s;})();`
	state, err := parseKuaishouPageState(html)
	if err != nil {
		t.Fatalf("含 JS 字面量的页面解析失败: %v", err)
	}
	if len(state.Liveroom.PlayList) != 2 {
		t.Fatalf("playList 数量 = %d, 期望 2", len(state.Liveroom.PlayList))
	}
}

func TestSanitizeJSLiterals(t *testing.T) {
	in := `{"a":undefined,"b":[1,NaN,-Infinity],"c":"undefined NotNaN ok","d":{"e":undefined}}`
	want := `{"a":null,"b":[1,null,null],"c":"undefined NotNaN ok","d":{"e":null}}`
	if got := sanitizeJSLiterals(in); got != want {
		t.Fatalf("sanitize = %s, 期望 %s", got, want)
	}
}

// JSON 字符串里含 ";(function" 与嵌套括号时的配对健壮性
func TestExtractJSONObject_TrickyString(t *testing.T) {
	raw := `{"a":"xx;(function(){var s;}","b":{"c":[1,2,{"d":"}"}]}}`
	got, ok := extractJSONObject(raw)
	if !ok {
		t.Fatalf("配对失败")
	}
	if got != raw {
		t.Fatalf("截取不完整: %q", got)
	}
}

// ---- playUrls 新旧两种结构 ----

func TestParseKuaishouPlayUrls_LegacyArray(t *testing.T) {
	raw := []byte(`[{"adaptationSet":{"representation":[{"qualityLevel":1,"bitrate":3000,"url":"https://flv.example/best.flv"},{"qualityLevel":4,"bitrate":500,"url":"https://flv.example/worst.flv"}]}}]`)
	reps := parseKuaishouPlayUrls(raw)
	if len(reps) != 2 {
		t.Fatalf("representation 数量 = %d, 期望 2", len(reps))
	}
}

func TestParseKuaishouPlayUrls_H265Fallback(t *testing.T) {
	raw := []byte(`{"h265":{"adaptationSet":{"representation":[{"qualityLevel":1,"bitrate":3000,"url":"https://h265.example/best.flv"}]}}}`)
	reps := parseKuaishouPlayUrls(raw)
	if len(reps) != 1 || reps[0].URL != "https://h265.example/best.flv" {
		t.Fatalf("h265 回退失败: %+v", reps)
	}
}

func TestKuaishouPickRepresentation_Degrade(t *testing.T) {
	// 只有标清时，任何画质请求都应拿到标清而不是空
	reps := []ksRepresentation{{QualityLevel: 4, Bitrate: 500, URL: "https://flv.example/low.flv"}}
	for _, q := range []string{"uhd", "hd", "sd", "unknown"} {
		if got := kuaishouPickRepresentation(reps, q); got != "https://flv.example/low.flv" {
			t.Fatalf("q=%s 选路 = %q, 期望唯一档位", q, got)
		}
	}
}

func TestKuaishouPickRepresentation_BackupURL(t *testing.T) {
	reps := []ksRepresentation{{QualityLevel: 1, BackupURL: "https://bk.example/s.flv"}}
	if got := kuaishouPickRepresentation(reps, "uhd"); got != "https://bk.example/s.flv" {
		t.Fatalf("backupUrl 未生效: %q", got)
	}
}

// ---- 房间号规范化 ----

func TestKuaishouNormalizeRoomID(t *testing.T) {
	cases := map[string]string{
		" 3xc466ctabqw23c ":                                 "3xc466ctabqw23c",
		"https://live.kuaishou.com/u/1987326.html":          "1987326",
		"https://live.kuaishou.com/u/Gydw452323":            "Gydw452323",
		"https://live.kuaishou.com/profile/3xtw2i2xh2j9jsy": "3xtw2i2xh2j9jsy",
	}
	for in, want := range cases {
		if got := kuaishouNormalizeRoomID(in); got != want {
			t.Fatalf("normalize(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// ---- App 分享接口响应 ----

func TestKuaishouByUserResp_Living(t *testing.T) {
	body := []byte(`{"result":1,"liveStream":{"living":true,"user":{"user_name":"分享主播","headUrl":"https://h.example/h.jpg"},"hlsPlayUrl":"https://hls.example/x.m3u8","multiResolutionPlayUrls":[{"level":1,"urls":[{"url":"https://flv.example/share.flv"}]},{"level":3,"urls":[{"url":"https://flv.example/share_sd.flv"}]}]}}`)
	var data ksByUserResp
	if err := json.Unmarshal(body, &data); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !data.LiveStream.Living || data.LiveStream.User.UserName != "分享主播" {
		t.Fatalf("字段解析不完整: %+v", data.LiveStream)
	}
	if got := kuaishouPickResolutionGroup(data.LiveStream.MultiResolutionPlayUrls, "uhd"); got != "https://flv.example/share.flv" {
		t.Fatalf("uhd = %q", got)
	}
	if got := kuaishouPickResolutionGroup(data.LiveStream.MultiResolutionPlayUrls, "sd"); got != "https://flv.example/share_sd.flv" {
		t.Fatalf("sd = %q", got)
	}
}

func TestKuaishouByUserResp_RiskControl(t *testing.T) {
	body := []byte(`{"result":2,"error_msg":"Your action is too frequent. Please try again later.","host-name":"x"}`)
	var data ksByUserResp
	if err := json.Unmarshal(body, &data); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// result!=1 且未开播：调用方据此判定为风控错误
	if data.Result == 1 || data.LiveStream.Living {
		t.Fatalf("风控响应被误判为成功")
	}
}
