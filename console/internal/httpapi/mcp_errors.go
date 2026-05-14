package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// computerUse worker-sys readiness errors are JSON-RPC application-range codes
	// (-32099..-32000 per JSON-RPC 2.0) so they never collide with the spec's
	// reserved codes.
	mcpErrorCodeWorkerSysRequired = -32010
	mcpErrorCodeWorkerSysOffline  = -32011
	// Stable machine-readable identifiers embedded in the JSON-RPC error's Data
	// field so integrators can route on them without pattern-matching message
	// strings. REQUIRED means the caller has never created a worker-sys; OFFLINE
	// means one exists but no instance is currently connected.
	errorCodeWorkerSysRequired = "WORKER_SYS_REQUIRED"
	errorCodeWorkerSysOffline  = "WORKER_SYS_OFFLINE"
)

// WorkerSysCounter is the minimal capability handleMCPComputerUseTool needs to
// distinguish "never provisioned" from "registered but offline". *registry.Store
// satisfies it via CountWorkersByOwnerAndType.
type WorkerSysCounter interface {
	CountWorkersByOwnerAndType(ownerID string, workerType string) int
}

func workerSysRequiredError() error {
	data, _ := json.Marshal(map[string]string{
		"error_code": errorCodeWorkerSysRequired,
	})
	return &jsonrpc.Error{
		Code:    mcpErrorCodeWorkerSysRequired,
		Message: errorCodeWorkerSysRequired + ": no worker-sys exists for the current account. Provision one in the application's worker management page, then start it locally and retry.",
		Data:    data,
	}
}

func workerSysOfflineError() error {
	data, _ := json.Marshal(map[string]string{
		"error_code": errorCodeWorkerSysOffline,
	})
	return &jsonrpc.Error{
		Code:    mcpErrorCodeWorkerSysOffline,
		Message: errorCodeWorkerSysOffline + ": your worker-sys is registered but no instance is currently online. Start the worker-sys process locally and retry.",
		Data:    data,
	}
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
