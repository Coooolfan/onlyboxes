package grpcserver

import (
	"encoding/json"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	taskOwnerScopePrefix                = "obx"
	taskOwnerScopeSeparator             = ":"
	taskRequestScopeSeparator           = "\x00"
	taskCapabilityTerminalExec          = "terminalexec"
	taskCapabilityTerminalResource      = "terminalresource"
	taskOwnerScopeInvalidPayloadCode    = "invalid_payload"
	taskOwnerScopeInvalidPayloadMessage = "session_id owner mismatch"
)

type terminalExecScopedPayload struct {
	Command         string `json:"command"`
	SessionID       string `json:"session_id,omitempty"`
	CreateIfMissing bool   `json:"create_if_missing,omitempty"`
	LeaseTTLSec     *int   `json:"lease_ttl_sec,omitempty"`
}

type terminalResourceScopedPayload struct {
	SessionID string `json:"session_id"`
	FilePath  string `json:"file_path"`
	Action    string `json:"action,omitempty"`
}

func normalizeTaskOwnerID(ownerID string) string {
	return strings.TrimSpace(ownerID)
}

func taskRequestScopeKey(ownerID string, requestID string) string {
	return normalizeTaskOwnerID(ownerID) + taskRequestScopeSeparator + strings.TrimSpace(requestID)
}

func scopeTerminalSessionID(ownerID string, externalSessionID string) string {
	normalizedOwnerID := normalizeTaskOwnerID(ownerID)
	normalizedSessionID := strings.TrimSpace(externalSessionID)
	if normalizedOwnerID == "" || normalizedSessionID == "" {
		return normalizedSessionID
	}
	return strings.Join([]string{
		taskOwnerScopePrefix,
		normalizedOwnerID,
		normalizedSessionID,
	}, taskOwnerScopeSeparator)
}

func unscopeTerminalSessionID(ownerID string, scopedSessionID string) (string, bool) {
	normalizedOwnerID := normalizeTaskOwnerID(ownerID)
	normalizedSessionID := strings.TrimSpace(scopedSessionID)
	if normalizedSessionID == "" {
		return "", false
	}
	if normalizedOwnerID == "" {
		return normalizedSessionID, true
	}
	prefix := strings.Join([]string{
		taskOwnerScopePrefix,
		normalizedOwnerID,
	}, taskOwnerScopeSeparator) + taskOwnerScopeSeparator
	if !strings.HasPrefix(normalizedSessionID, prefix) {
		return "", false
	}
	externalSessionID := strings.TrimSpace(strings.TrimPrefix(normalizedSessionID, prefix))
	if externalSessionID == "" {
		return "", false
	}
	return externalSessionID, true
}

func (s *RegistryService) scopeTaskInputByOwner(capability string, ownerID string, inputJSON []byte) ([]byte, error) {
	normalizedOwnerID := normalizeTaskOwnerID(ownerID)
	if normalizedOwnerID == "" || len(inputJSON) == 0 {
		return inputJSON, nil
	}

	switch capability {
	case taskCapabilityTerminalExec:
		payload := terminalExecScopedPayload{}
		if err := json.Unmarshal(inputJSON, &payload); err != nil {
			return inputJSON, nil
		}
		sessionID := strings.TrimSpace(payload.SessionID)
		if sessionID == "" {
			if s == nil || s.newTerminalSessionIDFn == nil {
				return nil, status.Error(codes.Internal, "failed to create session_id")
			}
			createdSessionID, err := s.newTerminalSessionIDFn()
			if err != nil {
				return nil, status.Error(codes.Internal, "failed to create session_id")
			}
			sessionID = strings.TrimSpace(createdSessionID)
			if sessionID == "" {
				return nil, status.Error(codes.Internal, "failed to create session_id")
			}
			payload.CreateIfMissing = true
		}
		payload.SessionID = scopeTerminalSessionID(normalizedOwnerID, sessionID)
		scopedPayload, err := json.Marshal(payload)
		if err != nil {
			return nil, status.Error(codes.Internal, "failed to encode terminalExec payload")
		}
		return scopedPayload, nil
	case taskCapabilityTerminalResource:
		payload := terminalResourceScopedPayload{}
		if err := json.Unmarshal(inputJSON, &payload); err != nil {
			return inputJSON, nil
		}
		sessionID := strings.TrimSpace(payload.SessionID)
		if sessionID == "" {
			return inputJSON, nil
		}
		payload.SessionID = scopeTerminalSessionID(normalizedOwnerID, sessionID)
		scopedPayload, err := json.Marshal(payload)
		if err != nil {
			return nil, status.Error(codes.Internal, "failed to encode terminalResource payload")
		}
		return scopedPayload, nil
	default:
		return inputJSON, nil
	}
}

func (s *RegistryService) restoreTaskResultOwnerScope(ownerID string, capability string, resultJSON []byte) ([]byte, bool) {
	normalizedOwnerID := normalizeTaskOwnerID(ownerID)
	if normalizedOwnerID == "" || len(resultJSON) == 0 {
		return resultJSON, true
	}
	if capability != taskCapabilityTerminalExec && capability != taskCapabilityTerminalResource {
		return resultJSON, true
	}

	decoded := map[string]any{}
	if err := json.Unmarshal(resultJSON, &decoded); err != nil {
		return resultJSON, true
	}

	sessionIDRaw, ok := decoded["session_id"]
	if !ok {
		return resultJSON, true
	}
	sessionID, ok := sessionIDRaw.(string)
	if !ok {
		return resultJSON, true
	}

	externalSessionID, scopedOK := unscopeTerminalSessionID(normalizedOwnerID, sessionID)
	if !scopedOK {
		// Defensive failure: worker returned a session_id outside the owner scope
		// established when the task was submitted. This blocks mismatched results
		// from being surfaced as successful task output.
		return nil, false
	}
	decoded["session_id"] = externalSessionID

	restoredJSON, err := json.Marshal(decoded)
	if err != nil {
		return nil, false
	}
	return restoredJSON, true
}
