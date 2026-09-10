package logx

import (
	"bytes"
	"testing"
)

func TestStoreAddSnapshot(t *testing.T) {
	s := NewStore(3, 10)
	s.Add("info", "hello", "")
	s.Add("error", "boom", "e")
	s.Add("warn", "careful", "")
	// drain channel into ring
	for i := 0; i < 3; i++ {
		e := <-s.Chan()
		s.Append(e)
	}
	if s.Len() != 3 {
		t.Fatalf("len=%d", s.Len())
	}
	if got := s.Snapshot("error", ""); len(got) != 1 || got[0].Message != "boom" {
		t.Fatalf("error filter %+v", got)
	}
	if got := s.Snapshot("", "care"); len(got) != 1 {
		t.Fatalf("keyword filter %+v", got)
	}
}

func TestInterceptor(t *testing.T) {
	s := NewStore(10, 10)
	var buf bytes.Buffer
	l := &Interceptor{Original: &buf, Store: s}
	n, err := l.Write([]byte("2026/01/01 00:00:00.000000 [UPLOAD][ERR] fail here\n"))
	if n == 0 || err != nil {
		t.Fatal(err)
	}
	e := <-s.Chan()
	if e.Level != "error" {
		t.Fatalf("level=%s", e.Level)
	}
	if e.Message != "[UPLOAD][ERR] fail here" {
		t.Fatalf("msg=%q", e.Message)
	}
}
