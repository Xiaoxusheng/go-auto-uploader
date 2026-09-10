package cryptox

import (
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
)

// RSAKeyPair 启动时生成的一次性 RSA-2048 密钥对。
type RSAKeyPair struct {
	Private      *rsa.PrivateKey
	PublicBase64 string
}

// GenerateRSAKeyPair 生成密钥对并导出 PKIX 公钥 Base64。
func GenerateRSAKeyPair() (*RSAKeyPair, error) {
	priv, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate rsa: %w", err)
	}
	pubASN1, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal pubkey: %w", err)
	}
	return &RSAKeyPair{
		Private:      priv,
		PublicBase64: base64.StdEncoding.EncodeToString(pubASN1),
	}, nil
}

// UnwrapAESKey 用 RSA-OAEP 解密前端送来的 32 字节 AES 密钥。
func (k *RSAKeyPair) UnwrapAESKey(encBase64 string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encBase64)
	if err != nil {
		return nil, err
	}
	aesKey, err := rsa.DecryptOAEP(sha256.New(), cryptorand.Reader, k.Private, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	if len(aesKey) != 32 {
		return nil, fmt.Errorf("aes key must be 32 bytes, got %d", len(aesKey))
	}
	return aesKey, nil
}
