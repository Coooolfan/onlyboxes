package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
)

type fakeEchoDispatcher struct {
	dispatch func(ctx context.Context, message string, timeout time.Duration) (string, error)
}

func (f *fakeEchoDispatcher) DispatchEcho(ctx context.Context, message string, timeout time.Duration) (string, error) {
	return f.dispatch(ctx, message, timeout)
}

func (f *fakeEchoDispatcher) SubmitTask(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
	return grpcserver.SubmitTaskResult{}, grpcserver.ErrNoCapabilityWorker
}

func (f *fakeEchoDispatcher) GetTask(taskID string) (grpcserver.TaskSnapshot, bool) {
	return grpcserver.TaskSnapshot{}, false
}

func (f *fakeEchoDispatcher) CancelTask(taskID string) (grpcserver.TaskSnapshot, error) {
	return grpcserver.TaskSnapshot{}, grpcserver.ErrTaskNotFound
}

func TestEchoCommandSuccess(t *testing.T) {
	store := registry.NewStore()
	dispatcher := &fakeEchoDispatcher{
		dispatch: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			if timeout != 5*time.Second {
				t.Fatalf("expected default timeout 5s, got %s", timeout)
			}
			return message, nil
		},
	}
	handler := NewWorkerHandler(store, 15*time.Second, dispatcher)
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/commands/echo", strings.NewReader(`{"message":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"message":"hello"`) {
		t.Fatalf("expected echo payload, got %s", rec.Body.String())
	}
}

func TestEchoCommandRejectsInvalidInput(t *testing.T) {
	store := registry.NewStore()
	handler := NewWorkerHandler(store, 15*time.Second, &fakeEchoDispatcher{
		dispatch: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			return message, nil
		},
	})
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/commands/echo", strings.NewReader(`{"message":"   ","timeout_ms":0}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEchoCommandMapsNoWorkerError(t *testing.T) {
	store := registry.NewStore()
	handler := NewWorkerHandler(store, 15*time.Second, &fakeEchoDispatcher{
		dispatch: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			return "", grpcserver.ErrNoEchoWorker
		},
	})
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/commands/echo", strings.NewReader(`{"message":"hello","timeout_ms":1000}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEchoCommandMapsCapacityError(t *testing.T) {
	store := registry.NewStore()
	handler := NewWorkerHandler(store, 15*time.Second, &fakeEchoDispatcher{
		dispatch: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			return "", grpcserver.ErrNoWorkerCapacity
		},
	})
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/commands/echo", strings.NewReader(`{"message":"hello","timeout_ms":1000}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEchoCommandMapsTimeoutError(t *testing.T) {
	store := registry.NewStore()
	handler := NewWorkerHandler(store, 15*time.Second, &fakeEchoDispatcher{
		dispatch: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			return "", grpcserver.ErrEchoTimeout
		},
	})
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/commands/echo", strings.NewReader(`{"message":"hello","timeout_ms":1000}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEchoCommandMapsExecutionError(t *testing.T) {
	store := registry.NewStore()
	handler := NewWorkerHandler(store, 15*time.Second, &fakeEchoDispatcher{
		dispatch: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			return "", &grpcserver.CommandExecutionError{
				Code:    "unsupported_capability",
				Message: "echo is disabled",
			}
		},
	})
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/commands/echo", strings.NewReader(`{"message":"hello","timeout_ms":1000}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", rec.Code, rec.Body.String())
	}
}
