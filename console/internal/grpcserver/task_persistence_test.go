package grpcserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

func insertQueuedTaskForTest(t *testing.T, svc *RegistryService, taskID string, ownerID string, capability string, now time.Time) {
	t.Helper()
	queries := svc.taskQueries()
	if queries == nil {
		t.Fatalf("task queries must be available")
	}
	if err := queries.InsertTask(context.Background(), sqlc.InsertTaskParams{
		TaskID:            taskID,
		OwnerID:           ownerID,
		RequestID:         "",
		Capability:        capability,
		InputJson:         `{"message":"hello"}`,
		Status:            string(TaskStatusQueued),
		CommandID:         "",
		ResultJson:        "",
		ErrorCode:         "",
		ErrorMessage:      "",
		CreatedAtUnixMs:   now.UnixMilli(),
		UpdatedAtUnixMs:   now.UnixMilli(),
		DeadlineAtUnixMs:  now.Add(30 * time.Second).UnixMilli(),
		CompletedAtUnixMs: 0,
		ExpiresAtUnixMs:   0,
	}); err != nil {
		t.Fatalf("insert task: %v", err)
	}
}

func TestExecuteTaskMarksPersistenceErrorWhenMarkDispatchedFails(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	now := time.Unix(1_700_000_000, 0)
	svc.nowFn = func() time.Time { return now }

	const taskID = "task-persist-dispatched-fail"
	const ownerID = "owner-a"
	insertQueuedTaskForTest(t, svc, taskID, ownerID, "echo", now)

	if _, err := svc.store.Persistence().SQL.ExecContext(
		context.Background(),
		`CREATE TRIGGER fail_mark_dispatched
BEFORE UPDATE OF status ON tasks
WHEN NEW.status = 'dispatched'
BEGIN
  SELECT RAISE(FAIL, 'forced mark dispatched failure');
END`,
	); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	runtime := &taskRecord{
		id:     taskID,
		done:   make(chan struct{}),
		cancel: func() {},
	}
	svc.setTaskRuntime(taskID, runtime)

	svc.executeTask(context.Background(), taskID, ownerID, "echo", []byte(`{"message":"hello"}`))

	task, err := svc.taskQueries().GetTaskByID(context.Background(), taskID)
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.Status != string(TaskStatusFailed) {
		t.Fatalf("expected task status failed, got %q", task.Status)
	}
	if task.ErrorCode != defaultTaskPersistErrCode {
		t.Fatalf("expected error_code=%q, got %q", defaultTaskPersistErrCode, task.ErrorCode)
	}
	if !strings.Contains(task.ErrorMessage, "mark_dispatched") {
		t.Fatalf("expected error_message to include stage, got %q", task.ErrorMessage)
	}

	select {
	case <-runtime.done:
	default:
		t.Fatalf("expected task runtime done channel to be closed")
	}
}

func TestTaskStateTransitionsReturnErrorWhenDBUnavailable(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	now := time.Unix(1_700_000_100, 0)
	svc.nowFn = func() time.Time { return now }

	const taskID = "task-persist-db-closed"
	insertQueuedTaskForTest(t, svc, taskID, "owner-a", "echo", now)

	if err := svc.store.Persistence().Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	if err := svc.markTaskDispatched(taskID); err == nil {
		t.Fatalf("expected markTaskDispatched to return error when db is closed")
	}
	if err := svc.markTaskRunning(taskID, "cmd-1"); err == nil {
		t.Fatalf("expected markTaskRunning to return error when db is closed")
	}
	if err := svc.finishTask(taskID, TaskStatusSucceeded, []byte(`{"ok":true}`), "", "", now); err == nil {
		t.Fatalf("expected finishTask to return error when db is closed")
	}
}

func TestCancelTaskReturnsErrorWhenTerminalWriteFails(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	now := time.Unix(1_700_000_200, 0)
	svc.nowFn = func() time.Time { return now }

	const taskID = "task-cancel-write-fail"
	const ownerID = "owner-a"
	insertQueuedTaskForTest(t, svc, taskID, ownerID, "echo", now)

	if _, err := svc.store.Persistence().SQL.ExecContext(
		context.Background(),
		`CREATE TRIGGER fail_mark_canceled
BEFORE UPDATE OF status ON tasks
WHEN NEW.status = 'canceled'
BEGIN
  SELECT RAISE(FAIL, 'forced cancel terminal failure');
END`,
	); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if _, err := svc.CancelTask(taskID, ownerID); err == nil {
		t.Fatalf("expected CancelTask to return error when terminal write fails")
	}
}

func TestFailTaskOnPersistenceErrorTriggersCriticalHookWhenFallbackAlsoFails(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	now := time.Unix(1_700_000_300, 0)
	svc.nowFn = func() time.Time { return now }

	const taskID = "task-persist-fallback-fail"
	insertQueuedTaskForTest(t, svc, taskID, "owner-a", "echo", now)

	var (
		hookCalled bool
		hookErr    error
	)
	svc.criticalPersistenceFailureFn = func(err error) {
		hookCalled = true
		hookErr = err
	}

	if err := svc.store.Persistence().Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	err := svc.failTaskOnPersistenceError(taskID, "mark_running", errors.New("original persistence error"))
	if err == nil {
		t.Fatalf("expected failTaskOnPersistenceError to return error when fallback write fails")
	}
	if !hookCalled {
		t.Fatalf("expected critical persistence failure hook to be called")
	}
	if hookErr == nil {
		t.Fatalf("expected critical hook to receive error")
	}
	if !strings.Contains(hookErr.Error(), "persistence fallback failed") {
		t.Fatalf("expected critical hook error context, got %v", hookErr)
	}
}
