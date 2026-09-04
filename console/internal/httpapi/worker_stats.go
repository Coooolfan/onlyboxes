package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
)

const defaultStaleAfterSec = 30

type InflightStatsProvider interface {
	InflightStats() []grpcserver.WorkerInflightSnapshot
}

type workerStatsResponse struct {
	Total         int       `json:"total"`
	Online        int       `json:"online"`
	Offline       int       `json:"offline"`
	Stale         int       `json:"stale"`
	StaleAfterSec int       `json:"stale_after_sec"`
	GeneratedAt   time.Time `json:"generated_at"`
}

type capabilityInflightJSON struct {
	Name        string `json:"name"`
	Inflight    int    `json:"inflight"`
	MaxInflight int    `json:"max_inflight"`
}

type terminalSessionCapacityJSON struct {
	MaxActiveSessions int `json:"max_active_sessions"`
}

type workerInflightJSON struct {
	NodeID                  string                      `json:"node_id"`
	ActiveSessionCount      int                         `json:"active_session_count"`
	TerminalSessionCapacity terminalSessionCapacityJSON `json:"terminal_session_capacity"`
	Capabilities            []capabilityInflightJSON    `json:"capabilities"`
}

type workerInflightResponse struct {
	Workers     []workerInflightJSON `json:"workers"`
	GeneratedAt time.Time            `json:"generated_at"`
}

func (h *WorkerHandler) WorkerStats(c *gin.Context) {
	ownerID, isAdmin, ok := resolveWorkerAccessScope(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	staleAfterSec, ok := parsePositiveIntQuery(c, "stale_after_sec", defaultStaleAfterSec)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "stale_after_sec must be a positive integer"})
		return
	}

	now := h.nowFn()
	var stats registry.WorkerStats
	if isAdmin {
		stats = h.store.Stats(now, h.offlineTTL, time.Duration(staleAfterSec)*time.Second)
	} else {
		stats = h.store.StatsScoped(
			now,
			h.offlineTTL,
			time.Duration(staleAfterSec)*time.Second,
			ownerID,
			registry.WorkerTypeSys,
		)
	}
	c.JSON(http.StatusOK, workerStatsResponse{
		Total:         stats.Total,
		Online:        stats.Online,
		Offline:       stats.Offline,
		Stale:         stats.Stale,
		StaleAfterSec: staleAfterSec,
		GeneratedAt:   now,
	})
}

func (h *WorkerHandler) WorkerInflight(c *gin.Context) {
	ownerID, isAdmin, ok := resolveWorkerAccessScope(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	if h.inflightStats == nil {
		c.JSON(http.StatusOK, workerInflightResponse{
			Workers:     []workerInflightJSON{},
			GeneratedAt: h.nowFn(),
		})
		return
	}

	snapshots := h.inflightStats.InflightStats()
	allowedNodeIDs := map[string]struct{}{}
	if !isAdmin {
		for _, nodeID := range h.store.ListNodeIDsByOwnerAndType(ownerID, registry.WorkerTypeSys) {
			allowedNodeIDs[strings.TrimSpace(nodeID)] = struct{}{}
		}
	}

	workers := make([]workerInflightJSON, 0, len(snapshots))
	for _, snap := range snapshots {
		if !isAdmin {
			if _, ok := allowedNodeIDs[strings.TrimSpace(snap.NodeID)]; !ok {
				continue
			}
		}
		entries := make([]capabilityInflightJSON, len(snap.Capabilities))
		for j, entry := range snap.Capabilities {
			entries[j] = capabilityInflightJSON{
				Name:        entry.Name,
				Inflight:    entry.Inflight,
				MaxInflight: entry.MaxInflight,
			}
		}
		workers = append(workers, workerInflightJSON{
			NodeID:             snap.NodeID,
			ActiveSessionCount: snap.ActiveSessionCount,
			TerminalSessionCapacity: terminalSessionCapacityJSON{
				MaxActiveSessions: snap.TerminalSessionCapacity.MaxActiveSessions,
			},
			Capabilities: entries,
		})
	}
	c.JSON(http.StatusOK, workerInflightResponse{
		Workers:     workers,
		GeneratedAt: h.nowFn(),
	})
}
