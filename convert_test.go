package main

import "testing"

func TestIsTSVideoFile(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"a.ts":  true,
		"a.TS":  true,
		"a.mp4": false,
		"a.png": false,
		"a":     false,
	}
	for in, want := range cases {
		if got := isTSVideoFile(in); got != want {
			t.Fatalf("isTSVideoFile(%q)=%v want %v", in, got, want)
		}
	}
}

func TestShouldSkipUploadArtifact(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"/x/y.mp4.part": true,
		"/x/y.TS.TMP":   true,
		"/x/y.ts":       false,
		"/x/y.mp4":      false,
	}
	for in, want := range cases {
		if got := shouldSkipUploadArtifact(in); got != want {
			t.Fatalf("shouldSkipUploadArtifact(%q)=%v want %v", in, got, want)
		}
	}
}
