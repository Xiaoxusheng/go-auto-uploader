package naming

import "testing"

func TestCleanFileName(t *testing.T) {
	t.Parallel()
	got := CleanFileName("主播🌸直播.ts")
	if got == "" || got == ".ts" {
		t.Fatalf("unexpected clean result %q", got)
	}
	if CleanFileName("...") == "" {
		t.Fatal("empty should fall back to file")
	}
}

func TestDetectStreamer(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/home/_safe_uploads/主播/xx.ts":      "主播",
		"/_safe_uploads/主播/xx.ts":           "主播",
		"/home/_safe_uploads/主播/date/xx.ts": "主播",
		"/x":                                 "未知",
	}
	for in, want := range cases {
		if got := DetectStreamer(in); got != want {
			t.Fatalf("DetectStreamer(%q)=%q want %q", in, got, want)
		}
	}
}
