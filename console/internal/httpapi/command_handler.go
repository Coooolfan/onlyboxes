package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
)

const (
	defaultEchoTimeoutMS = 5000
	minEchoTimeoutMS     = 1
	maxEchoTimeoutMS     = 60000
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
