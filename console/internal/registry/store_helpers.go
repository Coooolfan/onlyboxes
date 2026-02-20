package registry

import (
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

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

func resolveProtoCapabilities(capabilities []*registryv1.CapabilityDeclaration) []CapabilityDeclaration {
	return cloneProtoCapabilities(capabilities)
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

func normalizeCapabilityName(capability string) string {
	return strings.TrimSpace(strings.ToLower(capability))
}
