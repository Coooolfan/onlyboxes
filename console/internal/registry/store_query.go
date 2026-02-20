package registry

import (
	"context"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

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
