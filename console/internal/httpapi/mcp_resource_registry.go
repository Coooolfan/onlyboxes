package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
)

type mcpTerminalResourcePayload struct {
	SessionID string            `json:"session_id"`
	FilePath  string            `json:"file_path"`
	Action    string            `json:"action,omitempty"`
	SignedURL string            `json:"signed_url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

type mcpTerminalResourceResult struct {
	SessionID string `json:"session_id"`
	FilePath  string `json:"file_path"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	Blob      []byte `json:"blob,omitempty"`
}

func callTerminalResource(
	ctx context.Context,
	dispatcher CommandDispatcher,
	payload mcpTerminalResourcePayload,
	timeout time.Duration,
) (mcpTerminalResourceResult, error) {
	return callResourceCapability(ctx, dispatcher, terminalResourceCapabilityName, payload, timeout)
}

func callResourceCapability(
	ctx context.Context,
	dispatcher CommandDispatcher,
	capability string,
	payload mcpTerminalResourcePayload,
	timeout time.Duration,
) (mcpTerminalResourceResult, error) {
	if dispatcher == nil {
		return mcpTerminalResourceResult{}, errors.New("task dispatcher is unavailable")
	}
	capabilityName := strings.TrimSpace(capability)
	if capabilityName == "" {
		return mcpTerminalResourceResult{}, errors.New("resource capability is required")
	}

	timeoutValue := timeout
	if timeoutValue <= 0 {
		timeoutValue = time.Duration(defaultMCPTaskTimeoutMS) * time.Millisecond
	}
	ownerID := requestOwnerIDFromContext(ctx)
	if ownerID == "" {
		return mcpTerminalResourceResult{}, errors.New("request owner is required")
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return mcpTerminalResourceResult{}, fmt.Errorf("failed to encode %s payload", capabilityName)
	}

	submitResult, err := dispatcher.SubmitTask(ctx, grpcserver.SubmitTaskRequest{
		Capability: capabilityName,
		InputJSON:  payloadJSON,
		Mode:       grpcserver.TaskModeSync,
		Timeout:    timeoutValue,
		OwnerID:    ownerID,
	})
	if err != nil {
		return mcpTerminalResourceResult{}, mapMCPToolTaskSubmitError(err)
	}
	if !submitResult.Completed {
		return mcpTerminalResourceResult{}, fmt.Errorf("%s task did not complete", capabilityName)
	}

	task := submitResult.Task
	switch task.Status {
	case grpcserver.TaskStatusSucceeded:
		decoded := mcpTerminalResourceResult{}
		if err := json.Unmarshal(task.ResultJSON, &decoded); err != nil {
			return mcpTerminalResourceResult{}, fmt.Errorf("invalid %s result payload", capabilityName)
		}
		return decoded, nil
	case grpcserver.TaskStatusTimeout:
		return mcpTerminalResourceResult{}, errors.New("task timed out")
	case grpcserver.TaskStatusCanceled:
		return mcpTerminalResourceResult{}, errors.New("task canceled")
	case grpcserver.TaskStatusFailed:
		return mcpTerminalResourceResult{}, formatTaskFailureError(task)
	default:
		return mcpTerminalResourceResult{}, errors.New("unexpected task status")
	}
}

func normalizeMIME(mimeType string) string {
	normalized := strings.TrimSpace(mimeType)
	if normalized == "" {
		return "application/octet-stream"
	}
	return normalized
}

func isImageMIME(mimeType string) bool {
	normalized := strings.ToLower(strings.TrimSpace(mimeType))
	return strings.HasPrefix(normalized, "image/")
}
