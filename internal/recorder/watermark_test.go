package recorder

import (
	"strings"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	if SanitizeName("a/b:c*d") != "abcd" {
		t.Fatal(SanitizeName("a/b:c*d"))
	}
	if SanitizeName("   ") != "未命名主播" {
		t.Fatal("empty fallback")
	}
}

func TestFormatDurationAndBytes(t *testing.T) {
	if FormatDuration(0) != "00分00秒" {
		t.Fatal(FormatDuration(0))
	}
	if FormatBytes(512) != "512 B" {
		t.Fatal(FormatBytes(512))
	}
	if FormatBytes(2048) != "2.00 KB" {
		t.Fatal(FormatBytes(2048))
	}
}

func TestBuildWatermarkTextAndPos(t *testing.T) {
	st := StyleFrom("", "%Y-%m-%d %H:%M:%S", "bottom-right", "#FFFFFF", 38)
	got := BuildWatermarkText(st, "主播")
	if !strings.HasPrefix(got, "主播 ") {
		t.Fatalf("prefix: %q", got)
	}
	if strings.Contains(got, "%{localtime") {
		t.Fatalf("static text must not emit localtime macro, got %q", got)
	}
	if DrawtextPos("top-left") != "x=20:y=20" {
		t.Fatal("pos")
	}
}

func TestBuildWatermarkTextLive(t *testing.T) {
	st := StyleFrom("", "%Y-%m-%d %H:%M:%S", "bottom-right", "#FFFFFF", 38)
	got := BuildWatermarkTextLive(st, "主播")
	// expansion=strftime：文本直接是 strftime 格式，不要 %{localtime} 宏
	if !strings.Contains(got, "%Y-%m-%d %H:%M:%S") {
		t.Fatalf("live text should keep strftime format, got %q", got)
	}
	if strings.Contains(got, "%{localtime") {
		t.Fatalf("live text must not use localtime macro, got %q", got)
	}
}

func TestStrftimeToGo(t *testing.T) {
	if strftimeToGo("%Y-%m-%d %H:%M:%S") != "2006-01-02 15:04:05" {
		t.Fatal(strftimeToGo("%Y-%m-%d %H:%M:%S"))
	}
}

// Windows 盘符里的冒号是 filtergraph 的参数分隔符，必须转义，否则整条滤镜被 FFmpeg 拒绝。
func TestEscapeFilterPath(t *testing.T) {
	if got := escapeFilterPath(`D:\upload\font.ttf`); got != `D\:/upload/font.ttf` {
		t.Fatalf("windows path: %q", got)
	}
	if got := escapeFilterPath("/home/upload/font.ttf"); got != "/home/upload/font.ttf" {
		t.Fatalf("linux path should be unchanged: %q", got)
	}
}

func TestNormalizeFontColor(t *testing.T) {
	if NormalizeFontColor("#FF0000") != "0xFF0000" {
		t.Fatal("hex")
	}
	if NormalizeFontColor("") != "white@0.95" {
		t.Fatal("default")
	}
}
