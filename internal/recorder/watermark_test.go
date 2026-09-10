package recorder

import "testing"

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
	wantPrefix := "主播 %{localtime:%Y-%m-%d %H\\:%M\\:%S}"
	if got != wantPrefix {
		t.Fatalf("got %q want %q", got, wantPrefix)
	}
	if DrawtextPos("top-left") != "x=20:y=20" {
		t.Fatal("pos")
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
