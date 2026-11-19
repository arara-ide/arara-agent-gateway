package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

const prefix = "v1:"

var ErrInvalid = errors.New("invalid ciphertext")

type Box struct {
	aead cipher.AEAD
}

func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

func (b *Box) Seal(plain []byte, associated string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, plain, []byte(associated))
	return prefix + base64.RawStdEncoding.EncodeToString(out), nil
}

func (b *Box) Open(sealed, associated string) ([]byte, error) {
	raw, ok := strings.CutPrefix(sealed, prefix)
	if !ok {
		return nil, ErrInvalid
	}
	data, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil || len(data) < b.aead.NonceSize() {
		return nil, ErrInvalid
	}
	nonce, ct := data[:b.aead.NonceSize()], data[b.aead.NonceSize():]
	plain, err := b.aead.Open(nil, nonce, ct, []byte(associated))
	if err != nil {
		return nil, ErrInvalid
	}
	return plain, nil
}
