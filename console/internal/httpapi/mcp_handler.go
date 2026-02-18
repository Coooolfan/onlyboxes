package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	mcpServerName              = "onlyboxes-console"
	mcpServerVersion           = "v0.1.0"
	pythonExecCapabilityName   = "pythonExec"
	terminalExecCapabilityName = "terminalExec"
	defaultMCPEchoTimeoutMS    = defaultEchoTimeoutMS
	minMCPTaskTimeoutMS        = 1
	defaultMCPTaskTimeoutMS    = defaultTaskTimeoutMS
	maxMCPPythonExecTimeoutMS  = maxTaskTimeoutMS
	minMCPTerminalLeaseSec     = 1
	maxMCPTerminalLeaseSec     = 86400
	mcpEchoToolTitle           = "Echo Message"
	mcpPythonExecToolTitle     = "Python Execute"
	mcpTerminalExecToolTitle   = "Terminal Execute"
)

type mcpEchoToolInput struct {
	Message   string `json:"message"`
	TimeoutMS *int   `json:"timeout_ms,omitempty"`
}

type mcpEchoToolOutput struct {
	Message string `json:"message"`
}

type mcpPythonExecToolInput struct {
	Code      string `json:"code"`
	TimeoutMS *int   `json:"timeout_ms,omitempty"`
}

type mcpPythonExecToolOutput struct {
	Output   string `json:"output"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

type mcpTerminalExecToolInput struct {
	Command         string `json:"command"`
	SessionID       string `json:"session_id,omitempty"`
	CreateIfMissing bool   `json:"create_if_missing,omitempty"`
	LeaseTTLSec     *int   `json:"lease_ttl_sec,omitempty"`
	TimeoutMS       *int   `json:"timeout_ms,omitempty"`
}

type mcpTerminalExecToolOutput struct {
	SessionID          string `json:"session_id"`
	Created            bool   `json:"created"`
	Stdout             string `json:"stdout"`
	Stderr             string `json:"stderr"`
	ExitCode           int    `json:"exit_code"`
	StdoutTruncated    bool   `json:"stdout_truncated"`
	StderrTruncated    bool   `json:"stderr_truncated"`
	LeaseExpiresUnixMS int64  `json:"lease_expires_unix_ms"`
}

type pythonExecPayload struct {
	Code string `json:"code"`
}

var mcpEchoToolDescription = "Echoes the input message exactly as returned by an online worker supporting the echo capability. Use this tool for connectivity checks, request tracing, and latency baselines. Do not use it for code execution, file operations, or long-running work. timeout_ms is an end-to-end dispatch timeout in milliseconds (1-60000, default 5000)."

var mcpPythonExecToolDescription = "Executes Python code in the worker sandbox via the pythonExec capability and returns stdout, stderr, and exit_code. Use this for short, self-contained snippets. Do not use it for long-running jobs or persistent state. timeout_ms is a synchronous execution timeout in milliseconds (1-600000, default 60000). A non-zero exit_code is returned as normal tool output, not as a protocol error."

var mcpTerminalExecToolDescription = "Executes shell commands in a persistent Docker-backed terminal session via the terminalExec capability. Reuse session_id to preserve filesystem state across calls. create_if_missing controls missing-session behavior. lease_ttl_sec extends session lease within worker-configured bounds. timeout_ms is a synchronous execution timeout in milliseconds (1-600000, default 60000)."

var mcpEchoInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"message"},
	"properties": map[string]any{
		"message": map[string]any{
			"type":        "string",
			"description": "Message to be echoed back unchanged. Empty or whitespace-only values are rejected.",
		},
		"timeout_ms": map[string]any{
			"type":        "integer",
			"description": "Optional end-to-end dispatch timeout in milliseconds for the echo capability.",
			"minimum":     minEchoTimeoutMS,
			"maximum":     maxEchoTimeoutMS,
			"default":     defaultMCPEchoTimeoutMS,
		},
	},
}

var mcpEchoOutputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"message"},
	"properties": map[string]any{
		"message": map[string]any{
			"type":        "string",
			"description": "Echoed message returned by the worker.",
		},
	},
}

var mcpPythonExecInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"code"},
	"properties": map[string]any{
		"code": map[string]any{
			"type":        "string",
			"description": "Python source code to execute in the worker sandbox. Empty or whitespace-only values are rejected.",
		},
		"timeout_ms": map[string]any{
			"type":        "integer",
			"description": "Optional synchronous execution timeout in milliseconds for this tool call.",
			"minimum":     minMCPTaskTimeoutMS,
			"maximum":     maxMCPPythonExecTimeoutMS,
			"default":     defaultMCPTaskTimeoutMS,
		},
	},
}

var mcpPythonExecOutputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"output", "stderr", "exit_code"},
	"properties": map[string]any{
		"output": map[string]any{
			"type":        "string",
			"description": "Captured stdout from Python execution.",
		},
		"stderr": map[string]any{
			"type":        "string",
			"description": "Captured stderr from Python execution.",
		},
		"exit_code": map[string]any{
			"type":        "integer",
			"description": "Process exit code from Python execution. Non-zero is reported as normal tool output.",
		},
	},
}

var mcpTerminalExecInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"command"},
	"properties": map[string]any{
		"command": map[string]any{
			"type":        "string",
			"description": "Shell command to run in the session container. Empty or whitespace-only values are rejected.",
		},
		"session_id": map[string]any{
			"type":        "string",
			"description": "Optional session identifier. Reuse it to keep filesystem state.",
		},
		"create_if_missing": map[string]any{
			"type":        "boolean",
			"description": "When true and session_id is missing on worker, create the session instead of returning session_not_found.",
			"default":     false,
		},
		"lease_ttl_sec": map[string]any{
			"type":        "integer",
			"description": "Optional lease duration in seconds for session expiry extension.",
			"minimum":     minMCPTerminalLeaseSec,
			"maximum":     maxMCPTerminalLeaseSec,
		},
		"timeout_ms": map[string]any{
			"type":        "integer",
			"description": "Optional synchronous execution timeout in milliseconds for this tool call.",
			"minimum":     minMCPTaskTimeoutMS,
			"maximum":     maxMCPPythonExecTimeoutMS,
			"default":     defaultMCPTaskTimeoutMS,
		},
	},
}

var mcpTerminalExecOutputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required": []string{
		"session_id",
		"created",
		"stdout",
		"stderr",
		"exit_code",
		"stdout_truncated",
		"stderr_truncated",
		"lease_expires_unix_ms",
	},
	"properties": map[string]any{
		"session_id": map[string]any{"type": "string"},
		"created":    map[string]any{"type": "boolean"},
		"stdout":     map[string]any{"type": "string"},
		"stderr":     map[string]any{"type": "string"},
		"exit_code":  map[string]any{"type": "integer"},
		"stdout_truncated": map[string]any{
			"type": "boolean",
		},
		"stderr_truncated": map[string]any{
			"type": "boolean",
		},
		"lease_expires_unix_ms": map[string]any{
			"type": "integer",
		},
	},
}

func NewMCPHandler(dispatcher CommandDispatcher) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    mcpServerName,
		Version: mcpServerVersion,
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Title:       mcpEchoToolTitle,
		Name:        "echo",
		Description: mcpEchoToolDescription,
		Annotations: &mcp.ToolAnnotations{
			Title:           mcpEchoToolTitle,
			ReadOnlyHint:    true,
			IdempotentHint:  true,
			DestructiveHint: boolPtr(false),
			OpenWorldHint:   boolPtr(false),
		},
		InputSchema:  mcpEchoInputSchema,
		OutputSchema: mcpEchoOutputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpEchoToolInput) (*mcp.CallToolResult, mcpEchoToolOutput, error) {
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
	})

	mcp.AddTool(server, &mcp.Tool{
		Title:       mcpPythonExecToolTitle,
		Name:        "pythonExec",
		Description: mcpPythonExecToolDescription,
		Annotations: &mcp.ToolAnnotations{
			Title:           mcpPythonExecToolTitle,
			DestructiveHint: boolPtr(true),
			OpenWorldHint:   boolPtr(true),
		},
		InputSchema:  mcpPythonExecInputSchema,
		OutputSchema: mcpPythonExecOutputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpPythonExecToolInput) (*mcp.CallToolResult, mcpPythonExecToolOutput, error) {
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

		payloadJSON, err := json.Marshal(pythonExecPayload{Code: input.Code})
		if err != nil {
			return nil, mcpPythonExecToolOutput{}, errors.New("failed to encode pythonExec payload")
		}

		result, err := dispatcher.SubmitTask(ctx, grpcserver.SubmitTaskRequest{
			Capability: pythonExecCapabilityName,
			InputJSON:  payloadJSON,
			Mode:       grpcserver.TaskModeSync,
			Timeout:    time.Duration(timeoutMS) * time.Millisecond,
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
	})

	mcp.AddTool(server, &mcp.Tool{
		Title:       mcpTerminalExecToolTitle,
		Name:        "terminalExec",
		Description: mcpTerminalExecToolDescription,
		Annotations: &mcp.ToolAnnotations{
			Title:           mcpTerminalExecToolTitle,
			DestructiveHint: boolPtr(true),
			OpenWorldHint:   boolPtr(true),
		},
		InputSchema:  mcpTerminalExecInputSchema,
		OutputSchema: mcpTerminalExecOutputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpTerminalExecToolInput) (*mcp.CallToolResult, mcpTerminalExecToolOutput, error) {
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
	})

	return mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
}

func invalidParamsError(message string) error {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		trimmed = "invalid params"
	}
	return &jsonrpc.Error{
		Code:    jsonrpc.CodeInvalidParams,
		Message: trimmed,
	}
}

func mapMCPToolEchoError(err error) error {
	var commandErr *grpcserver.CommandExecutionError
	switch {
	case errors.Is(err, grpcserver.ErrNoWorkerCapacity):
		return errors.New("no online worker capacity for requested capability")
	case errors.Is(err, grpcserver.ErrNoEchoWorker):
		return errors.New("no online worker supports echo")
	case errors.Is(err, grpcserver.ErrEchoTimeout):
		return errors.New("echo command timed out")
	case errors.As(err, &commandErr):
		return errors.New(commandErr.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("echo command timed out")
	default:
		return errors.New("failed to execute echo command")
	}
}

func mapMCPToolTaskSubmitError(err error) error {
	var commandErr *grpcserver.CommandExecutionError
	switch {
	case errors.Is(err, grpcserver.ErrTaskRequestInProgress):
		return errors.New("task request already in progress")
	case errors.Is(err, grpcserver.ErrNoCapabilityWorker):
		return errors.New("no online worker supports requested capability")
	case errors.Is(err, grpcserver.ErrNoWorkerCapacity):
		return errors.New("no online worker capacity for requested capability")
	case errors.As(err, &commandErr):
		return errors.New(commandErr.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("task timed out")
	case status.Code(err) == codes.InvalidArgument:
		return errors.New(status.Convert(err).Message())
	default:
		return errors.New("failed to submit task")
	}
}

func formatTaskFailureError(task grpcserver.TaskSnapshot) error {
	errorCode := strings.TrimSpace(task.ErrorCode)
	errorMessage := strings.TrimSpace(task.ErrorMessage)

	switch {
	case errorCode != "" && errorMessage != "":
		return errors.New(errorCode + ": " + errorMessage)
	case errorMessage != "":
		return errors.New(errorMessage)
	case errorCode != "":
		return errors.New(errorCode)
	default:
		return errors.New("task failed")
	}
}

func boolPtr(value bool) *bool {
	return &value
}
