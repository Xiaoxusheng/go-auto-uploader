package naming

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectRoot(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, "live")
	other := filepath.Join(base, "other")
	_ = os.MkdirAll(filepath.Join(live, "a"), 0755)
	_ = os.MkdirAll(other, 0755)
	roots := []string{live, other}
	file := filepath.Join(live, "a", "b.ts")
	if got := DetectRoot(file, roots); got != live {
		t.Fatalf("got %q want %q", got, live)
	}
	if DetectRoot(filepath.Join(base, "nope", "x.ts"), roots) != "" {
		t.Fatal("no match")
	}
}

func TestBuildRemotePath(t *testing.T) {
	got := BuildRemotePath("/home/_safe_uploads", "-坏/主播", "file🌸.ts")
	if got == "" {
		t.Fatal("empty")
	}
	if len(got) < len("/home/_safe_uploads/") || got[:len("/home/_safe_uploads/")] != "/home/_safe_uploads/" {
		t.Fatalf("got %q", got)
	}
}
