package proxytoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	Prefix             = "obx_route_v1."
	HeaderName         = "X-Onlyboxes-Route-Token"
	ProxyEndpointLabel = "obx.proxy_endpoint"
	keyDerivationLabel = "onlyboxes/proxy-route/v1"
	maxTokenLength     = 4096
)

var (
	ErrInvalidToken = errors.New("invalid route token")
	ErrExpiredToken = errors.New("route token expired")
)

type Claims struct {
	WorkerID        string `json:"worker_id"`
	SessionID       string `json:"session_id"`
	Port            int    `json:"port"`
	ExpiresAtUnixMs int64  `json:"exp"`
}

func DeriveKey(workerSecret string) ([]byte, error) {
	secret := strings.TrimSpace(workerSecret)
	if secret == "" {
		return nil, errors.New("worker secret is required")
	}

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(keyDerivationLabel))
	return mac.Sum(nil), nil
}

func Sign(key []byte, claims Claims) (string, error) {
	if len(key) == 0 {
		return "", errors.New("route token key is required")
	}
	if err := validateClaims(claims); err != nil {
		return "", err
	}

	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal route token claims: %w", err)
	}
	payloadEncoded := base64.RawURLEncoding.EncodeToString(payload)
	signedValue := Prefix + payloadEncoded

	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(signedValue))
	signatureEncoded := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signedValue + "." + signatureEncoded, nil
}

func Verify(key []byte, token string, now time.Time) (Claims, error) {
	if len(key) == 0 {
		return Claims{}, ErrInvalidToken
	}
	token = strings.TrimSpace(token)
	if len(token) == 0 || len(token) > maxTokenLength || !strings.HasPrefix(token, Prefix) {
		return Claims{}, ErrInvalidToken
	}

	parts := strings.Split(strings.TrimPrefix(token, Prefix), ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Claims{}, ErrInvalidToken
	}
	payloadEncoded := parts[0]
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size {
		return Claims{}, ErrInvalidToken
	}

	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(Prefix + payloadEncoded))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return Claims{}, ErrInvalidToken
	}

	payload, err := base64.RawURLEncoding.DecodeString(payloadEncoded)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	claims := Claims{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, ErrInvalidToken
	}
	if err := validateClaims(claims); err != nil {
		return Claims{}, ErrInvalidToken
	}
	if now.IsZero() {
		now = time.Now()
	}
	if now.UnixMilli() >= claims.ExpiresAtUnixMs {
		return Claims{}, ErrExpiredToken
	}
	return claims, nil
}

func validateClaims(claims Claims) error {
	if strings.TrimSpace(claims.WorkerID) == "" {
		return errors.New("worker_id is required")
	}
	if strings.TrimSpace(claims.SessionID) == "" {
		return errors.New("session_id is required")
	}
	if claims.Port < 1 || claims.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if claims.ExpiresAtUnixMs <= 0 {
		return errors.New("exp is required")
	}
	return nil
}
