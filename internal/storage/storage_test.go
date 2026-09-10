package storage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestHistoryAddCap(t *testing.T) {
	h := NewHistoryStore(3)
	for i := 0; i < 5; i++ {
		h.Add(HistoryRecord{Name: string(rune('a' + i))})
	}
	if h.Len() != 3 {
		t.Fatalf("len=%d want 3", h.Len())
	}
	snap := h.Snapshot()
	if snap[0].Name != "c" {
		t.Fatalf("oldest should be dropped, got %q", snap[0].Name)
	}
}

func TestSuccessAddFlushReload(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ok.json")
	s := NewSuccessStore(p, 100)
	s.Load()
	s.Add(UploadRecord{Time: time.Now(), Streamer: "主播", Name: "f.ts", Size: 1024})
	if s.Len() != 1 {
		t.Fatal("len")
	}
	s.Flush()

	s2 := NewSuccessStore(p, 100)
	s2.Load()
	if s2.Len() != 1 {
		t.Fatalf("reload len=%d", s2.Len())
	}
	if s2.Snapshot()[0].Streamer != "主播" {
		t.Fatal("streamer mismatch")
	}
}

func TestDirStatusFlushReload(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ds.json")
	st := NewDirStatusStore(p)
	ds := st.GetOrCreate("/data/a")
	ds.Mu.Lock()
	ds.TotalFiles = 3
	ds.UploadedFiles = 1
	ds.Mu.Unlock()
	st.Flush()

	st2 := NewDirStatusStore(p)
	st2.Load()
	got, ok := st2.Get("/data/a")
	if !ok || got.TotalFiles != 3 || got.UploadedFiles != 1 {
		t.Fatalf("reload mismatch %+v", got)
	}
}
