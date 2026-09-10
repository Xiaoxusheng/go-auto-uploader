package recorder

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestRC4RoundTrip(t *testing.T) {
	key := "key"
	pt := "hello-world-测试"
	enc := RC4Encrypt(pt, key)
	if enc == pt {
		t.Fatal("should change")
	}
	dec := RC4Encrypt(enc, key)
	if dec != pt {
		t.Fatalf("roundtrip %q", dec)
	}
}

func TestSM3Stable(t *testing.T) {
	s := NewSM3()
	s.Write("abc")
	sum1 := hex.EncodeToString(s.Sum())
	s2 := NewSM3()
	s2.Write("abc")
	sum2 := hex.EncodeToString(s2.Sum())
	if sum1 != sum2 || len(sum1) != 64 {
		t.Fatalf("sm3 unstable %s vs %s", sum1, sum2)
	}
}

func TestGenerateABogusNonEmpty(t *testing.T) {
	got := GenerateABogus("aid=6383&web_rid=123", "Mozilla/5.0")
	if got == "" || !bytes.Contains([]byte(got), []byte("=")) {
		t.Fatalf("abogus %q", got)
	}
}
