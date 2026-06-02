package httpapi

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
)

const (
	sandboxProviderName       = "onlyboxes"
	sandboxMetadataAPIVersion = "2026-05-25"
	defaultTerminalLeaseSec   = 60
)

type sandboxMetadataResponse struct {
	Provider     string                       `json:"provider"`
	APIVersion   string                       `json:"api_version"`
	Console      sandboxMetadataConsole       `json:"console"`
	Limits       sandboxMetadataLimits        `json:"limits"`
	Capabilities []sandboxMetadataCapability  `json:"capabilities"`
	Workers      sandboxMetadataWorkerSummary `json:"workers"`
}

type sandboxMetadataConsole struct {
	Version string `json:"version"`
}

type sandboxMetadataLimits struct {
	MaxTaskTimeoutMS        int `json:"max_task_timeout_ms"`
	MaxTaskWaitMS           int `json:"max_task_wait_ms"`
	MaxTerminalTimeoutMS    int `json:"max_terminal_timeout_ms"`
	DefaultTerminalLeaseSec int `json:"default_terminal_lease_sec"`
	MaxTerminalLeaseSec     int `json:"max_terminal_lease_sec"`
}

type sandboxMetadataCapability struct {
	Name        string `json:"name"`
	Available   bool   `json:"available"`
	OnlineNodes int    `json:"online_nodes"`
	MaxInflight int    `json:"max_inflight,omitempty"`
}

type sandboxMetadataWorkerSummary struct {
	Total   int `json:"total"`
	Online  int `json:"online"`
	Offline int `json:"offline"`
	Stale   int `json:"stale"`
}

func (h *WorkerHandler) SandboxMetadata(c *gin.Context) {
	ownerID, ok := requireRequestOwnerID(c)
	if !ok {
		return
	}
	if h == nil || h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "worker registry is unavailable"})
		return
	}

	now := h.nowFn()
	offlineTTL := h.offlineTTL
	stats := h.store.Stats(now, offlineTTL, time.Duration(defaultStaleAfterSec)*time.Second)
	response := sandboxMetadataResponse{
		Provider:   sandboxProviderName,
		APIVersion: sandboxMetadataAPIVersion,
		Console: sandboxMetadataConsole{
			Version: consoleVersion(),
		},
		Limits: sandboxMetadataLimits{
			MaxTaskTimeoutMS:        maxTaskTimeoutMS,
			MaxTaskWaitMS:           maxTaskWaitMS,
			MaxTerminalTimeoutMS:    maxTerminalTimeoutMS,
			DefaultTerminalLeaseSec: defaultTerminalLeaseSec,
			MaxTerminalLeaseSec:     maxMCPTerminalLeaseSec,
		},
		Capabilities: h.sandboxCapabilityMetadata(ownerID, now, offlineTTL),
		Workers: sandboxMetadataWorkerSummary{
			Total:   stats.Total,
			Online:  stats.Online,
			Offline: stats.Offline,
			Stale:   stats.Stale,
		},
	}
	c.JSON(http.StatusOK, response)
}

func (h *WorkerHandler) sandboxCapabilityMetadata(ownerID string, now time.Time, offlineTTL time.Duration) []sandboxMetadataCapability {
	capabilities := []string{
		"echo",
		"pythonExec",
		"terminalExec",
		"terminalResource",
		"computerUse",
		"readImage",
	}

	out := make([]sandboxMetadataCapability, 0, len(capabilities))
	for _, capability := range capabilities {
		onlineNodes := h.onlineNodeIDsForCapability(ownerID, capability, now, offlineTTL)
		out = append(out, sandboxMetadataCapability{
			Name:        capability,
			Available:   len(onlineNodes) > 0,
			OnlineNodes: len(onlineNodes),
			MaxInflight: h.maxInflightForCapability(onlineNodes, capability, now, offlineTTL),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func (h *WorkerHandler) onlineNodeIDsForCapability(ownerID string, capability string, now time.Time, offlineTTL time.Duration) []string {
	normalizedCapability := normalizeToolCapabilityKey(capability)
	switch normalizedCapability {
	case "computeruse", "readimage":
		return h.store.ListOnlineNodeIDsByOwnerTypeAndCapability(
			strings.TrimSpace(ownerID),
			registry.WorkerTypeSys,
			normalizedCapability,
			now,
			offlineTTL,
		)
	default:
		return h.store.ListOnlineNodeIDsByCapability(normalizedCapability, now, offlineTTL)
	}
}

func (h *WorkerHandler) maxInflightForCapability(nodeIDs []string, capability string, now time.Time, offlineTTL time.Duration) int {
	maxInflight := 0
	normalizedCapability := normalizeToolCapabilityKey(capability)
	for _, nodeID := range nodeIDs {
		worker, ok := h.store.GetByNodeID(nodeID, now, offlineTTL)
		if !ok {
			continue
		}
		for _, capDecl := range worker.Capabilities {
			if normalizeToolCapabilityKey(capDecl.Name) != normalizedCapability {
				continue
			}
			if int(capDecl.MaxInflight) > maxInflight {
				maxInflight = int(capDecl.MaxInflight)
			}
		}
	}
	return maxInflight
}
