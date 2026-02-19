package persistence

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

const HashAlgorithmHMACSHA256 = "hmac-sha256"

type Hasher struct {
	key []byte
}

func NewHasher(key string) (*Hasher, error) {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return nil, errors.New("CONSOLE_HASH_KEY is required")
	}
	return &Hasher{key: []byte(trimmed)}, nil
}

func (h *Hasher) Hash(value string) string {
	if h == nil {
		return ""
	}
	mac := hmac.New(sha256.New, h.key)
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (h *Hasher) Equal(hash string, plain string) bool {
	if h == nil {
		return false
	}
	expected := h.Hash(plain)
	return hmac.Equal([]byte(strings.TrimSpace(hash)), []byte(expected))
}
