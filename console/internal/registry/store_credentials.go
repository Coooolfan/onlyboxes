package registry

import (
	"context"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

func (s *Store) GetCredentialHash(nodeID string) (string, bool) {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" || s == nil || s.queries == nil {
		return "", false
	}
	credential, err := s.queries.GetWorkerCredentialByNode(context.Background(), trimmedNodeID)
	if err != nil {
		return "", false
	}
	secretHash := strings.TrimSpace(credential.SecretHash)
	if secretHash == "" {
		return "", false
	}
	return secretHash, true
}

func (s *Store) PutCredentialHashIfAbsent(nodeID string, secretHash string, hashAlgo string, now time.Time) bool {
	trimmedNodeID := strings.TrimSpace(nodeID)
	trimmedHash := strings.TrimSpace(secretHash)
	trimmedHashAlgo := strings.TrimSpace(hashAlgo)
	if trimmedNodeID == "" || trimmedHash == "" || trimmedHashAlgo == "" || s == nil || s.queries == nil {
		return false
	}

	nowMS := now.UnixMilli()
	inserted, err := s.queries.InsertWorkerCredentialIfAbsent(context.Background(), sqlc.InsertWorkerCredentialIfAbsentParams{
		NodeID:          trimmedNodeID,
		SecretHash:      trimmedHash,
		HashAlgo:        trimmedHashAlgo,
		CreatedAtUnixMs: nowMS,
		UpdatedAtUnixMs: nowMS,
	})
	return err == nil && inserted == 1
}

func (s *Store) DeleteCredential(nodeID string) bool {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" || s == nil || s.queries == nil {
		return false
	}
	deleted, err := s.queries.DeleteWorkerCredentialByNode(context.Background(), trimmedNodeID)
	return err == nil && deleted == 1
}

func (s *Store) ListCredentialHashes() map[string]string {
	if s == nil || s.queries == nil {
		return map[string]string{}
	}
	credentials, err := s.queries.ListWorkerCredentials(context.Background())
	if err != nil {
		return map[string]string{}
	}
	values := make(map[string]string, len(credentials))
	for _, credential := range credentials {
		nodeID := strings.TrimSpace(credential.NodeID)
		secretHash := strings.TrimSpace(credential.SecretHash)
		if nodeID == "" || secretHash == "" {
			continue
		}
		values[nodeID] = secretHash
	}
	return values
}
