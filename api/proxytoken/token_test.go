package proxytoken

import (
	"encoding/base64"
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

func TestSignMatchesCrossLanguageVector(t *testing.T) {
	key, err := DeriveKey("worker-secret")
	if err != nil {
		t.Fatalf("derive key: %v", err)
	}
	token, err := Sign(key, Claims{
		WorkerID:        "worker-a",
		SessionID:       "obx:owner:session",
		Port:            8080,
		ExpiresAtUnixMs: 4_102_444_800_000,
	})
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	const want = "obx_route_v1.eyJ3b3JrZXJfaWQiOiJ3b3JrZXItYSIsInNlc3Npb25faWQiOiJvYng6b3duZXI6c2Vzc2lvbiIsInBvcnQiOjgwODAsImV4cCI6NDEwMjQ0NDgwMDAwMH0.Ed5xO4kv0pF1fEyvuqn33UVFDJbZF_z_8tpX7ChTvlk"
	if token != want {
		t.Fatalf("unexpected token:\nwant %s\n got %s", want, token)
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
		{WorkerID: "worker", SessionID: "session", Port: 8080, ExpiresAtUnixMs: 1, Generation: -1},
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

func TestPersistentGenerationVectorAndTampering(t *testing.T) {
	key, _ := DeriveKey("worker-secret")
	claims := Claims{WorkerID: "worker-a", SessionID: "obx:owner:session", Port: 8080, ExpiresAtUnixMs: 4_102_444_800_000, Generation: 2}
	token, err := Sign(key, claims)
	if err != nil {
		t.Fatal(err)
	}
	const want = "obx_route_v1.eyJ3b3JrZXJfaWQiOiJ3b3JrZXItYSIsInNlc3Npb25faWQiOiJvYng6b3duZXI6c2Vzc2lvbiIsInBvcnQiOjgwODAsImV4cCI6NDEwMjQ0NDgwMDAwMCwiZ2VuZXJhdGlvbiI6Mn0.-agCbNWpdTN1rBhGjyL6ol1vPBPv0XdKogMNgyVkxg0"
	if token != want {
		t.Fatal("persistent cross-language vector changed")
	}
	got, err := Verify(key, token, time.UnixMilli(1_730_000_000_000))
	if err != nil || got != claims {
		t.Fatalf("persistent claims lost: %+v %v", got, err)
	}
	// Replace only the encoded generation, preserving the original signature.
	parts := strings.Split(token, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	changed := strings.Replace(string(payload), `"generation":2`, `"generation":3`, 1)
	if changed == string(payload) {
		t.Fatal("test did not alter generation")
	}
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(changed))
	tampered := strings.Join(parts, ".")
	if _, err := Verify(key, tampered, time.UnixMilli(1_730_000_000_000)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("generation tampering accepted: %v", err)
	}
}

func TestVerifyRejectsSignedNegativeGeneration(t *testing.T) {
	key, _ := DeriveKey("worker-secret")
	const token = "obx_route_v1.eyJ3b3JrZXJfaWQiOiJ3b3JrZXItYSIsInNlc3Npb25faWQiOiJvYng6b3duZXI6c2Vzc2lvbiIsInBvcnQiOjgwODAsImV4cCI6NDEwMjQ0NDgwMDAwMCwiZ2VuZXJhdGlvbiI6LTF9.9k_vKjIBgU0b4I-vMmMLQT03emp_d4cih1J-gtjpKLo"
	if _, err := Verify(key, token, time.UnixMilli(1_730_000_000_000)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("negative generation accepted: %v", err)
	}
}
