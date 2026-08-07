package proxytoken

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignAndVerify(t *testing.T) {
	key, err := DeriveKey("worker-secret-a")
	if err != nil {
		t.Fatalf("derive key: %v", err)
	}
	now := time.UnixMilli(1_730_000_000_000)
	want := Claims{
		WorkerID:        "worker-01",
		SessionID:       "obx:acc_xxx:session-01",
		Port:            8080,
		ExpiresAtUnixMs: now.Add(15 * time.Second).UnixMilli(),
	}

	token, err := Sign(key, want)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	if !strings.HasPrefix(token, Prefix) {
		t.Fatalf("expected prefix %q, got %q", Prefix, token)
	}

	got, err := Verify(key, token, now)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if got != want {
		t.Fatalf("unexpected claims: want=%#v got=%#v", want, got)
	}
}

func TestVerifyRejectsTamperingAndWrongWorkerKey(t *testing.T) {
	keyA, err := DeriveKey("worker-secret-a")
	if err != nil {
		t.Fatalf("derive key A: %v", err)
	}
	keyB, err := DeriveKey("worker-secret-b")
	if err != nil {
		t.Fatalf("derive key B: %v", err)
	}
	now := time.UnixMilli(1_730_000_000_000)
	token, err := Sign(keyA, Claims{
		WorkerID:        "worker-a",
		SessionID:       "session-a",
		Port:            3000,
		ExpiresAtUnixMs: now.Add(15 * time.Second).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	if _, err := Verify(keyB, token, now); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected wrong key rejection, got %v", err)
	}

	last := token[len(token)-1]
	replacement := byte('A')
	if last == replacement {
		replacement = 'B'
	}
	tampered := token[:len(token)-1] + string(replacement)
	if _, err := Verify(keyA, tampered, now); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected tampered token rejection, got %v", err)
	}
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	key, err := DeriveKey("worker-secret")
	if err != nil {
		t.Fatalf("derive key: %v", err)
	}
	now := time.UnixMilli(1_730_000_000_000)
	token, err := Sign(key, Claims{
		WorkerID:        "worker-a",
		SessionID:       "session-a",
		Port:            8080,
		ExpiresAtUnixMs: now.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	if _, err := Verify(key, token, now); !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("expected expired token, got %v", err)
	}
}

func TestSignRejectsInvalidClaims(t *testing.T) {
	key, err := DeriveKey("worker-secret")
	if err != nil {
		t.Fatalf("derive key: %v", err)
	}

	tests := []Claims{
		{SessionID: "session", Port: 8080, ExpiresAtUnixMs: 1},
		{WorkerID: "worker", Port: 8080, ExpiresAtUnixMs: 1},
		{WorkerID: "worker", SessionID: "session", Port: 0, ExpiresAtUnixMs: 1},
		{WorkerID: "worker", SessionID: "session", Port: 8080},
	}
	for _, claims := range tests {
		if _, err := Sign(key, claims); err == nil {
			t.Fatalf("expected invalid claims rejection: %#v", claims)
		}
	}
}
