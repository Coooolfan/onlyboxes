package httpapi

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
