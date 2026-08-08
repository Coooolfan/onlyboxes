package runner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

func TestAllCapabilityDispatchersEncodeResults(t *testing.T) {
	originalPython := runPythonExec
	originalTerminal := runTerminalExec
	originalResource := runTerminalResource
	originalProxy := runTerminalProxy
	t.Cleanup(func() {
		runPythonExec = originalPython
		runTerminalExec = originalTerminal
		runTerminalResource = originalResource
		runTerminalProxy = originalProxy
	})
	runPythonExec = func(_ context.Context, code string) (pythonExecRunResult, error) {
		if code != "print(1)" {
			t.Fatalf("unexpected Python code %q", code)
		}
		return pythonExecRunResult{Output: "1\n", ExitCode: 0}, nil
	}
	runTerminalExec = func(_ context.Context, req terminalExecRequest) (terminalExecRunResult, error) {
		if req.Command != "pwd" || req.SessionID != "session-1" {
			t.Fatalf("unexpected terminal request: %#v", req)
		}
		return terminalExecRunResult{
			SessionID:          req.SessionID,
			Stdout:             "/workspace\n",
			LeaseExpiresUnixMS: 1234,
		}, nil
	}
	runTerminalResource = func(_ context.Context, req terminalResourceRequest) (terminalResourceRunResult, error) {
		if req.SessionID != "session-1" || req.FilePath != "/tmp/a.txt" || req.Action != "read" {
			t.Fatalf("unexpected resource request: %#v", req)
		}
		return terminalResourceRunResult{
			SessionID: req.SessionID,
			FilePath:  req.FilePath,
			MIMEType:  "text/plain",
			SizeBytes: 1,
			Blob:      []byte("a"),
		}, nil
	}
	runTerminalProxy = func(_ context.Context, sessionID string, port int, _ time.Time) (terminalProxyRunResult, error) {
		if sessionID != "session-1" || port != 8080 {
			t.Fatalf("unexpected proxy request: session=%q port=%d", sessionID, port)
		}
		return terminalProxyRunResult{URL: "https://8080-sandbox.e2b.app", TrafficToken: "traffic-secret"}, nil
	}

	tests := []struct {
		name       string
		capability string
		payload    string
		assert     func(*testing.T, []byte)
	}{
		{
			name:       "echo",
			capability: "echo",
			payload:    `{"message":"hello"}`,
			assert: func(t *testing.T, payload []byte) {
				if string(payload) != `{"message":"hello"}` {
					t.Fatalf("unexpected echo payload %s", payload)
				}
			},
		},
		{
			name:       "pythonExec",
			capability: "pythonExec",
			payload:    `{"code":"print(1)"}`,
			assert: func(t *testing.T, payload []byte) {
				var result pythonExecResult
				if json.Unmarshal(payload, &result) != nil || result.Output != "1\n" || result.ExitCode != 0 {
					t.Fatalf("unexpected Python result %s", payload)
				}
			},
		},
		{
			name:       "terminalExec",
			capability: "terminalExec",
			payload:    `{"command":"pwd","session_id":"session-1"}`,
			assert: func(t *testing.T, payload []byte) {
				var result terminalExecRunResult
				if json.Unmarshal(payload, &result) != nil || result.SessionID != "session-1" || result.Stdout != "/workspace\n" {
					t.Fatalf("unexpected terminal result %s", payload)
				}
			},
		},
		{
			name:       "terminalResource",
			capability: "terminalResource",
			payload:    `{"session_id":"session-1","file_path":"/tmp/a.txt","action":"read"}`,
			assert: func(t *testing.T, payload []byte) {
				var result terminalResourceRunResult
				if json.Unmarshal(payload, &result) != nil || string(result.Blob) != "a" || result.MIMEType != "text/plain" {
					t.Fatalf("unexpected resource result %s", payload)
				}
			},
		},
		{
			name:       "terminalProxy",
			capability: "terminalProxy",
			payload:    `{"session_id":"session-1","port":8080}`,
			assert: func(t *testing.T, payload []byte) {
				var result terminalProxyRunResult
				if json.Unmarshal(payload, &result) != nil || result.URL != "https://8080-sandbox.e2b.app" || result.TrafficToken != "traffic-secret" {
					t.Fatalf("unexpected proxy result %s", payload)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := buildCommandResultWithContext(context.Background(), &registryv1.CommandDispatch{
				CommandId:   "command-" + tc.name,
				Capability:  tc.capability,
				PayloadJson: []byte(tc.payload),
			})
			result := request.GetCommandResult()
			if result == nil || result.GetError() != nil || result.GetCompletedUnixMs() == 0 {
				t.Fatalf("unexpected command result: %#v", result)
			}
			tc.assert(t, result.GetPayloadJson())
		})
	}
}

func TestTerminalCapacityErrorPreservedInCommandResult(t *testing.T) {
	originalTerminal := runTerminalExec
	t.Cleanup(func() { runTerminalExec = originalTerminal })
	runTerminalExec = func(context.Context, terminalExecRequest) (terminalExecRunResult, error) {
		return terminalExecRunResult{}, newTerminalExecError(
			terminalExecCodeSessionCapacityExceeded,
			terminalExecCapacityMessage,
		)
	}

	request := buildCommandResultWithContext(context.Background(), &registryv1.CommandDispatch{
		CommandId:   "capacity",
		Capability:  "terminalExec",
		PayloadJson: []byte(`{"command":"pwd","session_id":"new","create_if_missing":true}`),
	})
	errorResult := request.GetCommandResult().GetError()
	if errorResult.GetCode() != terminalExecCodeSessionCapacityExceeded || errorResult.GetMessage() != terminalExecCapacityMessage {
		t.Fatalf("unexpected command error: %#v", errorResult)
	}
}

func TestExpiredDispatchDoesNotInvokeTool(t *testing.T) {
	originalTerminal := runTerminalExec
	t.Cleanup(func() { runTerminalExec = originalTerminal })
	called := false
	runTerminalExec = func(context.Context, terminalExecRequest) (terminalExecRunResult, error) {
		called = true
		return terminalExecRunResult{}, nil
	}
	request := buildCommandResultWithContext(context.Background(), &registryv1.CommandDispatch{
		CommandId:      "expired",
		Capability:     "terminalExec",
		PayloadJson:    []byte(`{"command":"pwd"}`),
		DeadlineUnixMs: time.Now().Add(-time.Second).UnixMilli(),
	})
	if called {
		t.Fatal("expired dispatch invoked the tool")
	}
	if code := request.GetCommandResult().GetError().GetCode(); code != "deadline_exceeded" {
		t.Fatalf("unexpected error code %q", code)
	}
}
