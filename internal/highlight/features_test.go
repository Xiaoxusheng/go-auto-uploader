package highlight

import (
	"context"
	"math"
	"os"
	"testing"
	"time"
)

func TestFeatureName(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{"lavfi.signalstats.YAVG", "v_YAVG"},
		{"lavfi.signalstats.YMAX", "v_YMAX"},
		{"lavfi.signalstats.YDIF", "v_YDIF"},
		{"lavfi.astats.Overall.RMS_level", "a_RMS_level"},
		{"lavfi.astats.Overall.Zero_crossings_rate", "a_Zero_crossings_rate"},
		// per-channel 字段必须丢弃：Overall 已覆盖同样信息，多声道会产生重复列
		{"lavfi.astats.1.RMS_level", ""},
		{"lavfi.astats.2.Crest_factor", ""},
		{"lavfi.astats.Overall.", "a_"},
		{"frame:0    pts:1", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := featureName(c.key); got != c.want {
			t.Errorf("featureName(%q) = %q，期望 %q", c.key, got, c.want)
		}
	}
}

func TestParseMetadataLine(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		wantKey string
		wantVal float64
		wantOK  bool
	}{
		{
			name:    "视频字段",
			line:    "[Parsed_metadata_4 @ 000001d705e5fac0] lavfi.signalstats.YAVG=3.29903",
			wantKey: "lavfi.signalstats.YAVG",
			wantVal: 3.29903,
			wantOK:  true,
		},
		{
			name:    "音频负值",
			line:    "[Parsed_ametadata_3 @ 000001ff9d41d500] lavfi.astats.Overall.RMS_level=-27.271846",
			wantKey: "lavfi.astats.Overall.RMS_level",
			wantVal: -27.271846,
			wantOK:  true,
		},
		{
			// 帧头行不是键值对，必须落到「记录时间戳」分支
			name:   "帧头行",
			line:   "[Parsed_metadata_4 @ 000001d705e5fac0] frame:0    pts:1       pts_time:0.5",
			wantOK: false,
		},
		{
			// -inf 等非数字值不应被解析成 0
			name:   "非数字值",
			line:   "[Parsed_metadata_4 @ 0x1] lavfi.astats.Overall.Flat_factor=-inf",
			wantOK: false,
		},
		{
			name:   "无括号前缀",
			line:   "lavfi.signalstats.YAVG=1.0",
			wantOK: false,
		},
		{
			name:   "空行",
			line:   "",
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, val, ok := parseMetadataLine(c.line)
			if ok != c.wantOK {
				t.Fatalf("ok = %v，期望 %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if key != c.wantKey {
				t.Errorf("key = %q，期望 %q", key, c.wantKey)
			}
			if math.Abs(val-c.wantVal) > 1e-9 {
				t.Errorf("val = %v，期望 %v", val, c.wantVal)
			}
		})
	}
}

// 帧头行的正则必须能从真实格式里取到时间戳。
func TestFullPtsTimeRegex(t *testing.T) {
	line := "[Parsed_ametadata_3 @ 000001ff9d41d500] frame:0    pts:0       pts_time:0"
	m := reFullPtsTime.FindStringSubmatch(line)
	if m == nil || m[1] != "0" {
		t.Fatalf("未取到 pts_time: %v", m)
	}
}

func TestFeaturesColumnAndSeries(t *testing.T) {
	f := &Features{
		Seconds: 3,
		Names:   []string{"v_YAVG", "a_RMS_level"},
		Columns: map[string][]float64{
			"v_YAVG":      {1, 2, 3},
			"a_RMS_level": {-30, -20, -10},
		},
	}
	if got := f.Column("v_YAVG"); len(got) != 3 || got[2] != 3 {
		t.Fatalf("Column 返回异常: %v", got)
	}
	if f.Column("nope") != nil {
		t.Fatal("不存在的列应返回 nil")
	}
	s := f.Series()
	if len(s.Motion) != 3 || len(s.Audio) != 3 {
		t.Fatalf("Series 长度错误: motion=%d audio=%d", len(s.Motion), len(s.Audio))
	}
	if s.Motion[0] != 1 || s.Audio[2] != -10 {
		t.Fatalf("Series 取值错误: %+v", s)
	}
	if f.Len() != 3 {
		t.Fatalf("Len = %d，期望 3", f.Len())
	}
	var nilF *Features
	if nilF.Len() != 0 || nilF.Column("x") != nil || nilF.Series() != nil {
		t.Fatal("nil Features 必须安全")
	}
}

// 真实素材端到端：验证能拿到多维特征，且运动量/音频与 Probe 同源。
//
//	HIGHLIGHT_E2E_FFMPEG=/path/to/ffmpeg
//	HIGHLIGHT_E2E_SRC=/path/to/clip.mp4
func TestExtractFeaturesRealClip(t *testing.T) {
	ffmpegBin := os.Getenv("HIGHLIGHT_E2E_FFMPEG")
	src := os.Getenv("HIGHLIGHT_E2E_SRC")
	if ffmpegBin == "" || src == "" {
		t.Skip("未设置 HIGHLIGHT_E2E_FFMPEG / HIGHLIGHT_E2E_SRC，跳过")
	}
	if _, err := os.Stat(src); err != nil {
		t.Skipf("素材不存在，跳过: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	start := time.Now()
	f, err := ExtractFeatures(ctx, ffmpegBin, src, 2)
	if err != nil {
		t.Fatalf("ExtractFeatures 失败: %v", err)
	}
	t.Logf("采样 %d 秒 / %d 列，耗时 %s", f.Seconds, len(f.Names), time.Since(start).Truncate(time.Second))

	for _, must := range []string{"v_YAVG", "a_RMS_level"} {
		if col := f.Column(must); len(col) == 0 {
			t.Fatalf("缺少必需列 %s（现有 Names: %v）", must, f.Names)
		}
	}
	if len(f.Names) < 20 {
		t.Fatalf("特征维度只有 %d，放开 key 过滤后应有 20+ 列", len(f.Names))
	}
	if s := f.Series(); s.Len() == 0 {
		t.Fatal("Series 转换后为空")
	}
}
