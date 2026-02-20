package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

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

	capabilities := resolveProtoCapabilities(req.GetCapabilities())
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
