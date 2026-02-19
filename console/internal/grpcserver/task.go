package grpcserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultTaskWait            = 1500 * time.Millisecond
	defaultTaskTimeout         = 60 * time.Second
	maxTaskWait                = 60 * time.Second
	maxTaskTimeout             = 10 * time.Minute
	inlineTaskPruneMinInterval = 15 * time.Second
	defaultTaskNoWorkerCode    = "no_worker"
	defaultTaskNoCapacityCode  = "no_capacity"
	defaultTaskCanceledCode    = "canceled"
	defaultTaskTimeoutCode     = "timeout"
	defaultTaskDispatchErrCode = "dispatch_failed"
	defaultTaskPersistErrCode  = "persistence_error"
)

var ErrTaskNotFound = errors.New("task not found")
var ErrTaskTerminal = errors.New("task already completed")
var ErrTaskTransitionNotApplied = errors.New("task state transition was not applied")

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
	OwnerID    string
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
	ownerID    string
	requestID  string
	status     TaskStatus
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
	ownerID := normalizeTaskOwnerID(req.OwnerID)
	if ownerID == "" {
		return SubmitTaskResult{}, status.Error(codes.InvalidArgument, "owner_id is required")
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
	scopedInputJSON, err := s.scopeTaskInputByOwner(capability, ownerID, inputJSON)
	if err != nil {
		return SubmitTaskResult{}, err
	}
	inputJSON = scopedInputJSON

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
	s.maybePruneExpiredTasks(s.nowFn())

	requestID := strings.TrimSpace(req.RequestID)
	requestKey := taskRequestScopeKey(ownerID, requestID)
	requestReserved := false
	if requestID != "" {
		s.tasksMu.Lock()
		if _, reserved := s.taskRequestReservations[requestKey]; reserved {
			s.tasksMu.Unlock()
			return SubmitTaskResult{}, ErrTaskRequestInProgress
		}
		s.taskRequestReservations[requestKey] = struct{}{}
		requestReserved = true
		s.tasksMu.Unlock()
		defer func() {
			if !requestReserved {
				return
			}
			s.tasksMu.Lock()
			delete(s.taskRequestReservations, requestKey)
			s.tasksMu.Unlock()
		}()
		existing, found := s.getTaskByOwnerAndRequest(ownerID, requestID)
		if found {
			return s.resolveSubmitTaskResult(ctx, existing.taskID, s.getTaskRuntime(existing.taskID), mode, wait)
		}
	}

	if availabilityErr := s.checkCapabilityAvailability(capability); availabilityErr != nil {
		return SubmitTaskResult{}, availabilityErr
	}

	taskID, err := s.newTaskIDFn()
	if err != nil {
		return SubmitTaskResult{}, status.Error(codes.Internal, "failed to create task_id")
	}
	now := s.nowFn()

	insertErr := s.taskQueries().InsertTask(context.Background(), sqlc.InsertTaskParams{
		TaskID:            taskID,
		OwnerID:           ownerID,
		RequestID:         requestID,
		Capability:        capability,
		InputJson:         string(inputJSON),
		Status:            string(TaskStatusQueued),
		CommandID:         "",
		ResultJson:        "",
		ErrorCode:         "",
		ErrorMessage:      "",
		CreatedAtUnixMs:   now.UnixMilli(),
		UpdatedAtUnixMs:   now.UnixMilli(),
		DeadlineAtUnixMs:  now.Add(timeout).UnixMilli(),
		CompletedAtUnixMs: 0,
		ExpiresAtUnixMs:   0,
	})
	if insertErr != nil {
		if requestID != "" && isTaskOwnerRequestConflict(insertErr) {
			existing, found := s.getTaskByOwnerAndRequest(ownerID, requestID)
			if found {
				return s.resolveSubmitTaskResult(ctx, existing.taskID, s.getTaskRuntime(existing.taskID), mode, wait)
			}
		}
		return SubmitTaskResult{}, status.Error(codes.Internal, "failed to create task")
	}

	taskCtx, taskCancel := context.WithTimeout(context.Background(), timeout)
	runtimeRecord := &taskRecord{
		id:        taskID,
		ownerID:   ownerID,
		requestID: requestID,
		cancel:    taskCancel,
		done:      make(chan struct{}),
	}
	s.setTaskRuntime(taskID, runtimeRecord)
	if requestReserved {
		s.tasksMu.Lock()
		delete(s.taskRequestReservations, requestKey)
		s.tasksMu.Unlock()
		requestReserved = false
	}

	go s.executeTask(taskCtx, taskID, ownerID, capability, inputJSON)
	return s.resolveSubmitTaskResult(ctx, taskID, runtimeRecord, mode, wait)
}

func (s *RegistryService) GetTask(taskID string, ownerID string) (TaskSnapshot, bool) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return TaskSnapshot{}, false
	}
	normalizedOwnerID := normalizeTaskOwnerID(ownerID)
	snapshot, found := s.getTaskByID(taskID)
	if !found || snapshot.ownerID != normalizedOwnerID {
		return TaskSnapshot{}, false
	}
	return snapshotTask(snapshot), true
}

func (s *RegistryService) CancelTask(taskID string, ownerID string) (TaskSnapshot, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return TaskSnapshot{}, ErrTaskNotFound
	}
	normalizedOwnerID := normalizeTaskOwnerID(ownerID)
	current, found := s.getTaskByID(taskID)
	if !found || current.ownerID != normalizedOwnerID {
		return TaskSnapshot{}, ErrTaskNotFound
	}
	if isTaskTerminal(current.status) {
		return snapshotTask(current), ErrTaskTerminal
	}

	now := s.nowFn()
	if err := s.finishTask(taskID, TaskStatusCanceled, nil, defaultTaskCanceledCode, "task canceled", now); err != nil {
		if errors.Is(err, ErrTaskTransitionNotApplied) {
			latest, found := s.getTaskByID(taskID)
			if !found || latest.ownerID != normalizedOwnerID {
				return TaskSnapshot{}, ErrTaskNotFound
			}
			if isTaskTerminal(latest.status) {
				return snapshotTask(latest), ErrTaskTerminal
			}
		}
		return TaskSnapshot{}, err
	}
	updated, found := s.getTaskByID(taskID)
	if !found {
		return TaskSnapshot{}, ErrTaskNotFound
	}
	return snapshotTask(updated), nil
}

func (s *RegistryService) resolveSubmitTaskResult(
	ctx context.Context,
	taskID string,
	runtime *taskRecord,
	mode TaskMode,
	wait time.Duration,
) (SubmitTaskResult, error) {
	if strings.TrimSpace(taskID) == "" {
		return SubmitTaskResult{}, ErrTaskNotFound
	}

	snapshotNow := func() (SubmitTaskResult, error) {
		taskState, found := s.getTaskByID(taskID)
		if !found {
			return SubmitTaskResult{}, ErrTaskNotFound
		}
		snapshot := snapshotTask(taskState)
		return SubmitTaskResult{Task: snapshot, Completed: isTaskTerminal(snapshot.Status)}, nil
	}

	snap, err := snapshotNow()
	if err != nil {
		return SubmitTaskResult{}, err
	}
	if mode == TaskModeAsync || snap.Completed {
		return snap, nil
	}

	waitDone := func(waitDuration time.Duration) error {
		if runtime == nil {
			return nil
		}
		if waitDuration <= 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-runtime.done:
				return nil
			}
		}
		timer := time.NewTimer(waitDuration)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-runtime.done:
			return nil
		case <-timer.C:
			return nil
		}
	}

	switch mode {
	case TaskModeSync:
		if err := waitDone(0); err != nil {
			return SubmitTaskResult{}, err
		}
		return snapshotNow()
	case TaskModeAuto:
		if err := waitDone(wait); err != nil {
			return SubmitTaskResult{}, err
		}
		return snapshotNow()
	default:
		return SubmitTaskResult{}, status.Error(codes.InvalidArgument, "unsupported mode")
	}
}

func (s *RegistryService) executeTask(ctx context.Context, taskID string, ownerID string, capability string, inputJSON []byte) {
	if err := s.markTaskDispatched(taskID); err != nil {
		if errors.Is(err, ErrTaskTransitionNotApplied) {
			return
		}
		log.Printf("task %s failed to mark dispatched: %v", taskID, err)
		if failErr := s.failTaskOnPersistenceError(taskID, "mark_dispatched", err); failErr != nil {
			log.Printf("task %s failed to persist persistence_error after mark_dispatched: %v", taskID, failErr)
		}
		return
	}
	var markRunningErr error
	outcome, err := s.dispatchCommand(ctx, capability, inputJSON, 0, func(commandID string) {
		if markErr := s.markTaskRunning(taskID, commandID); markErr != nil {
			markRunningErr = markErr
			runtime := s.getTaskRuntime(taskID)
			if runtime != nil && runtime.cancel != nil {
				runtime.cancelOnce.Do(runtime.cancel)
			}
		}
	})
	if markRunningErr != nil {
		if errors.Is(markRunningErr, ErrTaskTransitionNotApplied) {
			return
		}
		log.Printf("task %s failed to mark running: %v", taskID, markRunningErr)
		if failErr := s.failTaskOnPersistenceError(taskID, "mark_running", markRunningErr); failErr != nil {
			log.Printf("task %s failed to persist persistence_error after mark_running: %v", taskID, failErr)
		}
		return
	}
	if err != nil {
		if finishErr := s.finishTaskWithError(taskID, err); finishErr != nil {
			if errors.Is(finishErr, ErrTaskTransitionNotApplied) {
				return
			}
			log.Printf("task %s failed to mark terminal with dispatch error: %v", taskID, finishErr)
			if failErr := s.failTaskOnPersistenceError(taskID, "finish_error", finishErr); failErr != nil {
				log.Printf("task %s failed to persist persistence_error after finish_error: %v", taskID, failErr)
			}
		}
		return
	}
	if outcome.err != nil {
		if finishErr := s.finishTaskWithError(taskID, outcome.err); finishErr != nil {
			if errors.Is(finishErr, ErrTaskTransitionNotApplied) {
				return
			}
			log.Printf("task %s failed to mark terminal with worker error: %v", taskID, finishErr)
			if failErr := s.failTaskOnPersistenceError(taskID, "finish_error", finishErr); failErr != nil {
				log.Printf("task %s failed to persist persistence_error after finish_error: %v", taskID, failErr)
			}
		}
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

	scopedResultPayload, scopedOK := s.restoreTaskResultOwnerScope(ownerID, capability, resultPayload)
	if !scopedOK {
		if err := s.finishTask(taskID, TaskStatusFailed, nil, taskOwnerScopeInvalidPayloadCode, taskOwnerScopeInvalidPayloadMessage, completedAt); err != nil {
			if errors.Is(err, ErrTaskTransitionNotApplied) {
				return
			}
			log.Printf("task %s failed to mark invalid scoped payload: %v", taskID, err)
			if failErr := s.failTaskOnPersistenceError(taskID, "finish_invalid_payload", err); failErr != nil {
				log.Printf("task %s failed to persist persistence_error after finish_invalid_payload: %v", taskID, failErr)
			}
		}
		return
	}
	if err := s.finishTask(taskID, TaskStatusSucceeded, scopedResultPayload, "", "", completedAt); err != nil {
		if errors.Is(err, ErrTaskTransitionNotApplied) {
			return
		}
		log.Printf("task %s failed to mark succeeded: %v", taskID, err)
		if failErr := s.failTaskOnPersistenceError(taskID, "finish_succeeded", err); failErr != nil {
			log.Printf("task %s failed to persist persistence_error after finish_succeeded: %v", taskID, failErr)
		}
	}
}

func (s *RegistryService) markTaskDispatched(taskID string) error {
	if strings.TrimSpace(taskID) == "" {
		return errors.New("task_id is required")
	}
	queries := s.taskQueries()
	if queries == nil {
		return errors.New("task store is unavailable")
	}
	rows, err := queries.MarkTaskDispatched(context.Background(), sqlc.MarkTaskDispatchedParams{
		UpdatedAtUnixMs: s.nowFn().UnixMilli(),
		TaskID:          taskID,
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("%w: task %s transition to dispatched", ErrTaskTransitionNotApplied, taskID)
	}
	return nil
}

func (s *RegistryService) markTaskRunning(taskID string, commandID string) error {
	if strings.TrimSpace(taskID) == "" {
		return errors.New("task_id is required")
	}
	queries := s.taskQueries()
	if queries == nil {
		return errors.New("task store is unavailable")
	}
	rows, err := queries.MarkTaskRunning(context.Background(), sqlc.MarkTaskRunningParams{
		CommandID:       strings.TrimSpace(commandID),
		UpdatedAtUnixMs: s.nowFn().UnixMilli(),
		TaskID:          taskID,
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("%w: task %s transition to running", ErrTaskTransitionNotApplied, taskID)
	}
	return nil
}

func (s *RegistryService) finishTaskWithError(taskID string, err error) error {
	now := s.nowFn()
	var commandErr *CommandExecutionError
	switch {
	case errors.Is(err, ErrNoCapabilityWorker):
		return s.finishTask(taskID, TaskStatusFailed, nil, defaultTaskNoWorkerCode, "no online worker supports capability", now)
	case errors.Is(err, ErrNoWorkerCapacity):
		return s.finishTask(taskID, TaskStatusFailed, nil, defaultTaskNoCapacityCode, "no online worker capacity for capability", now)
	case errors.Is(err, context.DeadlineExceeded):
		return s.finishTask(taskID, TaskStatusTimeout, nil, defaultTaskTimeoutCode, "task timed out", now)
	case errors.Is(err, context.Canceled):
		return s.finishTask(taskID, TaskStatusCanceled, nil, defaultTaskCanceledCode, "task canceled", now)
	case errors.As(err, &commandErr):
		code := strings.TrimSpace(commandErr.Code)
		if code == "" {
			code = defaultTaskDispatchErrCode
		}
		return s.finishTask(taskID, TaskStatusFailed, nil, code, commandErr.Message, now)
	case status.Code(err) == codes.DeadlineExceeded:
		return s.finishTask(taskID, TaskStatusTimeout, nil, defaultTaskTimeoutCode, "task timed out", now)
	default:
		return s.finishTask(taskID, TaskStatusFailed, nil, defaultTaskDispatchErrCode, err.Error(), now)
	}
}

func (s *RegistryService) finishTask(taskID string, statusValue TaskStatus, resultJSON []byte, errorCode string, errorMessage string, completedAt time.Time) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return errors.New("task_id is required")
	}
	if completedAt.IsZero() {
		completedAt = s.nowFn()
	}

	resultPayload := string(resultJSON)
	if resultPayload == "" {
		resultPayload = ""
	}

	queries := s.taskQueries()
	if queries == nil {
		s.completeTaskRuntime(taskID)
		return errors.New("task store is unavailable")
	}
	rows, err := queries.MarkTaskTerminal(context.Background(), sqlc.MarkTaskTerminalParams{
		Status:            string(statusValue),
		ResultJson:        resultPayload,
		ErrorCode:         strings.TrimSpace(errorCode),
		ErrorMessage:      strings.TrimSpace(errorMessage),
		UpdatedAtUnixMs:   completedAt.UnixMilli(),
		CompletedAtUnixMs: completedAt.UnixMilli(),
		ExpiresAtUnixMs:   completedAt.Add(s.taskRetention).UnixMilli(),
		TaskID:            taskID,
	})

	s.completeTaskRuntime(taskID)
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("%w: task %s terminal transition", ErrTaskTransitionNotApplied, taskID)
	}
	return nil
}

func (s *RegistryService) failTaskOnPersistenceError(taskID string, stage string, cause error) error {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		stage = "unknown_stage"
	}
	if cause == nil {
		cause = errors.New("unknown persistence error")
	}

	message := fmt.Sprintf("failed to persist task state at %s: %v", stage, cause)
	if err := s.finishTask(taskID, TaskStatusFailed, nil, defaultTaskPersistErrCode, message, s.nowFn()); err != nil {
		criticalErr := fmt.Errorf("task %s persistence fallback failed at %s: original=%w fallback=%v", taskID, stage, cause, err)
		log.Printf("CRITICAL: %v", criticalErr)
		if s.criticalPersistenceFailureFn != nil {
			s.criticalPersistenceFailureFn(criticalErr)
		}
		return criticalErr
	}
	return nil
}

func (s *RegistryService) closeTaskRuntimeRecord(record *taskRecord) {
	if record == nil {
		return
	}
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
	nodeIDs := s.store.ListOnlineNodeIDsByCapability(capability, now, time.Duration(s.offlineTTLSec)*time.Second)
	if len(nodeIDs) == 0 {
		return ErrNoCapabilityWorker
	}

	for _, nodeID := range nodeIDs {
		session := s.getSession(nodeID)
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

func (s *RegistryService) pruneExpiredTasks(now time.Time) error {
	queries := s.taskQueries()
	if s == nil || queries == nil {
		return nil
	}
	_, err := queries.DeleteExpiredTerminalTasks(context.Background(), now.UnixMilli())
	return err
}

func (s *RegistryService) maybePruneExpiredTasks(now time.Time) {
	if s == nil {
		return
	}
	nowMS := now.UnixMilli()
	minIntervalMS := inlineTaskPruneMinInterval.Milliseconds()
	for {
		last := s.lastInlineTaskPruneUnixMs.Load()
		if last > 0 && nowMS-last < minIntervalMS {
			return
		}
		if s.lastInlineTaskPruneUnixMs.CompareAndSwap(last, nowMS) {
			break
		}
	}
	if err := s.pruneExpiredTasks(now); err != nil {
		log.Printf("task prune failed during submit: %v", err)
	}
}

func (s *RegistryService) taskQueries() *sqlc.Queries {
	if s == nil || s.store == nil || s.store.Persistence() == nil {
		return nil
	}
	return s.store.Persistence().Queries
}

func (s *RegistryService) getTaskByID(taskID string) (dbTaskSnapshot, bool) {
	if s.taskQueries() == nil {
		return dbTaskSnapshot{}, false
	}
	task, err := s.taskQueries().GetTaskByID(context.Background(), taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return dbTaskSnapshot{}, false
	}
	if err != nil {
		return dbTaskSnapshot{}, false
	}
	return convertDBTask(task), true
}

func (s *RegistryService) getTaskByOwnerAndRequest(ownerID string, requestID string) (dbTaskSnapshot, bool) {
	if s.taskQueries() == nil {
		return dbTaskSnapshot{}, false
	}
	task, err := s.taskQueries().GetTaskByOwnerAndRequest(context.Background(), sqlc.GetTaskByOwnerAndRequestParams{
		OwnerID:   ownerID,
		RequestID: requestID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return dbTaskSnapshot{}, false
	}
	if err != nil {
		return dbTaskSnapshot{}, false
	}
	return convertDBTask(task), true
}

func (s *RegistryService) setTaskRuntime(taskID string, record *taskRecord) {
	s.tasksMu.Lock()
	s.tasks[taskID] = record
	s.tasksMu.Unlock()
}

func (s *RegistryService) getTaskRuntime(taskID string) *taskRecord {
	s.tasksMu.RLock()
	record := s.tasks[taskID]
	s.tasksMu.RUnlock()
	return record
}

func (s *RegistryService) completeTaskRuntime(taskID string) {
	s.tasksMu.Lock()
	record := s.tasks[taskID]
	if record != nil {
		delete(s.tasks, taskID)
	}
	s.tasksMu.Unlock()
	s.closeTaskRuntimeRecord(record)
}

type dbTaskSnapshot struct {
	taskID       string
	ownerID      string
	requestID    string
	commandID    string
	capability   string
	status       TaskStatus
	resultJSON   []byte
	errorCode    string
	errorMessage string
	createdAt    time.Time
	updatedAt    time.Time
	deadlineAt   time.Time
	completedAt  *time.Time
	expiresAt    time.Time
}

func convertDBTask(task sqlc.Task) dbTaskSnapshot {
	var completedAt *time.Time
	if task.CompletedAtUnixMs > 0 {
		completed := time.UnixMilli(task.CompletedAtUnixMs)
		completedAt = &completed
	}
	return dbTaskSnapshot{
		taskID:       task.TaskID,
		ownerID:      task.OwnerID,
		requestID:    task.RequestID,
		commandID:    task.CommandID,
		capability:   task.Capability,
		status:       TaskStatus(task.Status),
		resultJSON:   []byte(task.ResultJson),
		errorCode:    task.ErrorCode,
		errorMessage: task.ErrorMessage,
		createdAt:    time.UnixMilli(task.CreatedAtUnixMs),
		updatedAt:    time.UnixMilli(task.UpdatedAtUnixMs),
		deadlineAt:   time.UnixMilli(task.DeadlineAtUnixMs),
		completedAt:  completedAt,
		expiresAt:    time.UnixMilli(task.ExpiresAtUnixMs),
	}
}

func snapshotTask(task dbTaskSnapshot) TaskSnapshot {
	return TaskSnapshot{
		TaskID:       task.taskID,
		RequestID:    task.requestID,
		CommandID:    task.commandID,
		Capability:   task.capability,
		Status:       task.status,
		ResultJSON:   append([]byte(nil), task.resultJSON...),
		ErrorCode:    task.errorCode,
		ErrorMessage: task.errorMessage,
		CreatedAt:    task.createdAt,
		UpdatedAt:    task.updatedAt,
		DeadlineAt:   task.deadlineAt,
		CompletedAt:  task.completedAt,
	}
}

func isTaskTerminal(statusValue TaskStatus) bool {
	switch statusValue {
	case TaskStatusSucceeded, TaskStatusFailed, TaskStatusTimeout, TaskStatusCanceled:
		return true
	default:
		return false
	}
}

func isTaskOwnerRequestConflict(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "idx_tasks_owner_request_unique") || strings.Contains(lower, "tasks.owner_id")
}
