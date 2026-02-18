package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
)

const (
	defaultEchoTimeoutMS            = 5000
	minEchoTimeoutMS                = 1
	maxEchoTimeoutMS                = 60000
	defaultTerminalTimeoutMS        = defaultTaskTimeoutMS
	minTerminalTimeoutMS            = 1
	maxTerminalTimeoutMS            = maxTaskTimeoutMS
	terminalExecCapability          = "terminalExec"
	terminalExecSessionNotFoundCode = "session_not_found"
	terminalExecSessionBusyCode     = "session_busy"
	terminalExecInvalidPayloadCode  = "invalid_payload"
	terminalTaskNoWorkerCode        = "no_worker"
	terminalTaskNoCapacityCode      = "no_capacity"
	terminalTaskTimeoutCode         = "timeout"
)

type EchoDispatcher interface {
	DispatchEcho(ctx context.Context, message string, timeout time.Duration) (string, error)
}

type TaskDispatcher interface {
	SubmitTask(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error)
	GetTask(taskID string) (grpcserver.TaskSnapshot, bool)
	CancelTask(taskID string) (grpcserver.TaskSnapshot, error)
}

type CommandDispatcher interface {
	EchoDispatcher
	TaskDispatcher
}

type echoCommandRequest struct {
	Message   string `json:"message"`
	TimeoutMS *int   `json:"timeout_ms,omitempty"`
}

type echoCommandResponse struct {
	Message string `json:"message"`
}

type terminalCommandRequest struct {
	Command         string `json:"command"`
	SessionID       string `json:"session_id,omitempty"`
	CreateIfMissing bool   `json:"create_if_missing,omitempty"`
	LeaseTTLSec     *int   `json:"lease_ttl_sec,omitempty"`
	TimeoutMS       *int   `json:"timeout_ms,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
}

type terminalExecPayload struct {
	Command         string `json:"command"`
	SessionID       string `json:"session_id,omitempty"`
	CreateIfMissing bool   `json:"create_if_missing,omitempty"`
	LeaseTTLSec     *int   `json:"lease_ttl_sec,omitempty"`
}

type terminalCommandResponse struct {
	SessionID          string `json:"session_id"`
	Created            bool   `json:"created"`
	Stdout             string `json:"stdout"`
	Stderr             string `json:"stderr"`
	ExitCode           int    `json:"exit_code"`
	StdoutTruncated    bool   `json:"stdout_truncated"`
	StderrTruncated    bool   `json:"stderr_truncated"`
	LeaseExpiresUnixMS int64  `json:"lease_expires_unix_ms"`
}

func (h *WorkerHandler) EchoCommand(c *gin.Context) {
	if h.dispatcher == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "echo command dispatcher is unavailable"})
		return
	}

	var req echoCommandRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	message := req.Message
	if strings.TrimSpace(message) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message is required"})
		return
	}

	timeoutMS := defaultEchoTimeoutMS
	if req.TimeoutMS != nil {
		timeoutMS = *req.TimeoutMS
	}
	if timeoutMS < minEchoTimeoutMS || timeoutMS > maxEchoTimeoutMS {
		c.JSON(http.StatusBadRequest, gin.H{"error": "timeout_ms must be between 1 and 60000"})
		return
	}

	timeout := time.Duration(timeoutMS) * time.Millisecond
	result, err := h.dispatcher.DispatchEcho(c.Request.Context(), message, timeout)
	if err != nil {
		var commandErr *grpcserver.CommandExecutionError
		switch {
		case errors.Is(err, grpcserver.ErrNoWorkerCapacity):
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "no online worker capacity for requested capability"})
		case errors.Is(err, grpcserver.ErrNoEchoWorker):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no online worker supports echo"})
		case errors.Is(err, grpcserver.ErrEchoTimeout):
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "echo command timed out"})
		case errors.As(err, &commandErr):
			c.JSON(http.StatusBadGateway, gin.H{"error": commandErr.Error()})
		case errors.Is(err, context.DeadlineExceeded):
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "echo command timed out"})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to execute echo command"})
		}
		return
	}

	c.JSON(http.StatusOK, echoCommandResponse{
		Message: result,
	})
}

func (h *WorkerHandler) TerminalCommand(c *gin.Context) {
	if h.dispatcher == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task dispatcher is unavailable"})
		return
	}

	var req terminalCommandRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "command is required"})
		return
	}

	timeoutMS := defaultTerminalTimeoutMS
	if req.TimeoutMS != nil {
		timeoutMS = *req.TimeoutMS
	}
	if timeoutMS < minTerminalTimeoutMS || timeoutMS > maxTerminalTimeoutMS {
		c.JSON(http.StatusBadRequest, gin.H{"error": "timeout_ms must be between 1 and 600000"})
		return
	}

	payloadJSON, err := json.Marshal(terminalExecPayload{
		Command:         req.Command,
		SessionID:       strings.TrimSpace(req.SessionID),
		CreateIfMissing: req.CreateIfMissing,
		LeaseTTLSec:     req.LeaseTTLSec,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode terminal payload"})
		return
	}

	taskResult, err := h.dispatcher.SubmitTask(c.Request.Context(), grpcserver.SubmitTaskRequest{
		Capability: terminalExecCapability,
		InputJSON:  payloadJSON,
		Mode:       grpcserver.TaskModeSync,
		Timeout:    time.Duration(timeoutMS) * time.Millisecond,
		RequestID:  strings.TrimSpace(req.RequestID),
	})
	if err != nil {
		h.writeTaskSubmitError(c, err)
		return
	}
	if !taskResult.Completed {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "task timed out"})
		return
	}

	task := taskResult.Task
	switch task.Status {
	case grpcserver.TaskStatusSucceeded:
		response := terminalCommandResponse{}
		if err := json.Unmarshal(task.ResultJSON, &response); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "invalid terminalExec result payload"})
			return
		}
		c.JSON(http.StatusOK, response)
	case grpcserver.TaskStatusTimeout:
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "task timed out"})
	case grpcserver.TaskStatusCanceled:
		c.JSON(http.StatusConflict, gin.H{"error": "task canceled"})
	case grpcserver.TaskStatusFailed:
		statusCode, message := mapTerminalTaskFailure(task)
		c.JSON(statusCode, gin.H{"error": message})
	default:
		c.JSON(http.StatusBadGateway, gin.H{"error": "unexpected task status"})
	}
}

func mapTerminalTaskFailure(task grpcserver.TaskSnapshot) (int, string) {
	code := strings.TrimSpace(task.ErrorCode)
	message := strings.TrimSpace(task.ErrorMessage)
	if message == "" {
		message = "terminal command failed"
	}

	switch code {
	case terminalExecSessionNotFoundCode:
		return http.StatusNotFound, message
	case terminalExecSessionBusyCode:
		return http.StatusConflict, message
	case terminalTaskNoWorkerCode:
		return http.StatusServiceUnavailable, "no online worker supports requested capability"
	case terminalTaskNoCapacityCode:
		return http.StatusTooManyRequests, "no online worker capacity for requested capability"
	case terminalExecInvalidPayloadCode:
		return http.StatusBadRequest, message
	case terminalTaskTimeoutCode, "deadline_exceeded":
		return http.StatusGatewayTimeout, message
	default:
		return http.StatusBadGateway, message
	}
}
