package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

var runComputerUse = runComputerUseUnavailable
var runReadImage = runReadImageUnavailable

func buildCommandResult(dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	return buildCommandResultWithContext(context.Background(), dispatch)
}

func buildCommandResultWithContext(baseCtx context.Context, dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	commandID := strings.TrimSpace(dispatch.GetCommandId())
	if commandID == "" {
		return &registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_CommandResult{
				CommandResult: &registryv1.CommandResult{
					CommandId: commandID,
					Error: &registryv1.CommandError{
						Code:    "invalid_command_id",
						Message: "command_id is required",
					},
				},
			},
		}
	}

	if deadline := dispatch.GetDeadlineUnixMs(); deadline > 0 && time.Now().UnixMilli() > deadline {
		return commandErrorResult(commandID, "deadline_exceeded", "command deadline exceeded")
	}

	capability := strings.TrimSpace(strings.ToLower(dispatch.GetCapability()))
	switch capability {
	case computerUseCapabilityName:
		return buildComputerUseCommandResult(baseCtx, commandID, dispatch)
	case readImageCapabilityName:
		return buildReadImageCommandResult(baseCtx, commandID, dispatch)
	default:
		return commandErrorResult(commandID, "unsupported_capability", fmt.Sprintf("capability %q is not supported", dispatch.GetCapability()))
	}
}

func buildComputerUseCommandResult(baseCtx context.Context, commandID string, dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	payload := append([]byte(nil), dispatch.GetPayloadJson()...)
	if len(payload) == 0 {
		return commandErrorResult(commandID, computerUseCodeInvalidPayload, "computerUse payload is required")
	}

	decoded := computerUsePayload{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return commandErrorResult(commandID, computerUseCodeInvalidPayload, "payload_json is not valid computerUse payload")
	}
	if strings.TrimSpace(decoded.Command) == "" {
		return commandErrorResult(commandID, computerUseCodeInvalidPayload, "computerUse command is required")
	}

	commandCtx := baseCtx
	if commandCtx == nil {
		commandCtx = context.Background()
	}
	cancel := func() {}
	if deadlineUnixMS := dispatch.GetDeadlineUnixMs(); deadlineUnixMS > 0 {
		commandCtx, cancel = context.WithDeadline(commandCtx, time.UnixMilli(deadlineUnixMS))
	}
	defer cancel()

	execResult, err := runComputerUse(commandCtx, computerUseRequest{Command: decoded.Command})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return commandErrorResult(commandID, "deadline_exceeded", "command deadline exceeded")
		}
		var computerUseErr *computerUseError
		if errors.As(err, &computerUseErr) {
			return commandErrorResult(commandID, computerUseErr.Code(), computerUseErr.Error())
		}
		return commandErrorResult(commandID, "execution_failed", fmt.Sprintf("computerUse execution failed: %v", err))
	}

	resultPayload, err := json.Marshal(execResult)
	if err != nil {
		return commandErrorResult(commandID, "encode_failed", "failed to encode computerUse payload")
	}

	return &registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_CommandResult{
			CommandResult: &registryv1.CommandResult{
				CommandId:       commandID,
				PayloadJson:     resultPayload,
				CompletedUnixMs: time.Now().UnixMilli(),
			},
		},
	}
}

func buildReadImageCommandResult(baseCtx context.Context, commandID string, dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	payload := append([]byte(nil), dispatch.GetPayloadJson()...)
	if len(payload) == 0 {
		return commandErrorResult(commandID, readImageCodeInvalidPayload, "readImage payload is required")
	}

	decoded := readImagePayload{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return commandErrorResult(commandID, readImageCodeInvalidPayload, "payload_json is not valid readImage payload")
	}
	if strings.TrimSpace(decoded.SessionID) == "" || strings.TrimSpace(decoded.FilePath) == "" {
		return commandErrorResult(commandID, readImageCodeInvalidPayload, "readImage session_id and file_path are required")
	}

	commandCtx := baseCtx
	if commandCtx == nil {
		commandCtx = context.Background()
	}
	cancel := func() {}
	if deadlineUnixMS := dispatch.GetDeadlineUnixMs(); deadlineUnixMS > 0 {
		commandCtx, cancel = context.WithDeadline(commandCtx, time.UnixMilli(deadlineUnixMS))
	}
	defer cancel()

	execResult, err := runReadImage(commandCtx, readImageRequest{
		SessionID: decoded.SessionID,
		FilePath:  decoded.FilePath,
		Action:    decoded.Action,
		SignedURL: decoded.SignedURL,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return commandErrorResult(commandID, "deadline_exceeded", "command deadline exceeded")
		}
		var readImageErr *readImageError
		if errors.As(err, &readImageErr) {
			return commandErrorResult(commandID, readImageErr.Code(), readImageErr.Error())
		}
		return commandErrorResult(commandID, "execution_failed", fmt.Sprintf("readImage execution failed: %v", err))
	}

	resultPayload, err := json.Marshal(execResult)
	if err != nil {
		return commandErrorResult(commandID, "encode_failed", "failed to encode readImage payload")
	}

	return &registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_CommandResult{
			CommandResult: &registryv1.CommandResult{
				CommandId:       commandID,
				PayloadJson:     resultPayload,
				CompletedUnixMs: time.Now().UnixMilli(),
			},
		},
	}
}

func commandErrorResult(commandID string, code string, message string) *registryv1.ConnectRequest {
	return &registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_CommandResult{
			CommandResult: &registryv1.CommandResult{
				CommandId:       commandID,
				CompletedUnixMs: time.Now().UnixMilli(),
				Error: &registryv1.CommandError{
					Code:    code,
					Message: message,
				},
			},
		},
	}
}

func runComputerUseUnavailable(context.Context, computerUseRequest) (computerUseRunResult, error) {
	return computerUseRunResult{}, newComputerUseError("execution_failed", computerUseNotReadyMessage)
}

func runReadImageUnavailable(context.Context, readImageRequest) (readImageRunResult, error) {
	return readImageRunResult{}, newReadImageError("execution_failed", readImageNotReadyMessage)
}
