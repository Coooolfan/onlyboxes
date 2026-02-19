package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

var ErrNodeNotFound = errors.New("worker node not found")
var ErrSessionMismatch = errors.New("worker session mismatch")

type WorkerStatus string

const (
	StatusAll     WorkerStatus = "all"
	StatusOnline  WorkerStatus = "online"
	StatusOffline WorkerStatus = "offline"
)

type CapabilityDeclaration struct {
	Name        string `json:"name"`
	MaxInflight int32  `json:"max_inflight"`
}

type Worker struct {
	NodeID       string
	SessionID    string
	Provisioned  bool
	NodeName     string
	ExecutorKind string
	Capabilities []CapabilityDeclaration
	Labels       map[string]string
	Version      string
	RegisteredAt time.Time
	LastSeenAt   time.Time
}

type WorkerView struct {
	Worker
	Status WorkerStatus
}

type WorkerStats struct {
	Total   int
	Online  int
	Offline int
	Stale   int
}

type ProvisionedWorker struct {
	Slot   int
	NodeID string
	Labels map[string]string
}

type Store struct {
	db      *persistence.DB
	queries *sqlc.Queries
}

func NewStoreWithPersistence(db *persistence.DB) *Store {
	if db == nil {
		panic("registry store requires non-nil persistence db")
	}
	return &Store{db: db, queries: db.Queries}
}

func (s *Store) Persistence() *persistence.DB {
	if s == nil {
		return nil
	}
	return s.db
}

func (s *Store) Upsert(req *registryv1.ConnectHello, sessionID string, now time.Time) error {
	if s == nil || s.db == nil || s.queries == nil {
		return errors.New("registry store is unavailable")
	}
	if req == nil {
		return errors.New("connect hello is required")
	}

	ctx := context.Background()
	nodeID := strings.TrimSpace(req.GetNodeId())
	if nodeID == "" {
		return errors.New("node_id is required")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("session_id is required")
	}
	nodeName := strings.TrimSpace(req.GetNodeName())

	existingNode, err := s.queries.GetWorkerNodeByID(ctx, nodeID)
	hasExisting := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if nodeName == "" && hasExisting {
		nodeName = existingNode.NodeName
	}

	labels := cloneMap(req.GetLabels())
	provisioned := int64(0)
	if hasExisting && existingNode.Provisioned != 0 {
		provisioned = 1
		existingLabels, err := s.queries.ListWorkerLabelsByNode(ctx, nodeID)
		if err != nil {
			return err
		}
		base := make(map[string]string, len(existingLabels))
		for _, row := range existingLabels {
			base[row.LabelKey] = row.LabelValue
		}
		labels = mergeLabels(base, labels)
	}

	capabilities := resolveProtoCapabilities(req.GetCapabilities(), req.GetLanguages())
	nowMS := now.UnixMilli()

	return s.db.WithTx(ctx, func(q *sqlc.Queries) error {
		if err := q.UpsertWorkerNode(ctx, sqlc.UpsertWorkerNodeParams{
			NodeID:             nodeID,
			SessionID:          sessionID,
			Provisioned:        provisioned,
			NodeName:           nodeName,
			ExecutorKind:       req.GetExecutorKind(),
			Version:            req.GetVersion(),
			RegisteredAtUnixMs: nowMS,
			LastSeenAtUnixMs:   nowMS,
		}); err != nil {
			return err
		}
		if err := q.DeleteWorkerCapabilitiesByNode(ctx, nodeID); err != nil {
			return err
		}
		for _, capability := range capabilities {
			if err := q.InsertWorkerCapability(ctx, sqlc.InsertWorkerCapabilityParams{
				NodeID:         nodeID,
				CapabilityName: capability.Name,
				MaxInflight:    int64(capability.MaxInflight),
			}); err != nil {
				return err
			}
		}
		if err := q.DeleteWorkerLabelsByNode(ctx, nodeID); err != nil {
			return err
		}
		for key, value := range labels {
			if err := q.InsertWorkerLabel(ctx, sqlc.InsertWorkerLabelParams{
				NodeID:     nodeID,
				LabelKey:   key,
				LabelValue: value,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) SeedProvisionedWorkers(workers []ProvisionedWorker, now time.Time, offlineTTL time.Duration) int {
	if len(workers) == 0 || s == nil || s.db == nil {
		return 0
	}

	lastSeenAt := now.Add(-time.Second)
	if offlineTTL > 0 {
		lastSeenAt = now.Add(-(offlineTTL + time.Second))
	}
	registeredMS := now.UnixMilli()
	lastSeenMS := lastSeenAt.UnixMilli()

	added := 0
	for _, worker := range workers {
		nodeID := strings.TrimSpace(worker.NodeID)
		if nodeID == "" {
			continue
		}

		nodeName := fmt.Sprintf("worker-slot-%d", worker.Slot)
		if worker.Slot <= 0 {
			nodeName = fmt.Sprintf("worker-slot-%s", shortNodeID(nodeID))
		}

		labels := cloneMap(worker.Labels)
		if worker.Slot > 0 {
			if _, exists := labels["slot"]; !exists {
				labels["slot"] = fmt.Sprintf("%d", worker.Slot)
			}
		}

		inserted := int64(0)
		err := s.db.WithTx(context.Background(), func(q *sqlc.Queries) error {
			rows, err := q.InsertProvisionedWorkerNodeIfAbsent(context.Background(), sqlc.InsertProvisionedWorkerNodeIfAbsentParams{
				NodeID:             nodeID,
				NodeName:           nodeName,
				RegisteredAtUnixMs: registeredMS,
				LastSeenAtUnixMs:   lastSeenMS,
			})
			if err != nil {
				return err
			}
			inserted = rows
			if rows == 0 {
				return nil
			}
			for key, value := range labels {
				if err := q.InsertWorkerLabel(context.Background(), sqlc.InsertWorkerLabelParams{
					NodeID:     nodeID,
					LabelKey:   key,
					LabelValue: value,
				}); err != nil {
					return err
				}
			}
			return nil
		})
		if err == nil && inserted == 1 {
			added++
		}
	}
	return added
}

func (s *Store) Delete(nodeID string) bool {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" || s == nil || s.queries == nil {
		return false
	}

	rows, err := s.queries.DeleteWorkerNodeByID(context.Background(), trimmedNodeID)
	return err == nil && rows > 0
}

func (s *Store) TouchWithSession(nodeID string, sessionID string, now time.Time) error {
	if s == nil || s.queries == nil {
		return ErrNodeNotFound
	}

	rows, err := s.queries.UpdateWorkerHeartbeatBySession(context.Background(), sqlc.UpdateWorkerHeartbeatBySessionParams{
		LastSeenAtUnixMs: now.UnixMilli(),
		NodeID:           strings.TrimSpace(nodeID),
		SessionID:        strings.TrimSpace(sessionID),
	})
	if err != nil {
		return err
	}
	if rows > 0 {
		return nil
	}

	node, err := s.queries.GetWorkerNodeByID(context.Background(), strings.TrimSpace(nodeID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNodeNotFound
	}
	if err != nil {
		return err
	}
	if node.SessionID != strings.TrimSpace(sessionID) {
		return ErrSessionMismatch
	}
	return nil
}

func (s *Store) ClearSession(nodeID string, sessionID string) error {
	if s == nil || s.queries == nil {
		return errors.New("registry store is unavailable")
	}
	_, err := s.queries.ClearWorkerSessionByNodeAndSession(context.Background(), sqlc.ClearWorkerSessionByNodeAndSessionParams{
		NodeID:    strings.TrimSpace(nodeID),
		SessionID: strings.TrimSpace(sessionID),
	})
	return err
}

func (s *Store) ClearSessionByNode(nodeID string) error {
	if s == nil || s.queries == nil {
		return errors.New("registry store is unavailable")
	}
	_, err := s.queries.ClearWorkerSessionByNode(context.Background(), strings.TrimSpace(nodeID))
	return err
}

func (s *Store) List(status WorkerStatus, page int, pageSize int, now time.Time, offlineTTL time.Duration) ([]WorkerView, int) {
	if s == nil || s.queries == nil {
		return []WorkerView{}, 0
	}

	nodes, err := s.queries.ListWorkerNodesOrdered(context.Background())
	if err != nil {
		return []WorkerView{}, 0
	}
	capabilityRows, err := s.queries.ListWorkerCapabilitiesAll(context.Background())
	if err != nil {
		return []WorkerView{}, 0
	}
	labelRows, err := s.queries.ListWorkerLabelsAll(context.Background())
	if err != nil {
		return []WorkerView{}, 0
	}

	capabilityByNode := make(map[string][]CapabilityDeclaration, len(nodes))
	for _, row := range capabilityRows {
		capabilityByNode[row.NodeID] = append(capabilityByNode[row.NodeID], CapabilityDeclaration{
			Name:        row.CapabilityName,
			MaxInflight: int32(row.MaxInflight),
		})
	}
	labelsByNode := make(map[string]map[string]string, len(nodes))
	for _, row := range labelRows {
		if _, ok := labelsByNode[row.NodeID]; !ok {
			labelsByNode[row.NodeID] = map[string]string{}
		}
		labelsByNode[row.NodeID][row.LabelKey] = row.LabelValue
	}

	filtered := make([]WorkerView, 0, len(nodes))
	for _, node := range nodes {
		worker := Worker{
			NodeID:       node.NodeID,
			SessionID:    node.SessionID,
			Provisioned:  node.Provisioned != 0,
			NodeName:     node.NodeName,
			ExecutorKind: node.ExecutorKind,
			Capabilities: cloneCapabilities(capabilityByNode[node.NodeID]),
			Labels:       cloneMap(labelsByNode[node.NodeID]),
			Version:      node.Version,
			RegisteredAt: time.UnixMilli(node.RegisteredAtUnixMs),
			LastSeenAt:   time.UnixMilli(node.LastSeenAtUnixMs),
		}
		workerStatus := statusOf(worker.LastSeenAt, now, offlineTTL)
		if status != StatusAll && status != workerStatus {
			continue
		}
		filtered = append(filtered, WorkerView{Worker: worker, Status: workerStatus})
	}

	total := len(filtered)
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}

	start := (page - 1) * pageSize
	if start >= total {
		return []WorkerView{}, total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return filtered[start:end], total
}

func (s *Store) Stats(now time.Time, offlineTTL time.Duration, staleAfter time.Duration) WorkerStats {
	if s == nil || s.queries == nil {
		return WorkerStats{}
	}

	nodes, err := s.queries.ListWorkerNodesOrdered(context.Background())
	if err != nil {
		return WorkerStats{}
	}

	stats := WorkerStats{}
	for _, node := range nodes {
		stats.Total++
		lastSeenAt := time.UnixMilli(node.LastSeenAtUnixMs)
		workerStatus := statusOf(lastSeenAt, now, offlineTTL)
		if workerStatus == StatusOnline {
			stats.Online++
		} else {
			stats.Offline++
		}
		if now.Sub(lastSeenAt) > staleAfter {
			stats.Stale++
		}
	}
	return stats
}

func (s *Store) ListOnlineNodeIDsByCapability(capability string, now time.Time, offlineTTL time.Duration) []string {
	trimmed := strings.TrimSpace(strings.ToLower(capability))
	if trimmed == "" || s == nil || s.queries == nil {
		return []string{}
	}

	nodeIDs, err := s.queries.ListOnlineWorkerNodeIDsByCapability(context.Background(), sqlc.ListOnlineWorkerNodeIDsByCapabilityParams{
		CapabilityName:   trimmed,
		LastSeenAtUnixMs: now.Add(-offlineTTL).UnixMilli(),
	})
	if err != nil {
		return []string{}
	}

	return append([]string(nil), nodeIDs...)
}

func (s *Store) PruneOffline(now time.Time, offlineTTL time.Duration) int {
	if s == nil || s.queries == nil {
		return 0
	}

	rows, err := s.queries.DeleteOfflineRuntimeWorkers(context.Background(), now.Add(-offlineTTL).UnixMilli())
	if err != nil {
		return 0
	}
	return int(rows)
}

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

func statusOf(lastSeenAt time.Time, now time.Time, offlineTTL time.Duration) WorkerStatus {
	if now.Sub(lastSeenAt) <= offlineTTL {
		return StatusOnline
	}
	return StatusOffline
}

func cloneWorker(worker Worker) Worker {
	worker.Capabilities = cloneCapabilities(worker.Capabilities)
	worker.Labels = cloneMap(worker.Labels)
	return worker
}

func resolveProtoCapabilities(capabilities []*registryv1.CapabilityDeclaration, legacyLanguages []*registryv1.LanguageCapability) []CapabilityDeclaration {
	declared := cloneProtoCapabilities(capabilities)
	if len(declared) > 0 {
		return declared
	}
	return cloneLegacyLanguages(legacyLanguages)
}

func cloneProtoCapabilities(capabilities []*registryv1.CapabilityDeclaration) []CapabilityDeclaration {
	if len(capabilities) == 0 {
		return []CapabilityDeclaration{}
	}
	cloned := make([]CapabilityDeclaration, 0, len(capabilities))
	for _, capability := range capabilities {
		if capability == nil {
			continue
		}
		name := strings.TrimSpace(capability.GetName())
		if name == "" {
			continue
		}
		cloned = append(cloned, CapabilityDeclaration{
			Name:        name,
			MaxInflight: capability.GetMaxInflight(),
		})
	}
	return cloned
}

func cloneLegacyLanguages(languages []*registryv1.LanguageCapability) []CapabilityDeclaration {
	if len(languages) == 0 {
		return []CapabilityDeclaration{}
	}

	cloned := make([]CapabilityDeclaration, 0, len(languages))
	for _, language := range languages {
		if language == nil {
			continue
		}
		name := strings.TrimSpace(language.GetLanguage())
		if name == "" {
			continue
		}
		cloned = append(cloned, CapabilityDeclaration{
			Name:        name,
			MaxInflight: 0,
		})
	}
	return cloned
}

func cloneCapabilities(capabilities []CapabilityDeclaration) []CapabilityDeclaration {
	if len(capabilities) == 0 {
		return []CapabilityDeclaration{}
	}
	cloned := make([]CapabilityDeclaration, len(capabilities))
	copy(cloned, capabilities)
	return cloned
}

func cloneMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return map[string]string{}
	}
	cloned := make(map[string]string, len(input))
	for k, v := range input {
		cloned[k] = v
	}
	return cloned
}

func mergeLabels(base map[string]string, override map[string]string) map[string]string {
	merged := cloneMap(base)
	for key, value := range override {
		merged[key] = value
	}
	return merged
}

func shortNodeID(nodeID string) string {
	if len(nodeID) <= 8 {
		return nodeID
	}
	return nodeID[:8]
}

func hasCapability(capabilities []CapabilityDeclaration, expected string) bool {
	for _, capability := range capabilities {
		if strings.EqualFold(strings.TrimSpace(capability.Name), expected) {
			return true
		}
	}
	return false
}
