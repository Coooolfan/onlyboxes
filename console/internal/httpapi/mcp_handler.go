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
	mcpServerName             = "onlyboxes-console"
	mcpServerVersion          = "v0.1.0"
	pythonExecCapabilityName  = "pythonExec"
	defaultMCPEchoTimeoutMS   = defaultEchoTimeoutMS
	minMCPTaskTimeoutMS       = 1
	defaultMCPTaskTimeoutMS   = defaultTaskTimeoutMS
	maxMCPPythonExecTimeoutMS = maxTaskTimeoutMS
	mcpEchoToolTitle          = "Echo Message"
	mcpPythonExecToolTitle    = "Python Execute"
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

type pythonExecPayload struct {
	Code string `json:"code"`
}

var mcpEchoToolDescription = "Echoes the input message exactly as returned by an online worker supporting the echo capability. Use this tool for connectivity checks, request tracing, and latency baselines. Do not use it for code execution, file operations, or long-running work. timeout_ms is an end-to-end dispatch timeout in milliseconds (1-60000, default 5000)."

var mcpPythonExecToolDescription = "Executes Python code in the worker sandbox via the pythonExec capability and returns stdout, stderr, and exit_code. Use this for short, self-contained snippets. Do not use it for long-running jobs or persistent state. timeout_ms is a synchronous execution timeout in milliseconds (1-600000, default 60000). A non-zero exit_code is returned as normal tool output, not as a protocol error."

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
			return nil, mcpPythonExecToolOutput{}, formatTaskTerminalError(task)
		default:
			return nil, mcpPythonExecToolOutput{}, fmt.Errorf("unexpected task status: %s", task.Status)
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

func formatTaskTerminalError(task grpcserver.TaskSnapshot) error {
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
