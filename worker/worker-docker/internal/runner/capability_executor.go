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
	case echoCapabilityName:
		return buildEchoCommandResult(commandID, dispatch)
	case pythonExecCapabilityName:
		return buildPythonExecCommandResult(baseCtx, commandID, dispatch)
	case terminalExecCapabilityName:
		return buildTerminalExecCommandResult(baseCtx, commandID, dispatch)
	case terminalResourceCapabilityName:
		return buildTerminalResourceCommandResult(baseCtx, commandID, dispatch)
	default:
		return commandErrorResult(commandID, "unsupported_capability", fmt.Sprintf("capability %q is not supported", dispatch.GetCapability()))
	}
}

func buildEchoCommandResult(commandID string, dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	message := ""
	resultPayload := append([]byte(nil), dispatch.GetPayloadJson()...)
	if len(resultPayload) > 0 {
		decoded := struct {
			Message string `json:"message"`
		}{}
		if err := json.Unmarshal(resultPayload, &decoded); err != nil {
			return commandErrorResult(commandID, "invalid_payload", "payload_json is not valid echo payload")
		}
		message = decoded.Message
	}
	if strings.TrimSpace(message) == "" {
		return commandErrorResult(commandID, "invalid_payload", "echo payload is required")
	}
	if len(resultPayload) == 0 {
		encoded, err := json.Marshal(struct {
			Message string `json:"message"`
		}{
			Message: message,
		})
		if err != nil {
			return commandErrorResult(commandID, "encode_failed", "failed to encode echo payload")
		}
		resultPayload = encoded
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

type pythonExecPayload struct {
	Code string `json:"code"`
}

type pythonExecResult struct {
	Output   string `json:"output"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

type pythonExecRunResult struct {
	Output   string
	Stderr   string
	ExitCode int
}

func buildPythonExecCommandResult(baseCtx context.Context, commandID string, dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	payload := append([]byte(nil), dispatch.GetPayloadJson()...)
	if len(payload) == 0 {
		return commandErrorResult(commandID, "invalid_payload", "pythonExec payload is required")
	}

	decoded := pythonExecPayload{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return commandErrorResult(commandID, "invalid_payload", "payload_json is not valid pythonExec payload")
	}
	if strings.TrimSpace(decoded.Code) == "" {
		return commandErrorResult(commandID, "invalid_payload", "pythonExec code is required")
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

	execResult, err := runPythonExec(commandCtx, decoded.Code)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return commandErrorResult(commandID, "deadline_exceeded", "command deadline exceeded")
		}
		return commandErrorResult(commandID, "execution_failed", fmt.Sprintf("pythonExec execution failed: %v", err))
	}

	resultPayload, err := json.Marshal(pythonExecResult{
		Output:   execResult.Output,
		Stderr:   execResult.Stderr,
		ExitCode: execResult.ExitCode,
	})
	if err != nil {
		return commandErrorResult(commandID, "encode_failed", "failed to encode pythonExec payload")
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

func buildTerminalExecCommandResult(baseCtx context.Context, commandID string, dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	payload := append([]byte(nil), dispatch.GetPayloadJson()...)
	if len(payload) == 0 {
		return commandErrorResult(commandID, terminalExecCodeInvalidPayload, "terminalExec payload is required")
	}

	decoded := terminalExecPayload{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return commandErrorResult(commandID, terminalExecCodeInvalidPayload, "payload_json is not valid terminalExec payload")
	}
	if strings.TrimSpace(decoded.Command) == "" {
		return commandErrorResult(commandID, terminalExecCodeInvalidPayload, "terminalExec command is required")
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

	execResult, err := runTerminalExec(commandCtx, terminalExecRequest{
		Command:         decoded.Command,
		SessionID:       decoded.SessionID,
		CreateIfMissing: decoded.CreateIfMissing,
		LeaseTTLSec:     decoded.LeaseTTLSec,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return commandErrorResult(commandID, "deadline_exceeded", "command deadline exceeded")
		}
		var terminalErr *terminalExecError
		if errors.As(err, &terminalErr) {
			return commandErrorResult(commandID, terminalErr.Code(), terminalErr.Error())
		}
		return commandErrorResult(commandID, "execution_failed", fmt.Sprintf("terminalExec execution failed: %v", err))
	}

	resultPayload, err := json.Marshal(execResult)
	if err != nil {
		return commandErrorResult(commandID, "encode_failed", "failed to encode terminalExec payload")
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

func buildTerminalResourceCommandResult(baseCtx context.Context, commandID string, dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	payload := append([]byte(nil), dispatch.GetPayloadJson()...)
	if len(payload) == 0 {
		return commandErrorResult(commandID, terminalExecCodeInvalidPayload, "terminalResource payload is required")
	}

	decoded := terminalResourcePayload{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return commandErrorResult(commandID, terminalExecCodeInvalidPayload, "payload_json is not valid terminalResource payload")
	}
	if strings.TrimSpace(decoded.SessionID) == "" || strings.TrimSpace(decoded.FilePath) == "" {
		return commandErrorResult(commandID, terminalExecCodeInvalidPayload, "terminalResource session_id and file_path are required")
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

	resourceResult, err := runTerminalResource(commandCtx, terminalResourceRequest{
		SessionID: decoded.SessionID,
		FilePath:  decoded.FilePath,
		Action:    decoded.Action,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return commandErrorResult(commandID, "deadline_exceeded", "command deadline exceeded")
		}
		var terminalErr *terminalExecError
		if errors.As(err, &terminalErr) {
			return commandErrorResult(commandID, terminalErr.Code(), terminalErr.Error())
		}
		return commandErrorResult(commandID, "execution_failed", fmt.Sprintf("terminalResource execution failed: %v", err))
	}

	resultPayload, err := json.Marshal(resourceResult)
	if err != nil {
		return commandErrorResult(commandID, "encode_failed", "failed to encode terminalResource payload")
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

func runTerminalExecUnavailable(context.Context, terminalExecRequest) (terminalExecRunResult, error) {
	return terminalExecRunResult{}, newTerminalExecError("execution_failed", terminalExecNotReadyMessage)
}

func runTerminalResourceUnavailable(context.Context, terminalResourceRequest) (terminalResourceRunResult, error) {
	return terminalResourceRunResult{}, newTerminalExecError("execution_failed", terminalExecNotReadyMessage)
}
