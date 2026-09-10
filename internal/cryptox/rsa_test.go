package cryptox

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"testing"
)

func TestRSAKeyPairRoundTrip(t *testing.T) {
	kp, err := GenerateRSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if kp.PublicBase64 == "" {
		t.Fatal("empty pubkey")
	}
	// 用公钥加密一个 32 字节 AES 密钥再解包
	pubDER, err := base64.StdEncoding.DecodeString(kp.PublicBase64)
	if err != nil {
		t.Fatal(err)
	}
	pubAny, err := x509.ParsePKIXPublicKey(pubDER)
	if err != nil {
		t.Fatal(err)
	}
	pub := pubAny.(*rsa.PublicKey)
	aesKey := make([]byte, 32)
	_, _ = rand.Read(aesKey)
	enc, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, aesKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := kp.UnwrapAESKey(base64.StdEncoding.EncodeToString(enc))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(aesKey) {
		t.Fatal("unwrap mismatch")
	}
}
