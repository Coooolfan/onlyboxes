package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

func TestBuildCommandResultTerminalResourceSuccess(t *testing.T) {
	originalRunTerminalResource := runTerminalResource
	t.Cleanup(func() {
		runTerminalResource = originalRunTerminalResource
	})

	runTerminalResource = func(_ context.Context, req terminalResourceRequest) (terminalResourceRunResult, error) {
		if req.SessionID != "sess-1" || req.FilePath != "app/main.py" {
			t.Fatalf("unexpected request: %#v", req)
		}
		if req.Headers["x-amz-acl"] != "public-read" {
			t.Fatalf("expected export headers to be decoded, got %#v", req.Headers)
		}
		return terminalResourceRunResult{
			SessionID: "sess-1",
			FilePath:  "app/main.py",
			MIMEType:  "text/x-python",
			SizeBytes: 3,
			Blob:      []byte("abc"),
		}, nil
	}

	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:   "cmd-term-res-1",
		Capability:  terminalResourceCapabilityDeclared,
		PayloadJson: []byte(`{"session_id":"sess-1","file_path":"app/main.py","action":"read","headers":{"x-amz-acl":"public-read"}}`),
	})

	result := req.GetCommandResult()
	if result == nil {
		t.Fatalf("expected command_result payload")
	}
	if result.GetError() != nil {
		t.Fatalf("expected success, got error %#v", result.GetError())
	}

	decoded := terminalResourceRunResult{}
	if err := json.Unmarshal(result.GetPayloadJson(), &decoded); err != nil {
		t.Fatalf("invalid payload: %v", err)
	}
	if decoded.SessionID != "sess-1" || decoded.FilePath != "app/main.py" || string(decoded.Blob) != "abc" {
		t.Fatalf("unexpected result payload: %#v", decoded)
	}
}

func TestBuildCommandResultTerminalResourceInvalidPayload(t *testing.T) {
	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:   "cmd-term-res-invalid",
		Capability:  terminalResourceCapabilityDeclared,
		PayloadJson: []byte(`{"session_id":"sess-1"}`),
	})
	result := req.GetCommandResult()
	if result == nil || result.GetError() == nil {
		t.Fatalf("expected invalid payload error, got %#v", result)
	}
	if result.GetError().GetCode() != terminalExecCodeInvalidPayload {
		t.Fatalf("expected invalid_payload, got %s", result.GetError().GetCode())
	}
}

func TestBuildCommandResultTerminalResourceInvalidExportPayload(t *testing.T) {
	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:   "cmd-term-res-invalid-export",
		Capability:  terminalResourceCapabilityDeclared,
		PayloadJson: []byte(`{"session_id":"sess-1","file_path":"app/main.py","action":"export"}`),
	})
	result := req.GetCommandResult()
	if result == nil || result.GetError() == nil {
		t.Fatalf("expected invalid payload error, got %#v", result)
	}
	if result.GetError().GetCode() != terminalExecCodeInvalidPayload {
		t.Fatalf("expected invalid_payload, got %s", result.GetError().GetCode())
	}
}

func TestBuildCommandResultTerminalResourceSessionError(t *testing.T) {
	originalRunTerminalResource := runTerminalResource
	t.Cleanup(func() {
		runTerminalResource = originalRunTerminalResource
	})

	runTerminalResource = func(_ context.Context, _ terminalResourceRequest) (terminalResourceRunResult, error) {
		return terminalResourceRunResult{}, newTerminalExecError(terminalResourceCodeFileNotFound, "file not found")
	}

	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:   "cmd-term-res-err",
		Capability:  terminalResourceCapabilityDeclared,
		PayloadJson: []byte(`{"session_id":"sess-1","file_path":"missing.txt"}`),
	})
	result := req.GetCommandResult()
	if result == nil || result.GetError() == nil {
		t.Fatalf("expected error result, got %#v", result)
	}
	if result.GetError().GetCode() != terminalResourceCodeFileNotFound {
		t.Fatalf("expected file_not_found, got %s", result.GetError().GetCode())
	}
}

func TestTerminalSessionManagerResolveResourceValidateAndRead(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
	})

	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		if args[0] != "exec" {
			return dockerCommandResult{ExitCode: 0}
		}
		action := argValue(args, "--action")
		switch action {
		case terminalResourceActionValidate:
			return dockerCommandResult{
				Stdout:   `{"mime_type":"text/plain","size_bytes":5}`,
				ExitCode: 0,
			}
		case terminalResourceActionRead:
			return dockerCommandResult{
				Stdout:   `{"mime_type":"text/plain","size_bytes":5,"blob":"aGVsbG8="}`,
				ExitCode: 0,
			}
		default:
			t.Fatalf("unexpected action: %q args=%#v", action, args)
			return dockerCommandResult{}
		}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024,
	})
	defer manager.Close()

	manager.mu.Lock()
	manager.sessions["sess-1"] = readyTerminalSession("sess-1", "container-1", time.Now().Add(time.Minute), 0)
	manager.mu.Unlock()

	validateResult, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: "sess-1",
		FilePath:  "/tmp/hello.txt",
		Action:    terminalResourceActionValidate,
	})
	if err != nil {
		t.Fatalf("validate failed: %v", err)
	}
	if validateResult.SizeBytes != 5 || validateResult.MIMEType != "text/plain" {
		t.Fatalf("unexpected validate result: %#v", validateResult)
	}

	readResult, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: "sess-1",
		FilePath:  "/tmp/hello.txt",
		Action:    terminalResourceActionRead,
	})
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(readResult.Blob) != "hello" {
		t.Fatalf("unexpected read blob: %q", string(readResult.Blob))
	}
}

func TestTerminalSessionManagerResolveResourceDomainErrors(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
	})

	tests := []struct {
		name      string
		stdout    string
		exitCode  int
		errorCode string
	}{
		{
			name:      "file_not_found",
			stdout:    `{"error":"file_not_found","message":"file not found"}`,
			exitCode:  10,
			errorCode: terminalResourceCodeFileNotFound,
		},
		{
			name:      "path_is_directory",
			stdout:    `{"error":"path_is_directory","message":"path is directory"}`,
			exitCode:  11,
			errorCode: terminalResourceCodePathIsDir,
		},
		{
			name:      "file_too_large",
			stdout:    `{"error":"file_too_large","message":"file exceeds read limit"}`,
			exitCode:  12,
			errorCode: terminalResourceCodeFileTooLarge,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
				if args[0] == "exec" {
					return dockerCommandResult{
						Stdout:   tc.stdout,
						ExitCode: tc.exitCode,
					}
				}
				return dockerCommandResult{ExitCode: 0}
			}

			manager := newTerminalSessionManager(terminalSessionManagerConfig{
				LeaseMinSec:      60,
				LeaseMaxSec:      1800,
				LeaseDefaultSec:  60,
				OutputLimitBytes: 1024,
			})
			defer manager.Close()

			manager.mu.Lock()
			manager.sessions["sess-1"] = readyTerminalSession("sess-1", "container-1", time.Now().Add(time.Minute), 0)
			manager.mu.Unlock()

			_, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
				SessionID: "sess-1",
				FilePath:  "/tmp/hello.txt",
				Action:    terminalResourceActionRead,
			})
			var terminalErr *terminalExecError
			if !errors.As(err, &terminalErr) {
				t.Fatalf("expected terminalExecError, got %v", err)
			}
			if terminalErr.Code() != tc.errorCode {
				t.Fatalf("expected code=%s, got %s", tc.errorCode, terminalErr.Code())
			}
		})
	}
}

func TestTerminalSessionManagerResolveResourceProbeExecFailureReportsDockerError(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
	})

	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		if args[0] == "exec" {
			return dockerCommandResult{
				Stdout:   `OCI runtime exec failed: exec: "python3": executable file not found in $PATH`,
				ExitCode: 126,
			}
		}
		return dockerCommandResult{ExitCode: 0}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024,
	})
	defer manager.Close()

	manager.mu.Lock()
	manager.sessions["sess-1"] = readyTerminalSession("sess-1", "container-1", time.Now().Add(time.Minute), 0)
	manager.mu.Unlock()

	_, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: "sess-1",
		FilePath:  "/tmp/hello.txt",
		Action:    terminalResourceActionValidate,
	})
	if err == nil {
		t.Fatalf("expected docker exec failure")
	}
	if !strings.Contains(err.Error(), `docker exec failed: exit code=126`) {
		t.Fatalf("expected exit code in error, got %v", err)
	}
	if !strings.Contains(err.Error(), `stdout=OCI runtime exec failed`) {
		t.Fatalf("expected stdout summary in error, got %v", err)
	}
}

func TestTerminalSessionManagerResolveResourceExportSuccess(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
	})

	var uploadedBody string
	var uploadedACL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("expected PUT request, got %s", r.Method)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upload body: %v", err)
		}
		uploadedBody = string(body)
		uploadedACL = r.Header.Get("x-amz-acl")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var copiedTempPath string
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "exec":
			return dockerCommandResult{
				Stdout:   `{"mime_type":"text/plain","size_bytes":5}`,
				ExitCode: 0,
			}
		case "cp":
			copiedTempPath = args[2]
			if err := os.WriteFile(copiedTempPath, []byte("hello"), 0o600); err != nil {
				t.Fatalf("write copied temp file: %v", err)
			}
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{ExitCode: 0}
		}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024,
	})
	defer manager.Close()

	manager.mu.Lock()
	manager.sessions["sess-1"] = readyTerminalSession("sess-1", "container-1", time.Now().Add(time.Minute), 0)
	manager.mu.Unlock()

	result, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: "sess-1",
		FilePath:  "/tmp/hello.txt",
		Action:    terminalResourceActionExport,
		SignedURL: server.URL,
		Headers:   map[string]string{"x-amz-acl": "public-read"},
	})
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	if result.MIMEType != "text/plain" || result.SizeBytes != 5 {
		t.Fatalf("unexpected export result: %#v", result)
	}
	if uploadedBody != "hello" {
		t.Fatalf("unexpected uploaded body: %q", uploadedBody)
	}
	if uploadedACL != "public-read" {
		t.Fatalf("expected x-amz-acl header to be forwarded, got %q", uploadedACL)
	}
	if copiedTempPath == "" {
		t.Fatalf("expected docker cp temp path to be captured")
	}
	if _, err := os.Stat(copiedTempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected temp file cleanup, got err=%v", err)
	}
}

func TestTerminalSessionManagerResolveResourceExportUploadFailure(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upload failed", http.StatusInternalServerError)
	}))
	defer server.Close()

	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "exec":
			return dockerCommandResult{
				Stdout:   `{"mime_type":"text/plain","size_bytes":5}`,
				ExitCode: 0,
			}
		case "cp":
			if err := os.WriteFile(args[2], []byte("hello"), 0o600); err != nil {
				t.Fatalf("write copied temp file: %v", err)
			}
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{ExitCode: 0}
		}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024,
	})
	defer manager.Close()

	manager.mu.Lock()
	manager.sessions["sess-1"] = readyTerminalSession("sess-1", "container-1", time.Now().Add(time.Minute), 0)
	manager.mu.Unlock()

	_, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: "sess-1",
		FilePath:  "/tmp/hello.txt",
		Action:    terminalResourceActionExport,
		SignedURL: server.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "upload export file failed") {
		t.Fatalf("expected upload error, got %v", err)
	}
}

func TestTerminalSessionManagerResolveResourceExportRejectsOversizedFile(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
	})

	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "exec":
			return dockerCommandResult{
				Stdout:   `{"mime_type":"text/plain","size_bytes":10}`,
				ExitCode: 0,
			}
		case "cp":
			t.Fatalf("docker cp should not be called for oversized files")
			return dockerCommandResult{}
		default:
			return dockerCommandResult{ExitCode: 0}
		}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024,
		ExportMaxBytes:   3,
	})
	defer manager.Close()

	manager.mu.Lock()
	manager.sessions["sess-export-limit"] = readyTerminalSession("sess-export-limit", "container-1", time.Now().Add(time.Minute), 0)
	manager.mu.Unlock()

	_, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: "sess-export-limit",
		FilePath:  "/workspace/large.bin",
		Action:    terminalResourceActionExport,
		SignedURL: "https://uploads.example.com/put",
	})
	var terminalErr *terminalExecError
	if !errors.As(err, &terminalErr) || terminalErr.Code() != terminalResourceCodeFileTooLarge {
		t.Fatalf("expected file_too_large, got %v", err)
	}
}

func TestTerminalSessionManagerResolveResourceSessionRules(t *testing.T) {
	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024,
	})
	defer manager.Close()

	_, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: "missing",
		FilePath:  "/tmp/hello.txt",
	})
	var terminalErr *terminalExecError
	if !errors.As(err, &terminalErr) || terminalErr.Code() != terminalExecCodeSessionNotFound {
		t.Fatalf("expected session_not_found, got %v", err)
	}

	manager.mu.Lock()
	manager.sessions["busy"] = readyTerminalSession("busy", "container-1", time.Now().Add(time.Minute), 1)
	manager.mu.Unlock()

	_, err = manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: "busy",
		FilePath:  "/tmp/hello.txt",
	})
	if !errors.As(err, &terminalErr) || terminalErr.Code() != terminalExecCodeSessionBusy {
		t.Fatalf("expected session_busy, got %v", err)
	}
}

func TestTerminalSessionManagerResolveResourceTimeoutDestroysSession(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
	})

	var calls [][]string
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "exec" {
			return dockerCommandResult{Err: context.DeadlineExceeded}
		}
		return dockerCommandResult{ExitCode: 0}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024,
	})
	defer manager.Close()

	manager.mu.Lock()
	manager.sessions["sess-1"] = readyTerminalSession("sess-1", "container-1", time.Now().Add(time.Minute), 0)
	manager.mu.Unlock()

	_, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: "sess-1",
		FilePath:  "/tmp/hello.txt",
		Action:    terminalResourceActionRead,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}

	manager.mu.Lock()
	_, ok := manager.sessions["sess-1"]
	manager.mu.Unlock()
	if ok {
		t.Fatalf("expected session to be removed after timeout")
	}
	if len(calls) < 2 {
		t.Fatalf("expected exec + rm calls, got %#v", calls)
	}
}

func TestTerminalExecDockerResourceArgs(t *testing.T) {
	got := terminalExecDockerResourceArgs("container-a", terminalResourceActionRead, "/tmp/a.txt", 256)
	want := []string{
		"exec",
		"container-a",
		"python3",
		"-c",
		terminalResourceProbeScript,
		"--action",
		terminalResourceActionRead,
		"--file-path",
		"/tmp/a.txt",
		"--max-read-bytes",
		"256",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected resource args:\nwant=%#v\ngot=%#v", want, got)
	}
}
