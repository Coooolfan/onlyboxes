package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
)

type mcpTerminalResourcePayload struct {
	SessionID string `json:"session_id"`
	FilePath  string `json:"file_path"`
	Action    string `json:"action,omitempty"`
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
	if dispatcher == nil {
		return mcpTerminalResourceResult{}, errors.New("task dispatcher is unavailable")
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
		return mcpTerminalResourceResult{}, errors.New("failed to encode terminalResource payload")
	}

	submitResult, err := dispatcher.SubmitTask(ctx, grpcserver.SubmitTaskRequest{
		Capability: terminalResourceCapabilityName,
		InputJSON:  payloadJSON,
		Mode:       grpcserver.TaskModeSync,
		Timeout:    timeoutValue,
		OwnerID:    ownerID,
	})
	if err != nil {
		return mcpTerminalResourceResult{}, mapMCPToolTaskSubmitError(err)
	}
	if !submitResult.Completed {
		return mcpTerminalResourceResult{}, errors.New("terminalResource task did not complete")
	}

	task := submitResult.Task
	switch task.Status {
	case grpcserver.TaskStatusSucceeded:
		decoded := mcpTerminalResourceResult{}
		if err := json.Unmarshal(task.ResultJSON, &decoded); err != nil {
			return mcpTerminalResourceResult{}, errors.New("invalid terminalResource result payload")
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
