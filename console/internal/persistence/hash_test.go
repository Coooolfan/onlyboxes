package persistence

import "testing"

func TestNewHasherRequiresKey(t *testing.T) {
	if _, err := NewHasher(""); err == nil {
		t.Fatalf("expected error for empty key")
	}
	if _, err := NewHasher("   "); err == nil {
		t.Fatalf("expected error for blank key")
	}
}

func TestHasherHashAndEqual(t *testing.T) {
	hasher, err := NewHasher("test-key")
	if err != nil {
		t.Fatalf("new hasher: %v", err)
	}
	other, err := NewHasher("other-key")
	if err != nil {
		t.Fatalf("new hasher other: %v", err)
	}

	hash := hasher.Hash("worker-secret")
	if hash == "" {
		t.Fatalf("expected non-empty hash")
	}
	if hash != hasher.Hash("worker-secret") {
		t.Fatalf("hash should be deterministic")
	}
	if hash == other.Hash("worker-secret") {
		t.Fatalf("hash should depend on key")
	}

	if !hasher.Equal(hash, "worker-secret") {
		t.Fatalf("expected hash match")
	}
	if hasher.Equal(hash, "wrong-secret") {
		t.Fatalf("expected hash mismatch")
	}
}
