package recorder

import (
	"strings"
	"testing"
	"time"
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
		t.Fatalf("must not emit FFmpeg localtime macro, got %q", got)
	}
	want := "主播 " + time.Now().Format("2006-01-02 15:04:05")
	// 允许跨秒
	if got != want {
		got2 := BuildWatermarkText(st, "主播")
		if !strings.HasPrefix(got2, "主播 20") {
			t.Fatalf("got %q want like %q", got2, want)
		}
	}
	if DrawtextPos("top-left") != "x=20:y=20" {
		t.Fatal("pos")
	}
}

func TestStrftimeToGo(t *testing.T) {
	if strftimeToGo("%Y-%m-%d %H:%M:%S") != "2006-01-02 15:04:05" {
		t.Fatal(strftimeToGo("%Y-%m-%d %H:%M:%S"))
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
