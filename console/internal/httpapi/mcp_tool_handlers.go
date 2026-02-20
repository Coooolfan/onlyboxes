package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
)

func handleMCPEchoTool(ctx context.Context, dispatcher CommandDispatcher, input mcpEchoToolInput) (*mcp.CallToolResult, mcpEchoToolOutput, error) {
	if strings.TrimSpace(input.Message) == "" {
		return nil, mcpEchoToolOutput{}, invalidParamsError("message is required")
	}

	timeoutMS := defaultMCPEchoTimeoutMS
	if input.TimeoutMS != nil {
		timeoutMS = *input.TimeoutMS
	}
	if timeoutMS < minEchoTimeoutMS || timeoutMS > maxEchoTimeoutMS {
		return nil, mcpEchoToolOutput{}, invalidParamsError("timeout_ms must be between 1 and 60000")
	}
	if dispatcher == nil {
		return nil, mcpEchoToolOutput{}, errors.New("echo command dispatcher is unavailable")
	}

	result, err := dispatcher.DispatchEcho(ctx, input.Message, time.Duration(timeoutMS)*time.Millisecond)
	if err != nil {
		return nil, mcpEchoToolOutput{}, mapMCPToolEchoError(err)
	}
	return nil, mcpEchoToolOutput{Message: result}, nil
}

func handleMCPPythonExecTool(ctx context.Context, dispatcher CommandDispatcher, input mcpPythonExecToolInput) (*mcp.CallToolResult, mcpPythonExecToolOutput, error) {
	if strings.TrimSpace(input.Code) == "" {
		return nil, mcpPythonExecToolOutput{}, invalidParamsError("code is required")
	}

	timeoutMS := defaultMCPTaskTimeoutMS
	if input.TimeoutMS != nil {
		timeoutMS = *input.TimeoutMS
	}
	if timeoutMS < minMCPTaskTimeoutMS || timeoutMS > maxMCPPythonExecTimeoutMS {
		return nil, mcpPythonExecToolOutput{}, invalidParamsError("timeout_ms must be between 1 and 600000")
	}
	if dispatcher == nil {
		return nil, mcpPythonExecToolOutput{}, errors.New("task dispatcher is unavailable")
	}
	ownerID := requestOwnerIDFromContext(ctx)
	if ownerID == "" {
		return nil, mcpPythonExecToolOutput{}, errors.New("request owner is required")
	}

	payloadJSON, err := json.Marshal(pythonExecPayload{Code: input.Code})
	if err != nil {
		return nil, mcpPythonExecToolOutput{}, errors.New("failed to encode pythonExec payload")
	}

	result, err := dispatcher.SubmitTask(ctx, grpcserver.SubmitTaskRequest{
		Capability: pythonExecCapabilityName,
		InputJSON:  payloadJSON,
		Mode:       grpcserver.TaskModeSync,
		Timeout:    time.Duration(timeoutMS) * time.Millisecond,
		OwnerID:    ownerID,
	})
	if err != nil {
		return nil, mcpPythonExecToolOutput{}, mapMCPToolTaskSubmitError(err)
	}
	if !result.Completed {
		return nil, mcpPythonExecToolOutput{}, errors.New("pythonExec task did not complete")
	}

	task := result.Task
	switch task.Status {
	case grpcserver.TaskStatusSucceeded:
		decoded := mcpPythonExecToolOutput{}
		if err := json.Unmarshal(task.ResultJSON, &decoded); err != nil {
			return nil, mcpPythonExecToolOutput{}, errors.New("invalid pythonExec result payload")
		}
		return nil, decoded, nil
	case grpcserver.TaskStatusTimeout:
		return nil, mcpPythonExecToolOutput{}, errors.New("task timed out")
	case grpcserver.TaskStatusCanceled:
		return nil, mcpPythonExecToolOutput{}, errors.New("task canceled")
	case grpcserver.TaskStatusFailed:
		return nil, mcpPythonExecToolOutput{}, formatTaskFailureError(task)
	default:
		return nil, mcpPythonExecToolOutput{}, fmt.Errorf("unexpected task status: %s", task.Status)
	}
}

func handleMCPTerminalExecTool(ctx context.Context, dispatcher CommandDispatcher, input mcpTerminalExecToolInput) (*mcp.CallToolResult, mcpTerminalExecToolOutput, error) {
	if strings.TrimSpace(input.Command) == "" {
		return nil, mcpTerminalExecToolOutput{}, invalidParamsError("command is required")
	}
	if input.LeaseTTLSec != nil && *input.LeaseTTLSec < minMCPTerminalLeaseSec {
		return nil, mcpTerminalExecToolOutput{}, invalidParamsError("lease_ttl_sec must be positive")
	}

	timeoutMS := defaultMCPTaskTimeoutMS
	if input.TimeoutMS != nil {
		timeoutMS = *input.TimeoutMS
	}
	if timeoutMS < minMCPTaskTimeoutMS || timeoutMS > maxMCPPythonExecTimeoutMS {
		return nil, mcpTerminalExecToolOutput{}, invalidParamsError("timeout_ms must be between 1 and 600000")
	}
	if dispatcher == nil {
		return nil, mcpTerminalExecToolOutput{}, errors.New("task dispatcher is unavailable")
	}
	ownerID := requestOwnerIDFromContext(ctx)
	if ownerID == "" {
		return nil, mcpTerminalExecToolOutput{}, errors.New("request owner is required")
	}

	payloadJSON, err := json.Marshal(terminalExecPayload{
		Command:         input.Command,
		SessionID:       strings.TrimSpace(input.SessionID),
		CreateIfMissing: input.CreateIfMissing,
		LeaseTTLSec:     input.LeaseTTLSec,
	})
	if err != nil {
		return nil, mcpTerminalExecToolOutput{}, errors.New("failed to encode terminalExec payload")
	}

	result, err := dispatcher.SubmitTask(ctx, grpcserver.SubmitTaskRequest{
		Capability: terminalExecCapabilityName,
		InputJSON:  payloadJSON,
		Mode:       grpcserver.TaskModeSync,
		Timeout:    time.Duration(timeoutMS) * time.Millisecond,
		OwnerID:    ownerID,
	})
	if err != nil {
		return nil, mcpTerminalExecToolOutput{}, mapMCPToolTaskSubmitError(err)
	}
	if !result.Completed {
		return nil, mcpTerminalExecToolOutput{}, errors.New("terminalExec task did not complete")
	}

	task := result.Task
	switch task.Status {
	case grpcserver.TaskStatusSucceeded:
		decoded := mcpTerminalExecToolOutput{}
		if err := json.Unmarshal(task.ResultJSON, &decoded); err != nil {
			return nil, mcpTerminalExecToolOutput{}, errors.New("invalid terminalExec result payload")
		}
		return nil, decoded, nil
	case grpcserver.TaskStatusTimeout:
		return nil, mcpTerminalExecToolOutput{}, errors.New("task timed out")
	case grpcserver.TaskStatusCanceled:
		return nil, mcpTerminalExecToolOutput{}, errors.New("task canceled")
	case grpcserver.TaskStatusFailed:
		return nil, mcpTerminalExecToolOutput{}, formatTaskFailureError(task)
	default:
		return nil, mcpTerminalExecToolOutput{}, fmt.Errorf("unexpected task status: %s", task.Status)
	}
}

func handleMCPReadImageTool(ctx context.Context, dispatcher CommandDispatcher, input mcpReadImageToolInput) (*mcp.CallToolResult, any, error) {
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return nil, nil, invalidParamsError("session_id is required")
	}
	filePath := strings.TrimSpace(input.FilePath)
	if filePath == "" {
		return nil, nil, invalidParamsError("file_path is required")
	}

	timeoutMS := defaultMCPTaskTimeoutMS
	if input.TimeoutMS != nil {
		timeoutMS = *input.TimeoutMS
	}
	if timeoutMS < minMCPTaskTimeoutMS || timeoutMS > maxMCPPythonExecTimeoutMS {
		return nil, nil, invalidParamsError("timeout_ms must be between 1 and 600000")
	}
	if dispatcher == nil {
		return nil, nil, errors.New("task dispatcher is unavailable")
	}

	timeout := time.Duration(timeoutMS) * time.Millisecond
	validated, err := callTerminalResource(ctx, dispatcher, mcpTerminalResourcePayload{
		SessionID: sessionID,
		FilePath:  filePath,
		Action:    "validate",
	}, timeout)
	if err != nil {
		return nil, nil, err
	}

	mimeType := normalizeMIME(validated.MIMEType)
	if !isImageMIME(mimeType) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("unsupported mime type: %s; expected image/*", mimeType),
				},
			},
		}, nil, nil
	}

	readResult, err := callTerminalResource(ctx, dispatcher, mcpTerminalResourcePayload{
		SessionID: sessionID,
		FilePath:  filePath,
		Action:    "read",
	}, timeout)
	if err != nil {
		return nil, nil, err
	}

	readMIMEType := normalizeMIME(readResult.MIMEType)
	if !isImageMIME(readMIMEType) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("unsupported mime type: %s; expected image/*", readMIMEType),
				},
			},
		}, nil, nil
	}

	blob := append([]byte(nil), readResult.Blob...)
	if blob == nil {
		blob = []byte{}
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.ImageContent{
				MIMEType: readMIMEType,
				Data:     blob,
			},
		},
	}, nil, nil
}
