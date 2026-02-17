package grpcserver

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type WorkerCredential struct {
	Slot         int    `json:"slot"`
	WorkerID     string `json:"worker_id"`
	WorkerSecret string `json:"worker_secret"`
}

func GenerateWorkerCredentials(count int) ([]WorkerCredential, map[string]string, error) {
	if count <= 0 {
		return nil, nil, errors.New("worker credential count must be positive")
	}

	credentials := make([]WorkerCredential, 0, count)
	secretByWorkerID := make(map[string]string, count)
	for slot := 1; slot <= count; slot++ {
		workerID, err := generateUUIDv4()
		if err != nil {
			return nil, nil, fmt.Errorf("generate worker_id for slot %d: %w", slot, err)
		}
		workerSecret, err := generateSecretHex(32)
		if err != nil {
			return nil, nil, fmt.Errorf("generate worker_secret for slot %d: %w", slot, err)
		}
		credentials = append(credentials, WorkerCredential{
			Slot:         slot,
			WorkerID:     workerID,
			WorkerSecret: workerSecret,
		})
		secretByWorkerID[workerID] = workerSecret
	}

	return credentials, secretByWorkerID, nil
}

func WriteWorkerCredentialsFile(path string, credentials []WorkerCredential) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create credentials directory %q: %w", dir, err)
		}
	}

	content, err := json.MarshalIndent(credentials, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	content = append(content, '\n')

	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("write credentials file %q: %w", path, err)
	}
	return nil
}

func generateUUIDv4() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}

	// RFC 4122 variant and version bits.
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80

	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		binary.BigEndian.Uint32(raw[0:4]),
		binary.BigEndian.Uint16(raw[4:6]),
		binary.BigEndian.Uint16(raw[6:8]),
		binary.BigEndian.Uint16(raw[8:10]),
		raw[10:16],
	), nil
}

func generateSecretHex(length int) (string, error) {
	if length <= 0 {
		return "", errors.New("length must be positive")
	}

	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
