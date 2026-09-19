package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
)

// aad is protocol context only (not a resource binding). Ciphertext is portable.
const aad = "env/v1"

const nonceSize = 12

func seal(key, plaintext []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secrets: nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, plaintext, []byte(aad)), nil
}

func open(key, blob []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(blob) < nonceSize+aead.Overhead() {
		return nil, fmt.Errorf("%w: ciphertext too short", ErrOpen)
	}
	nonce, ct := blob[:nonceSize], blob[nonceSize:]
	plain, err := aead.Open(nil, nonce, ct, []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOpen, err)
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: wrap key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
