package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
)

type fakeTaskDispatcher struct {
	submit func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error)
	get    func(taskID string) (grpcserver.TaskSnapshot, bool)
	cancel func(taskID string) (grpcserver.TaskSnapshot, error)
}

func (f *fakeTaskDispatcher) DispatchEcho(ctx context.Context, message string, timeout time.Duration) (string, error) {
	return message, nil
}

func (f *fakeTaskDispatcher) SubmitTask(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
	return f.submit(ctx, req)
}

func (f *fakeTaskDispatcher) GetTask(taskID string) (grpcserver.TaskSnapshot, bool) {
	return f.get(taskID)
}

func (f *fakeTaskDispatcher) CancelTask(taskID string) (grpcserver.TaskSnapshot, error) {
	return f.cancel(taskID)
}

func TestSubmitTaskAccepted(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, &fakeTaskDispatcher{
		submit: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			return grpcserver.SubmitTaskResult{
				Task: grpcserver.TaskSnapshot{
					TaskID:     "task-1",
					Capability: "echo",
					Status:     grpcserver.TaskStatusRunning,
					CreatedAt:  now,
					UpdatedAt:  now,
					DeadlineAt: now.Add(60 * time.Second),
				},
				Completed: false,
			}, nil
		},
		get: func(taskID string) (grpcserver.TaskSnapshot, bool) {
			return grpcserver.TaskSnapshot{}, false
		},
		cancel: func(taskID string) (grpcserver.TaskSnapshot, error) {
			return grpcserver.TaskSnapshot{}, nil
		},
	})
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(`{"capability":"echo","input":{"message":"hello"},"mode":"async"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status_url":"/api/v1/tasks/task-1"`) {
		t.Fatalf("expected status_url in payload, got %s", rec.Body.String())
	}
}

func TestSubmitTaskCompletedSuccess(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, &fakeTaskDispatcher{
		submit: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			completed := now.Add(1 * time.Second)
			return grpcserver.SubmitTaskResult{
				Task: grpcserver.TaskSnapshot{
					TaskID:      "task-2",
					Capability:  "echo",
					Status:      grpcserver.TaskStatusSucceeded,
					ResultJSON:  []byte(`{"message":"ok"}`),
					CreatedAt:   now,
					UpdatedAt:   completed,
					DeadlineAt:  now.Add(60 * time.Second),
					CompletedAt: &completed,
				},
				Completed: true,
			}, nil
		},
		get: func(taskID string) (grpcserver.TaskSnapshot, bool) {
			return grpcserver.TaskSnapshot{}, false
		},
		cancel: func(taskID string) (grpcserver.TaskSnapshot, error) {
			return grpcserver.TaskSnapshot{}, nil
		},
	})
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(`{"capability":"echo","input":{"message":"hello"},"mode":"sync"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("expected succeeded status, got %s", rec.Body.String())
	}
}

func TestSubmitTaskNoCapacity(t *testing.T) {
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, &fakeTaskDispatcher{
		submit: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			return grpcserver.SubmitTaskResult{}, grpcserver.ErrNoWorkerCapacity
		},
		get: func(taskID string) (grpcserver.TaskSnapshot, bool) {
			return grpcserver.TaskSnapshot{}, false
		},
		cancel: func(taskID string) (grpcserver.TaskSnapshot, error) {
			return grpcserver.TaskSnapshot{}, nil
		},
	})
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(`{"capability":"echo","input":{"message":"hello"}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSubmitTaskRequestInProgress(t *testing.T) {
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, &fakeTaskDispatcher{
		submit: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			return grpcserver.SubmitTaskResult{}, grpcserver.ErrTaskRequestInProgress
		},
		get: func(taskID string) (grpcserver.TaskSnapshot, bool) {
			return grpcserver.TaskSnapshot{}, false
		},
		cancel: func(taskID string) (grpcserver.TaskSnapshot, error) {
			return grpcserver.TaskSnapshot{}, nil
		},
	})
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(`{"capability":"echo","input":{"message":"hello"},"request_id":"req-1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "task request already in progress") {
		t.Fatalf("expected conflict message, got %s", rec.Body.String())
	}
}

func TestGetTask(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, &fakeTaskDispatcher{
		submit: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			return grpcserver.SubmitTaskResult{}, nil
		},
		get: func(taskID string) (grpcserver.TaskSnapshot, bool) {
			if taskID != "task-3" {
				return grpcserver.TaskSnapshot{}, false
			}
			return grpcserver.TaskSnapshot{
				TaskID:     "task-3",
				Capability: "echo",
				Status:     grpcserver.TaskStatusRunning,
				CreatedAt:  now,
				UpdatedAt:  now,
				DeadlineAt: now.Add(30 * time.Second),
			}, true
		},
		cancel: func(taskID string) (grpcserver.TaskSnapshot, error) {
			return grpcserver.TaskSnapshot{}, nil
		},
	})
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/task-3", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var payload taskResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}
	if payload.TaskID != "task-3" {
		t.Fatalf("expected task-3, got %s", payload.TaskID)
	}
}

func TestCancelTaskTerminalConflict(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, &fakeTaskDispatcher{
		submit: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			return grpcserver.SubmitTaskResult{}, nil
		},
		get: func(taskID string) (grpcserver.TaskSnapshot, bool) {
			return grpcserver.TaskSnapshot{}, false
		},
		cancel: func(taskID string) (grpcserver.TaskSnapshot, error) {
			completed := now.Add(2 * time.Second)
			return grpcserver.TaskSnapshot{
				TaskID:      taskID,
				Capability:  "echo",
				Status:      grpcserver.TaskStatusSucceeded,
				CreatedAt:   now,
				UpdatedAt:   completed,
				DeadlineAt:  now.Add(60 * time.Second),
				CompletedAt: &completed,
			}, grpcserver.ErrTaskTerminal
		},
	})
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/task-5/cancel", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", rec.Code, rec.Body.String())
	}
}
