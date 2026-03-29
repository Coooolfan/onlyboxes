package httpapi

import "strings"

var publicCapabilities = map[string]bool{
	"echo":         true,
	"pythonexec":   true,
	"terminalexec": true,
	"computeruse":  true,
	"readimage":    true,
}

func normalizeToolCapabilityKey(value string) string {
	return strings.TrimSpace(strings.ToLower(value))
}

func isPublicCapability(capability string) bool {
	return publicCapabilities[normalizeToolCapabilityKey(capability)]
}

func isCapabilityHidden(hiddenTools map[string]bool, capability string) bool {
	key := normalizeToolCapabilityKey(capability)
	if key == "" {
		return false
	}
	return hiddenTools[key]
}
