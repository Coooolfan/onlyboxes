package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultTaskWait            = 1500 * time.Millisecond
	defaultTaskTimeout         = 60 * time.Second
	maxTaskWait                = 60 * time.Second
	maxTaskTimeout             = 10 * time.Minute
	defaultTaskNoWorkerCode    = "no_worker"
	defaultTaskNoCapacityCode  = "no_capacity"
	defaultTaskCanceledCode    = "canceled"
	defaultTaskTimeoutCode     = "timeout"
	defaultTaskDispatchErrCode = "dispatch_failed"
)

var ErrTaskNotFound = errors.New("task not found")
var ErrTaskTerminal = errors.New("task already completed")

type TaskMode string

const (
	TaskModeSync  TaskMode = "sync"
	TaskModeAsync TaskMode = "async"
	TaskModeAuto  TaskMode = "auto"
)

type TaskStatus string

const (
	TaskStatusQueued     TaskStatus = "queued"
	TaskStatusDispatched TaskStatus = "dispatched"
	TaskStatusRunning    TaskStatus = "running"
	TaskStatusSucceeded  TaskStatus = "succeeded"
	TaskStatusFailed     TaskStatus = "failed"
	TaskStatusTimeout    TaskStatus = "timeout"
	TaskStatusCanceled   TaskStatus = "canceled"
)

type SubmitTaskRequest struct {
	Capability string
	InputJSON  []byte
	Mode       TaskMode
	Wait       time.Duration
	Timeout    time.Duration
	RequestID  string
}

type SubmitTaskResult struct {
	Task      TaskSnapshot
	Completed bool
}

type TaskSnapshot struct {
	TaskID       string
	RequestID    string
	CommandID    string
	Capability   string
	Status       TaskStatus
	ResultJSON   []byte
	ErrorCode    string
	ErrorMessage string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeadlineAt   time.Time
	CompletedAt  *time.Time
}

type taskRecord struct {
	id         string
	requestID  string
	commandID  string
	capability string
	inputJSON  []byte

	status       TaskStatus
	resultJSON   []byte
	errorCode    string
	errorMessage string

	createdAt   time.Time
	updatedAt   time.Time
	deadlineAt  time.Time
	completedAt time.Time
	expiresAt   time.Time

	cancel     context.CancelFunc
	cancelOnce sync.Once
	done       chan struct{}
	doneOnce   sync.Once
}

func ParseTaskMode(raw string) (TaskMode, error) {
	trimmed := strings.TrimSpace(strings.ToLower(raw))
	if trimmed == "" {
		return TaskModeAuto, nil
	}
	switch TaskMode(trimmed) {
	case TaskModeSync, TaskModeAsync, TaskModeAuto:
		return TaskMode(trimmed), nil
	default:
		return "", fmt.Errorf("mode must be one of sync|async|auto")
	}
}

func (s *RegistryService) SubmitTask(ctx context.Context, req SubmitTaskRequest) (SubmitTaskResult, error) {
	capability := normalizeCapability(req.Capability)
	if capability == "" {
		return SubmitTaskResult{}, status.Error(codes.InvalidArgument, "capability is required")
	}

	mode, err := ParseTaskMode(string(req.Mode))
	if err != nil {
		return SubmitTaskResult{}, status.Error(codes.InvalidArgument, err.Error())
	}

	inputJSON := append([]byte(nil), req.InputJSON...)
	if len(inputJSON) == 0 {
		inputJSON = []byte("{}")
	}
	if !json.Valid(inputJSON) {
		return SubmitTaskResult{}, status.Error(codes.InvalidArgument, "input must be valid JSON")
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTaskTimeout
	}
	if timeout > maxTaskTimeout {
		return SubmitTaskResult{}, status.Error(codes.InvalidArgument, "timeout exceeds maximum allowed value")
	}

	wait := req.Wait
	if wait <= 0 {
		wait = defaultTaskWait
	}
	if wait > maxTaskWait {
		return SubmitTaskResult{}, status.Error(codes.InvalidArgument, "wait exceeds maximum allowed value")
	}
	if mode == TaskModeAuto && wait > timeout {
		wait = timeout
	}

	requestID := strings.TrimSpace(req.RequestID)
	requestReserved := false

	if requestID != "" {
		s.tasksMu.Lock()
		s.pruneTasksLocked(s.nowFn())
		if existingID, ok := s.taskByRequestKey[requestID]; ok {
			existing, exists := s.tasks[existingID]
			if exists {
				s.tasksMu.Unlock()
				return s.resolveSubmitTaskResult(ctx, existing, mode, wait)
			}
			delete(s.taskByRequestKey, requestID)
		}
		if _, reserved := s.taskRequestReservations[requestID]; reserved {
			s.tasksMu.Unlock()
			return SubmitTaskResult{}, ErrTaskRequestInProgress
		}
		// Reserve request_id inside the same critical section as the dedup check,
		// so concurrent submissions cannot pass validation and create duplicates.
		s.taskRequestReservations[requestID] = struct{}{}
		requestReserved = true
		s.tasksMu.Unlock()
		defer func() {
			if !requestReserved {
				return
			}
			s.tasksMu.Lock()
			delete(s.taskRequestReservations, requestID)
			s.tasksMu.Unlock()
		}()
	}

	if availabilityErr := s.checkCapabilityAvailability(capability); availabilityErr != nil {
		return SubmitTaskResult{}, availabilityErr
	}

	taskID, err := s.newTaskIDFn()
	if err != nil {
		return SubmitTaskResult{}, status.Error(codes.Internal, "failed to create task_id")
	}
	now := s.nowFn()
	taskCtx, taskCancel := context.WithTimeout(context.Background(), timeout)
	record := &taskRecord{
		id:         taskID,
		requestID:  requestID,
		capability: capability,
		inputJSON:  inputJSON,
		status:     TaskStatusQueued,
		createdAt:  now,
		updatedAt:  now,
		deadlineAt: now.Add(timeout),
		cancel:     taskCancel,
		done:       make(chan struct{}),
	}

	s.tasksMu.Lock()
	s.pruneTasksLocked(now)
	s.tasks[taskID] = record
	if requestID != "" {
		delete(s.taskRequestReservations, requestID)
		requestReserved = false
		s.taskByRequestKey[requestID] = taskID
	}
	s.tasksMu.Unlock()

	go s.executeTask(taskCtx, taskID, capability, inputJSON)

	return s.resolveSubmitTaskResult(ctx, record, mode, wait)
}

func (s *RegistryService) GetTask(taskID string) (TaskSnapshot, bool) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return TaskSnapshot{}, false
	}

	s.tasksMu.RLock()
	record, ok := s.tasks[taskID]
	s.tasksMu.RUnlock()
	if !ok || record == nil {
		return TaskSnapshot{}, false
	}

	s.tasksMu.Lock()
	s.pruneTasksLocked(s.nowFn())
	record, ok = s.tasks[taskID]
	if !ok || record == nil {
		s.tasksMu.Unlock()
		return TaskSnapshot{}, false
	}
	snapshot := snapshotTaskLocked(record)
	s.tasksMu.Unlock()
	return snapshot, true
}

func (s *RegistryService) CancelTask(taskID string) (TaskSnapshot, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return TaskSnapshot{}, ErrTaskNotFound
	}

	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()

	s.pruneTasksLocked(s.nowFn())
	record, ok := s.tasks[taskID]
	if !ok || record == nil {
		return TaskSnapshot{}, ErrTaskNotFound
	}
	if isTaskTerminal(record.status) {
		return snapshotTaskLocked(record), ErrTaskTerminal
	}

	now := s.nowFn()
	s.finishTaskLocked(record, TaskStatusCanceled, nil, defaultTaskCanceledCode, "task canceled", now)
	return snapshotTaskLocked(record), nil
}

func (s *RegistryService) resolveSubmitTaskResult(
	ctx context.Context,
	record *taskRecord,
	mode TaskMode,
	wait time.Duration,
) (SubmitTaskResult, error) {
	if record == nil {
		return SubmitTaskResult{}, ErrTaskNotFound
	}

	snapshotNow := func() SubmitTaskResult {
		s.tasksMu.RLock()
		defer s.tasksMu.RUnlock()
		return SubmitTaskResult{
			Task:      snapshotTaskLocked(record),
			Completed: isTaskTerminal(record.status),
		}
	}

	switch mode {
	case TaskModeAsync:
		return snapshotNow(), nil
	case TaskModeSync:
		select {
		case <-ctx.Done():
			return SubmitTaskResult{}, ctx.Err()
		case <-record.done:
			return snapshotNow(), nil
		}
	case TaskModeAuto:
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return SubmitTaskResult{}, ctx.Err()
		case <-record.done:
			return snapshotNow(), nil
		case <-timer.C:
			result := snapshotNow()
			result.Completed = isTaskTerminal(result.Task.Status)
			return result, nil
		}
	default:
		return SubmitTaskResult{}, status.Error(codes.InvalidArgument, "unsupported mode")
	}
}

func (s *RegistryService) executeTask(ctx context.Context, taskID string, capability string, inputJSON []byte) {
	s.markTaskDispatched(taskID)
	outcome, err := s.dispatchCommand(ctx, capability, inputJSON, 0, func(commandID string) {
		s.markTaskRunning(taskID, commandID)
	})
	if err != nil {
		s.finishTaskWithError(taskID, err)
		return
	}
	if outcome.err != nil {
		s.finishTaskWithError(taskID, outcome.err)
		return
	}

	resultPayload := append([]byte(nil), outcome.payloadJSON...)
	if len(resultPayload) == 0 && strings.TrimSpace(outcome.message) != "" {
		resultPayload = buildEchoPayload(outcome.message)
	}
	completedAt := outcome.completedAt
	if completedAt.IsZero() {
		completedAt = s.nowFn()
	}
	if !json.Valid(resultPayload) {
		resultPayload = buildEchoPayload(string(resultPayload))
	}

	s.tasksMu.Lock()
	record, ok := s.tasks[taskID]
	if ok && record != nil {
		s.finishTaskLocked(record, TaskStatusSucceeded, resultPayload, "", "", completedAt)
	}
	s.tasksMu.Unlock()
}

func (s *RegistryService) markTaskDispatched(taskID string) {
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	record, ok := s.tasks[taskID]
	if !ok || record == nil || isTaskTerminal(record.status) {
		return
	}
	record.status = TaskStatusDispatched
	record.updatedAt = s.nowFn()
}

func (s *RegistryService) markTaskRunning(taskID string, commandID string) {
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	record, ok := s.tasks[taskID]
	if !ok || record == nil || isTaskTerminal(record.status) {
		return
	}
	record.status = TaskStatusRunning
	record.commandID = strings.TrimSpace(commandID)
	record.updatedAt = s.nowFn()
}

func (s *RegistryService) finishTaskWithError(taskID string, err error) {
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()

	record, ok := s.tasks[taskID]
	if !ok || record == nil || isTaskTerminal(record.status) {
		return
	}

	now := s.nowFn()
	var commandErr *CommandExecutionError
	switch {
	case errors.Is(err, ErrNoCapabilityWorker):
		s.finishTaskLocked(record, TaskStatusFailed, nil, defaultTaskNoWorkerCode, "no online worker supports capability", now)
	case errors.Is(err, ErrNoWorkerCapacity):
		s.finishTaskLocked(record, TaskStatusFailed, nil, defaultTaskNoCapacityCode, "no online worker capacity for capability", now)
	case errors.Is(err, context.DeadlineExceeded):
		s.finishTaskLocked(record, TaskStatusTimeout, nil, defaultTaskTimeoutCode, "task timed out", now)
	case errors.Is(err, context.Canceled):
		s.finishTaskLocked(record, TaskStatusCanceled, nil, defaultTaskCanceledCode, "task canceled", now)
	case errors.As(err, &commandErr):
		code := strings.TrimSpace(commandErr.Code)
		if code == "" {
			code = defaultTaskDispatchErrCode
		}
		s.finishTaskLocked(record, TaskStatusFailed, nil, code, commandErr.Message, now)
	case status.Code(err) == codes.DeadlineExceeded:
		s.finishTaskLocked(record, TaskStatusTimeout, nil, defaultTaskTimeoutCode, "task timed out", now)
	default:
		s.finishTaskLocked(record, TaskStatusFailed, nil, defaultTaskDispatchErrCode, err.Error(), now)
	}
}

func (s *RegistryService) finishTaskLocked(
	record *taskRecord,
	statusValue TaskStatus,
	resultJSON []byte,
	errorCode string,
	errorMessage string,
	completedAt time.Time,
) {
	if record == nil || isTaskTerminal(record.status) {
		return
	}
	if completedAt.IsZero() {
		completedAt = s.nowFn()
	}

	record.status = statusValue
	record.resultJSON = append([]byte(nil), resultJSON...)
	record.errorCode = strings.TrimSpace(errorCode)
	record.errorMessage = strings.TrimSpace(errorMessage)
	record.updatedAt = completedAt
	record.completedAt = completedAt
	record.expiresAt = completedAt.Add(s.taskRetention)
	if record.cancel != nil {
		cancel := record.cancel
		record.cancel = nil
		record.cancelOnce.Do(cancel)
	}
	record.doneOnce.Do(func() {
		close(record.done)
	})
}

func (s *RegistryService) checkCapabilityAvailability(capability string) error {
	now := s.nowFn()
	workers := s.store.ListOnlineByCapability(capability, now, time.Duration(s.offlineTTLSec)*time.Second)
	if len(workers) == 0 {
		return ErrNoCapabilityWorker
	}

	for _, worker := range workers {
		session := s.getSession(worker.NodeID)
		if session == nil || !session.hasCapability(capability) {
			continue
		}
		inflight, maxInflight, ok := session.inflightSnapshot(capability)
		if !ok {
			continue
		}
		if inflight < maxInflight {
			return nil
		}
	}
	return ErrNoWorkerCapacity
}

func (s *RegistryService) pruneTasksLocked(now time.Time) {
	for taskID, record := range s.tasks {
		if record == nil || !isTaskTerminal(record.status) {
			continue
		}
		if now.Before(record.expiresAt) {
			continue
		}
		delete(s.tasks, taskID)
		if record.requestID != "" {
			if mapped, ok := s.taskByRequestKey[record.requestID]; ok && mapped == taskID {
				delete(s.taskByRequestKey, record.requestID)
			}
		}
	}
}

func snapshotTaskLocked(record *taskRecord) TaskSnapshot {
	if record == nil {
		return TaskSnapshot{}
	}
	snapshot := TaskSnapshot{
		TaskID:       record.id,
		RequestID:    record.requestID,
		CommandID:    record.commandID,
		Capability:   record.capability,
		Status:       record.status,
		ResultJSON:   append([]byte(nil), record.resultJSON...),
		ErrorCode:    record.errorCode,
		ErrorMessage: record.errorMessage,
		CreatedAt:    record.createdAt,
		UpdatedAt:    record.updatedAt,
		DeadlineAt:   record.deadlineAt,
	}
	if !record.completedAt.IsZero() {
		completed := record.completedAt
		snapshot.CompletedAt = &completed
	}
	return snapshot
}

func isTaskTerminal(statusValue TaskStatus) bool {
	switch statusValue {
	case TaskStatusSucceeded, TaskStatusFailed, TaskStatusTimeout, TaskStatusCanceled:
		return true
	default:
		return false
	}
}
