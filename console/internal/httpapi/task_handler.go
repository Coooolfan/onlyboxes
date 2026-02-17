package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultTaskWaitMS    = 1500
	defaultTaskTimeoutMS = 60000
	maxTaskWaitMS        = 60000
	maxTaskTimeoutMS     = 600000
)

type submitTaskRequest struct {
	Capability string          `json:"capability"`
	Input      json.RawMessage `json:"input"`
	Mode       string          `json:"mode,omitempty"`
	WaitMS     *int            `json:"wait_ms,omitempty"`
	TimeoutMS  *int            `json:"timeout_ms,omitempty"`
	RequestID  string          `json:"request_id,omitempty"`
}

type taskErrorBody struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

type taskResponse struct {
	TaskID      string          `json:"task_id"`
	RequestID   string          `json:"request_id,omitempty"`
	CommandID   string          `json:"command_id,omitempty"`
	Capability  string          `json:"capability"`
	Status      string          `json:"status"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       *taskErrorBody  `json:"error,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	DeadlineAt  time.Time       `json:"deadline_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	StatusURL   string          `json:"status_url,omitempty"`
}

func (h *WorkerHandler) SubmitTask(c *gin.Context) {
	if h.dispatcher == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task dispatcher is unavailable"})
		return
	}

	var req submitTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	waitMS := defaultTaskWaitMS
	if req.WaitMS != nil {
		waitMS = *req.WaitMS
	}
	timeoutMS := defaultTaskTimeoutMS
	if req.TimeoutMS != nil {
		timeoutMS = *req.TimeoutMS
	}
	if waitMS <= 0 || waitMS > maxTaskWaitMS {
		c.JSON(http.StatusBadRequest, gin.H{"error": "wait_ms must be between 1 and 60000"})
		return
	}
	if timeoutMS <= 0 || timeoutMS > maxTaskTimeoutMS {
		c.JSON(http.StatusBadRequest, gin.H{"error": "timeout_ms must be between 1 and 600000"})
		return
	}

	mode, err := grpcserver.ParseTaskMode(req.Mode)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	result, err := h.dispatcher.SubmitTask(c.Request.Context(), grpcserver.SubmitTaskRequest{
		Capability: req.Capability,
		InputJSON:  append([]byte(nil), req.Input...),
		Mode:       mode,
		Wait:       time.Duration(waitMS) * time.Millisecond,
		Timeout:    time.Duration(timeoutMS) * time.Millisecond,
		RequestID:  strings.TrimSpace(req.RequestID),
	})
	if err != nil {
		h.writeTaskSubmitError(c, err)
		return
	}

	response := buildTaskResponse(result.Task)
	if result.Completed {
		statusCode := mapTaskTerminalStatusToHTTP(result.Task)
		c.JSON(statusCode, response)
		return
	}

	response.StatusURL = taskStatusURL(result.Task.TaskID)
	c.JSON(http.StatusAccepted, response)
}

func (h *WorkerHandler) GetTask(c *gin.Context) {
	if h.dispatcher == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task dispatcher is unavailable"})
		return
	}

	taskID := strings.TrimSpace(c.Param("task_id"))
	task, ok := h.dispatcher.GetTask(taskID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	c.JSON(http.StatusOK, buildTaskResponse(task))
}

func (h *WorkerHandler) CancelTask(c *gin.Context) {
	if h.dispatcher == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task dispatcher is unavailable"})
		return
	}

	taskID := strings.TrimSpace(c.Param("task_id"))
	task, err := h.dispatcher.CancelTask(taskID)
	if err != nil {
		switch {
		case errors.Is(err, grpcserver.ErrTaskNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		case errors.Is(err, grpcserver.ErrTaskTerminal):
			c.JSON(http.StatusConflict, buildTaskResponse(task))
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to cancel task"})
		}
		return
	}
	c.JSON(http.StatusOK, buildTaskResponse(task))
}

func (h *WorkerHandler) writeTaskSubmitError(c *gin.Context, err error) {
	var commandErr *grpcserver.CommandExecutionError
	switch {
	case errors.Is(err, grpcserver.ErrTaskRequestInProgress):
		c.JSON(http.StatusConflict, gin.H{"error": "task request already in progress"})
	case errors.Is(err, grpcserver.ErrNoCapabilityWorker):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no online worker supports requested capability"})
	case errors.Is(err, grpcserver.ErrNoWorkerCapacity):
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "no online worker capacity for requested capability"})
	case errors.As(err, &commandErr):
		c.JSON(http.StatusBadGateway, gin.H{"error": commandErr.Error()})
	case errors.Is(err, context.DeadlineExceeded):
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "task timed out"})
	case status.Code(err) == codes.InvalidArgument:
		c.JSON(http.StatusBadRequest, gin.H{"error": status.Convert(err).Message()})
	default:
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to submit task"})
	}
}

func mapTaskTerminalStatusToHTTP(task grpcserver.TaskSnapshot) int {
	switch task.Status {
	case grpcserver.TaskStatusSucceeded:
		return http.StatusOK
	case grpcserver.TaskStatusTimeout:
		return http.StatusGatewayTimeout
	case grpcserver.TaskStatusCanceled:
		return http.StatusConflict
	case grpcserver.TaskStatusFailed:
		switch task.ErrorCode {
		case "no_capacity":
			return http.StatusTooManyRequests
		case "no_worker":
			return http.StatusServiceUnavailable
		default:
			return http.StatusBadGateway
		}
	default:
		return http.StatusOK
	}
}

func taskStatusURL(taskID string) string {
	return "/api/v1/tasks/" + url.PathEscape(taskID)
}

func buildTaskResponse(task grpcserver.TaskSnapshot) taskResponse {
	response := taskResponse{
		TaskID:      task.TaskID,
		RequestID:   task.RequestID,
		CommandID:   task.CommandID,
		Capability:  task.Capability,
		Status:      string(task.Status),
		Result:      append(json.RawMessage(nil), task.ResultJSON...),
		CreatedAt:   task.CreatedAt,
		UpdatedAt:   task.UpdatedAt,
		DeadlineAt:  task.DeadlineAt,
		CompletedAt: task.CompletedAt,
	}
	if strings.TrimSpace(task.ErrorCode) != "" || strings.TrimSpace(task.ErrorMessage) != "" {
		response.Error = &taskErrorBody{
			Code:    task.ErrorCode,
			Message: task.ErrorMessage,
		}
	}
	return response
}
