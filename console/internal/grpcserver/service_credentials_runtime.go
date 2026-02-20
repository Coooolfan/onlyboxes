package grpcserver

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *RegistryService) SetHasher(hasher *persistence.Hasher) {
	if s == nil {
		return
	}
	s.credentialsMu.Lock()
	s.hasher = hasher
	if hasher != nil {
		s.credentialHashAlgo = persistence.HashAlgorithmHMACSHA256
	} else {
		s.credentialHashAlgo = "legacy-plain"
	}
	s.credentialsMu.Unlock()
}

func (s *RegistryService) GetWorkerSecret(nodeID string) (string, bool) {
	secret, ok := s.getCredential(nodeID)
	if !ok || strings.TrimSpace(secret) == "" {
		return "", false
	}
	return secret, true
}

func (s *RegistryService) CreateProvisionedWorker(now time.Time, offlineTTL time.Duration) (string, string, error) {
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
					"source": "console-ui",
				},
			},
		}, now, offlineTTL)
		if seeded != 1 {
			continue
		}

		credentialValue := workerSecret
		hashAlgo := "legacy-plain"
		s.credentialsMu.RLock()
		hasher := s.hasher
		if strings.TrimSpace(s.credentialHashAlgo) != "" {
			hashAlgo = s.credentialHashAlgo
		}
		s.credentialsMu.RUnlock()
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

func (s *RegistryService) DeleteProvisionedWorker(nodeID string) bool {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" {
		return false
	}

	deletedCredentialInMemory := s.deleteCredential(trimmedNodeID)
	deletedCredentialInDB := s.store.DeleteCredential(trimmedNodeID)
	deletedNode := s.store.Delete(trimmedNodeID)
	if !deletedCredentialInMemory && !deletedCredentialInDB && !deletedNode {
		return false
	}

	s.disconnectWorker(trimmedNodeID, "worker credential revoked")
	return true
}

func (s *RegistryService) getCredential(nodeID string) (string, bool) {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" {
		return "", false
	}

	s.credentialsMu.RLock()
	secret, ok := s.credentials[trimmedNodeID]
	s.credentialsMu.RUnlock()
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
	s.credentialsMu.Lock()
	s.credentials[trimmedNodeID] = hash
	s.credentialsMu.Unlock()
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

	s.sessionsMu.Lock()
	session := s.sessions[trimmedNodeID]
	if session != nil {
		delete(s.sessions, trimmedNodeID)
	}
	s.sessionsMu.Unlock()
	if s.store != nil {
		if err := s.store.ClearSessionByNode(trimmedNodeID); err != nil {
			log.Printf("failed to clear worker session by node: node_id=%s err=%v", trimmedNodeID, err)
		}
	}

	if session != nil {
		session.close(status.Error(codes.PermissionDenied, reason))
	}
}
