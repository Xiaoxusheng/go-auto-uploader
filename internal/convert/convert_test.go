package convert

import "testing"

func TestIsTS(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"a.ts":  true,
		"a.TS":  true,
		"a.mp4": false,
		"a.png": false,
		"a":     false,
	}
	for in, want := range cases {
		if got := IsTS(in); got != want {
			t.Fatalf("IsTS(%q)=%v want %v", in, got, want)
		}
	}
}

func TestIsArtifact(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"/x/y.mp4.part": true,
		"/x/y.TS.TMP":   true,
		"/x/y.ts":       false,
		"/x/y.mp4":      false,
	}
	for in, want := range cases {
		if got := IsArtifact(in); got != want {
			t.Fatalf("IsArtifact(%q)=%v want %v", in, got, want)
		}
	}
}
