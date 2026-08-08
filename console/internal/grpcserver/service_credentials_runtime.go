package grpcserver

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var ErrInvalidWorkerType = errors.New("invalid worker type")
var ErrWorkerSysAlreadyExists = errors.New("worker-sys already exists for owner")

const defaultWorkerOwnerID = "system"

func (s *RegistryService) SetHasher(hasher *persistence.Hasher) {
	if s == nil {
		return
	}
	s.credentialsMu.Lock()
	defer s.credentialsMu.Unlock()
	s.hasher = hasher
	if hasher != nil {
		s.credentialHashAlgo = persistence.HashAlgorithmHMACSHA256
	} else {
		s.credentialHashAlgo = "legacy-plain"
	}
}

func (s *RegistryService) GetWorkerSecret(nodeID string) (string, bool) {
	secret, ok := s.getCredential(nodeID)
	if !ok || strings.TrimSpace(secret) == "" {
		return "", false
	}
	return secret, true
}

func (s *RegistryService) CreateProvisionedWorker(now time.Time, offlineTTL time.Duration) (string, string, error) {
	return s.CreateProvisionedWorkerForOwner(defaultWorkerOwnerID, registry.WorkerTypeNormal, now, offlineTTL)
}

func (s *RegistryService) CreateProvisionedWorkerForOwner(
	ownerID string,
	workerType string,
	now time.Time,
	offlineTTL time.Duration,
) (string, string, error) {
	normalizedOwnerID := strings.TrimSpace(ownerID)
	if normalizedOwnerID == "" {
		return "", "", errors.New("owner_id is required")
	}
	normalizedWorkerType := normalizeProvisioningWorkerType(workerType)
	if normalizedWorkerType == "" {
		return "", "", ErrInvalidWorkerType
	}
	if normalizedWorkerType == registry.WorkerTypeSys {
		if count := s.store.CountWorkersByOwnerAndType(normalizedOwnerID, normalizedWorkerType); count > 0 {
			return "", "", ErrWorkerSysAlreadyExists
		}
	}

	for attempt := 0; attempt < maxProvisioningCreateAttempts; attempt++ {
		workerID, err := generateUUIDv4()
		if err != nil {
			return "", "", fmt.Errorf("generate worker_id: %w", err)
		}
		workerSecret, err := generateSecretHex(32)
		if err != nil {
			return "", "", fmt.Errorf("generate worker_secret: %w", err)
		}

		seeded := s.store.SeedProvisionedWorkers([]registry.ProvisionedWorker{
			{
				NodeID: workerID,
				Labels: map[string]string{
					"source":                    "console-ui",
					registry.LabelOwnerIDKey:    normalizedOwnerID,
					registry.LabelWorkerTypeKey: normalizedWorkerType,
				},
			},
		}, now, offlineTTL)
		if seeded != 1 {
			continue
		}
		if normalizedWorkerType == registry.WorkerTypeSys {
			claimed, claimErr := s.store.ClaimWorkerSysOwner(normalizedOwnerID, workerID, now)
			if claimErr != nil {
				s.store.Delete(workerID)
				return "", "", fmt.Errorf("claim worker-sys owner: %w", claimErr)
			}
			if !claimed {
				s.store.Delete(workerID)
				return "", "", ErrWorkerSysAlreadyExists
			}
		}

		credentialValue := workerSecret
		hasher, hashAlgo := func() (*persistence.Hasher, string) {
			s.credentialsMu.RLock()
			defer s.credentialsMu.RUnlock()
			hashAlgo := "legacy-plain"
			if strings.TrimSpace(s.credentialHashAlgo) != "" {
				hashAlgo = s.credentialHashAlgo
			}
			return s.hasher, hashAlgo
		}()
		if hasher != nil {
			credentialValue = hasher.Hash(workerSecret)
		}

		if !s.putCredentialIfAbsent(workerID, credentialValue) {
			s.store.Delete(workerID)
			continue
		}
		if !s.store.PutCredentialHashIfAbsent(workerID, credentialValue, hashAlgo, now) {
			s.deleteCredential(workerID)
			s.store.Delete(workerID)
			continue
		}

		return workerID, workerSecret, nil
	}
	return "", "", errors.New("failed to allocate unique worker_id")
}

func normalizeProvisioningWorkerType(workerType string) string {
	switch strings.TrimSpace(strings.ToLower(workerType)) {
	case registry.WorkerTypeNormal:
		return registry.WorkerTypeNormal
	case registry.WorkerTypeSys:
		return registry.WorkerTypeSys
	default:
		return ""
	}
}

func (s *RegistryService) DeleteProvisionedWorker(nodeID string) (bool, error) {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" {
		return false, nil
	}
	if _, err := s.deleteTerminalSessionRoutesByNode(trimmedNodeID); err != nil {
		return false, err
	}

	deletedCredentialInMemory := s.deleteCredential(trimmedNodeID)
	deletedCredentialInDB := s.store.DeleteCredential(trimmedNodeID)
	deletedNode := s.store.Delete(trimmedNodeID)
	if !deletedCredentialInMemory && !deletedCredentialInDB && !deletedNode {
		return false, nil
	}

	s.disconnectWorker(trimmedNodeID, "worker credential revoked")
	return true, nil
}

func (s *RegistryService) getCredential(nodeID string) (string, bool) {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" {
		return "", false
	}

	secret, ok := func() (string, bool) {
		s.credentialsMu.RLock()
		defer s.credentialsMu.RUnlock()
		secret, ok := s.credentials[trimmedNodeID]
		return secret, ok
	}()
	if ok {
		return secret, true
	}
	if s.store == nil {
		return "", false
	}
	hash, exists := s.store.GetCredentialHash(trimmedNodeID)
	if !exists {
		return "", false
	}
	func() {
		s.credentialsMu.Lock()
		defer s.credentialsMu.Unlock()
		s.credentials[trimmedNodeID] = hash
	}()
	return hash, true
}

func (s *RegistryService) putCredentialIfAbsent(nodeID string, secret string) bool {
	trimmedNodeID := strings.TrimSpace(nodeID)
	trimmedSecret := strings.TrimSpace(secret)
	if trimmedNodeID == "" || trimmedSecret == "" {
		return false
	}

	s.credentialsMu.Lock()
	defer s.credentialsMu.Unlock()

	if _, exists := s.credentials[trimmedNodeID]; exists {
		return false
	}
	s.credentials[trimmedNodeID] = trimmedSecret
	return true
}

func (s *RegistryService) deleteCredential(nodeID string) bool {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" {
		return false
	}

	s.credentialsMu.Lock()
	defer s.credentialsMu.Unlock()

	if _, exists := s.credentials[trimmedNodeID]; !exists {
		return false
	}
	delete(s.credentials, trimmedNodeID)
	return true
}

func (s *RegistryService) disconnectWorker(nodeID string, reason string) {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" {
		return
	}

	session := func() *activeSession {
		s.sessionsMu.Lock()
		defer s.sessionsMu.Unlock()
		session := s.sessions[trimmedNodeID]
		if session != nil {
			delete(s.sessions, trimmedNodeID)
		}
		return session
	}()
	if s.store != nil {
		if err := s.store.ClearSessionByNode(trimmedNodeID); err != nil {
			slog.Error("failed to clear worker session by node", "node_id", trimmedNodeID, "error", err)
		}
	}

	if session != nil {
		session.close(status.Error(codes.PermissionDenied, reason))
	}
}
