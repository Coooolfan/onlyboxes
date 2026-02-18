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

type fakeEchoDispatcher struct {
	dispatch   func(ctx context.Context, message string, timeout time.Duration) (string, error)
	submitTask func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error)
}

func (f *fakeEchoDispatcher) DispatchEcho(ctx context.Context, message string, timeout time.Duration) (string, error) {
	return f.dispatch(ctx, message, timeout)
}

func (f *fakeEchoDispatcher) SubmitTask(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
	if f.submitTask != nil {
		return f.submitTask(ctx, req)
	}
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
	handler := NewWorkerHandler(store, 15*time.Second, dispatcher, nil, "")
	router := NewRouter(handler, newTestConsoleAuth(t))

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
	}, nil, "")
	router := NewRouter(handler, newTestConsoleAuth(t))

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
	}, nil, "")
	router := NewRouter(handler, newTestConsoleAuth(t))

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
	}, nil, "")
	router := NewRouter(handler, newTestConsoleAuth(t))

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
	}, nil, "")
	router := NewRouter(handler, newTestConsoleAuth(t))

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
	}, nil, "")
	router := NewRouter(handler, newTestConsoleAuth(t))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/commands/echo", strings.NewReader(`{"message":"hello","timeout_ms":1000}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTerminalCommandSuccess(t *testing.T) {
	store := registry.NewStore()
	handler := NewWorkerHandler(store, 15*time.Second, &fakeEchoDispatcher{
		dispatch: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			return message, nil
		},
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			if req.Capability != terminalExecCapability {
				t.Fatalf("expected capability=%q, got %q", terminalExecCapability, req.Capability)
			}
			payload := terminalExecPayload{}
			if err := json.Unmarshal(req.InputJSON, &payload); err != nil {
				t.Fatalf("expected valid terminal payload, got %s", string(req.InputJSON))
			}
			if payload.Command != "pwd" {
				t.Fatalf("unexpected command payload: %#v", payload)
			}
			resultJSON, _ := json.Marshal(terminalCommandResponse{
				SessionID:          "session-1",
				Created:            true,
				Stdout:             "/workspace\n",
				Stderr:             "",
				ExitCode:           0,
				StdoutTruncated:    false,
				StderrTruncated:    false,
				LeaseExpiresUnixMS: 1234,
			})
			return grpcserver.SubmitTaskResult{
				Task: grpcserver.TaskSnapshot{
					TaskID:     "task-1",
					Capability: terminalExecCapability,
					Status:     grpcserver.TaskStatusSucceeded,
					ResultJSON: resultJSON,
				},
				Completed: true,
			}, nil
		},
	}, nil, "")
	router := NewRouter(handler, newTestConsoleAuth(t))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/commands/terminal", strings.NewReader(`{"command":"pwd","create_if_missing":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"session_id":"session-1"`) {
		t.Fatalf("expected terminal payload, got %s", rec.Body.String())
	}
}

func TestTerminalCommandStatusMappings(t *testing.T) {
	tests := []struct {
		name       string
		errorCode  string
		statusCode int
	}{
		{name: "session_not_found", errorCode: terminalExecSessionNotFoundCode, statusCode: http.StatusNotFound},
		{name: "session_busy", errorCode: terminalExecSessionBusyCode, statusCode: http.StatusConflict},
		{name: "invalid_payload", errorCode: terminalExecInvalidPayloadCode, statusCode: http.StatusBadRequest},
		{name: "no_capacity", errorCode: terminalTaskNoCapacityCode, statusCode: http.StatusTooManyRequests},
		{name: "no_worker", errorCode: terminalTaskNoWorkerCode, statusCode: http.StatusServiceUnavailable},
		{name: "other_failed", errorCode: "execution_failed", statusCode: http.StatusBadGateway},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := registry.NewStore()
			handler := NewWorkerHandler(store, 15*time.Second, &fakeEchoDispatcher{
				dispatch: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
					return message, nil
				},
				submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
					return grpcserver.SubmitTaskResult{
						Task: grpcserver.TaskSnapshot{
							TaskID:       "task-1",
							Capability:   terminalExecCapability,
							Status:       grpcserver.TaskStatusFailed,
							ErrorCode:    tc.errorCode,
							ErrorMessage: "terminal command failed",
						},
						Completed: true,
					}, nil
				},
			}, nil, "")
			router := NewRouter(handler, newTestConsoleAuth(t))

			req := httptest.NewRequest(http.MethodPost, "/api/v1/commands/terminal", strings.NewReader(`{"command":"pwd"}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != tc.statusCode {
				t.Fatalf("expected status %d, got %d body=%s", tc.statusCode, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestTerminalCommandRejectsInvalidInput(t *testing.T) {
	store := registry.NewStore()
	handler := NewWorkerHandler(store, 15*time.Second, &fakeEchoDispatcher{
		dispatch: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			return message, nil
		},
	}, nil, "")
	router := NewRouter(handler, newTestConsoleAuth(t))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/commands/terminal", strings.NewReader(`{"command":"   ","timeout_ms":0}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}
