package registry

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

func (s *Store) List(status WorkerStatus, page int, pageSize int, now time.Time, offlineTTL time.Duration) ([]WorkerView, int) {
	return s.ListScoped(status, page, pageSize, now, offlineTTL, "", "")
}

func (s *Store) ListScoped(
	status WorkerStatus,
	page int,
	pageSize int,
	now time.Time,
	offlineTTL time.Duration,
	ownerID string,
	workerType string,
) ([]WorkerView, int) {
	filtered := s.listFilteredViews(status, now, offlineTTL, ownerID, workerType)
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

func (s *Store) listFilteredViews(
	status WorkerStatus,
	now time.Time,
	offlineTTL time.Duration,
	ownerID string,
	workerType string,
) []WorkerView {
	if s == nil || s.queries == nil {
		return []WorkerView{}
	}

	nodes, err := s.queries.ListWorkerNodesOrdered(context.Background())
	if err != nil {
		return []WorkerView{}
	}
	capabilityRows, err := s.queries.ListWorkerCapabilitiesAll(context.Background())
	if err != nil {
		return []WorkerView{}
	}
	labelRows, err := s.queries.ListWorkerLabelsAll(context.Background())
	if err != nil {
		return []WorkerView{}
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
	normalizedOwnerID := strings.TrimSpace(ownerID)
	normalizedWorkerType := normalizeWorkerType(workerType)
	for _, node := range nodes {
		worker := Worker{
			NodeID:       node.NodeID,
			SessionID:    node.SessionID,
			Provisioned:  node.Provisioned != 0,
			NodeName:     node.NodeName,
			ExecutorKind: node.ExecutorKind,
			Capabilities: slices.Clone(capabilityByNode[node.NodeID]),
			Labels:       maps.Clone(labelsByNode[node.NodeID]),
			Version:      node.Version,
			RegisteredAt: time.UnixMilli(node.RegisteredAtUnixMs),
			LastSeenAt:   time.UnixMilli(node.LastSeenAtUnixMs),
		}
		workerStatus := statusOf(worker.LastSeenAt, now, offlineTTL)
		if status != StatusAll && status != workerStatus {
			continue
		}
		if normalizedOwnerID != "" {
			if strings.TrimSpace(worker.Labels[LabelOwnerIDKey]) != normalizedOwnerID {
				continue
			}
			if normalizedWorkerType != "" && resolveWorkerType(worker.Labels) != normalizedWorkerType {
				continue
			}
		}
		filtered = append(filtered, WorkerView{Worker: worker, Status: workerStatus})
	}
	return filtered
}

func (s *Store) Stats(now time.Time, offlineTTL time.Duration, staleAfter time.Duration) WorkerStats {
	return s.StatsScoped(now, offlineTTL, staleAfter, "", "")
}

func (s *Store) GetByNodeID(nodeID string, now time.Time, offlineTTL time.Duration) (WorkerView, bool) {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" || s == nil || s.queries == nil {
		return WorkerView{}, false
	}

	node, err := s.queries.GetWorkerNodeByID(context.Background(), trimmedNodeID)
	if err != nil {
		return WorkerView{}, false
	}
	capabilityRows, err := s.queries.ListWorkerCapabilitiesByNode(context.Background(), trimmedNodeID)
	if err != nil {
		return WorkerView{}, false
	}
	labelRows, err := s.queries.ListWorkerLabelsByNode(context.Background(), trimmedNodeID)
	if err != nil {
		return WorkerView{}, false
	}

	capabilities := make([]CapabilityDeclaration, 0, len(capabilityRows))
	for _, row := range capabilityRows {
		capabilities = append(capabilities, CapabilityDeclaration{
			Name:        row.CapabilityName,
			MaxInflight: int32(row.MaxInflight),
		})
	}
	labels := make(map[string]string, len(labelRows))
	for _, row := range labelRows {
		labels[row.LabelKey] = row.LabelValue
	}

	worker := Worker{
		NodeID:       node.NodeID,
		SessionID:    node.SessionID,
		Provisioned:  node.Provisioned != 0,
		NodeName:     node.NodeName,
		ExecutorKind: node.ExecutorKind,
		Capabilities: capabilities,
		Labels:       labels,
		Version:      node.Version,
		RegisteredAt: time.UnixMilli(node.RegisteredAtUnixMs),
		LastSeenAt:   time.UnixMilli(node.LastSeenAtUnixMs),
	}
	return WorkerView{
		Worker: worker,
		Status: statusOf(worker.LastSeenAt, now, offlineTTL),
	}, true
}

func (s *Store) LabelsByNodeID(nodeID string) map[string]string {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" || s == nil || s.queries == nil {
		return map[string]string{}
	}

	labelRows, err := s.queries.ListWorkerLabelsByNode(context.Background(), trimmedNodeID)
	if err != nil {
		return map[string]string{}
	}
	labels := make(map[string]string, len(labelRows))
	for _, row := range labelRows {
		labels[row.LabelKey] = row.LabelValue
	}
	return labels
}

func (s *Store) WorkerTypeByNodeID(nodeID string) string {
	return resolveWorkerType(s.LabelsByNodeID(nodeID))
}

func (s *Store) StatsScoped(
	now time.Time,
	offlineTTL time.Duration,
	staleAfter time.Duration,
	ownerID string,
	workerType string,
) WorkerStats {
	workers := s.listFilteredViews(StatusAll, now, offlineTTL, ownerID, workerType)
	stats := WorkerStats{}
	for _, worker := range workers {
		stats.Total++
		if worker.Status == StatusOnline {
			stats.Online++
		} else {
			stats.Offline++
		}
		if now.Sub(worker.LastSeenAt) > staleAfter {
			stats.Stale++
		}
	}
	return stats
}

func (s *Store) ListNodeIDsByOwnerAndType(ownerID string, workerType string) []string {
	trimmedOwnerID := strings.TrimSpace(ownerID)
	normalizedWorkerType := normalizeWorkerType(workerType)
	if trimmedOwnerID == "" || normalizedWorkerType == "" || s == nil || s.queries == nil {
		return []string{}
	}

	nodeIDs, err := s.queries.ListWorkerNodeIDsByOwnerAndType(context.Background(), sqlc.ListWorkerNodeIDsByOwnerAndTypeParams{
		LabelValue:   trimmedOwnerID,
		LabelValue_2: normalizedWorkerType,
	})
	if err != nil {
		return []string{}
	}
	return append([]string(nil), nodeIDs...)
}

func (s *Store) CountWorkersByOwnerAndType(ownerID string, workerType string) int {
	trimmedOwnerID := strings.TrimSpace(ownerID)
	normalizedWorkerType := normalizeWorkerType(workerType)
	if trimmedOwnerID == "" || normalizedWorkerType == "" || s == nil || s.queries == nil {
		return 0
	}

	count, err := s.queries.CountWorkerNodesByOwnerAndType(context.Background(), sqlc.CountWorkerNodesByOwnerAndTypeParams{
		LabelValue:   trimmedOwnerID,
		LabelValue_2: normalizedWorkerType,
	})
	if err != nil {
		return 0
	}
	return int(count)
}

func (s *Store) ClaimWorkerSysOwner(ownerID string, nodeID string, now time.Time) (bool, error) {
	trimmedOwnerID := strings.TrimSpace(ownerID)
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedOwnerID == "" || trimmedNodeID == "" {
		return false, errors.New("owner_id and node_id are required")
	}
	if s == nil || s.queries == nil {
		return false, errors.New("registry store is unavailable")
	}

	rows, err := s.queries.InsertWorkerSysOwnerClaimIfAbsent(context.Background(), sqlc.InsertWorkerSysOwnerClaimIfAbsentParams{
		OwnerID:         trimmedOwnerID,
		NodeID:          trimmedNodeID,
		ClaimedAtUnixMs: now.UnixMilli(),
	})
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

func (s *Store) ListOnlineNodeIDsByOwnerTypeAndCapability(
	ownerID string,
	workerType string,
	capability string,
	now time.Time,
	offlineTTL time.Duration,
) []string {
	trimmedOwnerID := strings.TrimSpace(ownerID)
	normalizedWorkerType := normalizeWorkerType(workerType)
	normalizedCapability := normalizeCapabilityName(capability)
	if trimmedOwnerID == "" || normalizedWorkerType == "" || normalizedCapability == "" || s == nil || s.queries == nil {
		return []string{}
	}

	nodeIDs, err := s.queries.ListOnlineWorkerNodeIDsByOwnerTypeAndCapability(
		context.Background(),
		sqlc.ListOnlineWorkerNodeIDsByOwnerTypeAndCapabilityParams{
			CapabilityName:   normalizedCapability,
			LabelValue:       trimmedOwnerID,
			LabelValue_2:     normalizedWorkerType,
			LastSeenAtUnixMs: now.Add(-offlineTTL).UnixMilli(),
		},
	)
	if err != nil {
		return []string{}
	}
	return append([]string(nil), nodeIDs...)
}

func (s *Store) ListOnlineNodeIDsByCapability(capability string, now time.Time, offlineTTL time.Duration) []string {
	trimmed := normalizeCapabilityName(capability)
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
