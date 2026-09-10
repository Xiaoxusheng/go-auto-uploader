package cryptox

import (
	"bytes"
	"net/http/httptest"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	plain := []byte(`{"hello":"世界"}`)
	enc, err := Encrypt(plain, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decrypt(enc, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("got %s", got)
	}
}

func TestDecryptGarbage(t *testing.T) {
	key := make([]byte, 32)
	if _, err := Decrypt("!!!not-base64!!!", key); err == nil {
		t.Fatal("should fail")
	}
	if _, err := Decrypt("AAAA", key); err == nil {
		t.Fatal("short ciphertext should fail")
	}
}

func TestSessionStore(t *testing.T) {
	s := NewSessionStore(2)
	key := make([]byte, 32)
	if err := s.Put("a", key); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("b", key); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("c", key); err == nil {
		t.Fatal("pool full should error")
	}
	if _, ok := s.Get("a"); !ok {
		t.Fatal("get a")
	}
	s.Delete("a")
	if _, ok := s.Get("a"); ok {
		t.Fatal("deleted")
	}
}

func TestSessionKeyFromRequest(t *testing.T) {
	s := NewSessionStore(0)
	key := make([]byte, 32)
	_ = s.Put("sid1", key)
	r := httptest.NewRequest("GET", "/api/x", nil)
	if _, err := s.SessionKeyFromRequest(r); err == nil {
		t.Fatal("missing sid")
	}
	r2 := httptest.NewRequest("GET", "/api/x", nil)
	r2.Header.Set("X-Session-Id", "sid1")
	got, err := s.SessionKeyFromRequest(r2)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("session key lookup")
	}
}
