package registry

import (
	"maps"
	"slices"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

const (
	LabelOwnerIDKey    = "obx.owner_id"
	LabelWorkerTypeKey = "obx.worker_type"

	WorkerTypeNormal = "normal"
	WorkerTypeSys    = "worker-sys"
)

func statusOf(lastSeenAt time.Time, now time.Time, offlineTTL time.Duration) WorkerStatus {
	if now.Sub(lastSeenAt) <= offlineTTL {
		return StatusOnline
	}
	return StatusOffline
}

func cloneWorker(worker Worker) Worker {
	worker.Capabilities = slices.Clone(worker.Capabilities)
	worker.Labels = maps.Clone(worker.Labels)
	return worker
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

func mergeLabels(base map[string]string, override map[string]string) map[string]string {
	merged := maps.Clone(base)
	if merged == nil {
		merged = map[string]string{}
	}
	for key, value := range override {
		merged[key] = value
	}
	return merged
}

func mergeLabelsPreserveKeys(base map[string]string, override map[string]string, protectedKeys ...string) map[string]string {
	merged := mergeLabels(base, override)
	for _, key := range protectedKeys {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		baseValue, ok := base[trimmedKey]
		if !ok {
			continue
		}
		merged[trimmedKey] = baseValue
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

func normalizeWorkerType(workerType string) string {
	return strings.TrimSpace(strings.ToLower(workerType))
}

func resolveWorkerType(labels map[string]string) string {
	value := ""
	if labels != nil {
		value = labels[LabelWorkerTypeKey]
	}
	return normalizeWorkerType(value)
}
