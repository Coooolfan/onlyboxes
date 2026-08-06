package grpcserver

import (
	"encoding/json"
	"testing"
)

func TestTerminalSessionIntentForTaskInput(t *testing.T) {
	tests := []struct {
		name       string
		capability string
		input      []byte
		want       terminalSessionIntent
	}{
		{
			name:       "terminal exec without session id is known new",
			capability: taskCapabilityTerminalExec,
			input:      []byte(`{"command":"pwd"}`),
			want:       terminalSessionIntentKnownNew,
		},
		{
			name:       "terminal exec with blank session id is known new",
			capability: taskCapabilityTerminalExec,
			input:      []byte(`{"command":"pwd","session_id":"  "}`),
			want:       terminalSessionIntentKnownNew,
		},
		{
			name:       "caller supplied session id is unknown",
			capability: taskCapabilityTerminalExec,
			input:      []byte(`{"command":"pwd","session_id":"session-1"}`),
			want:       terminalSessionIntentUnknown,
		},
		{
			name:       "malformed input is unknown",
			capability: taskCapabilityTerminalExec,
			input:      []byte(`{"command":`),
			want:       terminalSessionIntentUnknown,
		},
		{
			name:       "terminal resource is unknown",
			capability: taskCapabilityTerminalResource,
			input:      []byte(`{"session_id":"session-1"}`),
			want:       terminalSessionIntentUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalSessionIntentForTaskInput(tc.capability, tc.input); got != tc.want {
				t.Fatalf("intent: got %d want %d", got, tc.want)
			}
		})
	}
}

func TestUnscopeTerminalSessionIDRejectsOwnerMismatch(t *testing.T) {
	_, ok := unscopeTerminalSessionID("owner-a", "obx:owner-b:session-1")
	if ok {
		t.Fatalf("expected owner mismatch to fail unscope")
	}
}

func TestScopeAndUnscopeTerminalSessionIDSupportsColonInExternalID(t *testing.T) {
	scoped := scopeTerminalSessionID("owner-a", "session:part:1")
	if scoped != "obx:owner-a:session:part:1" {
		t.Fatalf("unexpected scoped session_id: %q", scoped)
	}

	external, ok := unscopeTerminalSessionID("owner-a", scoped)
	if !ok {
		t.Fatalf("expected scoped session_id to be recoverable")
	}
	if external != "session:part:1" {
		t.Fatalf("expected external session_id to be preserved, got %q", external)
	}
}

func TestRestoreTaskResultOwnerScopeReturnsFalseOnOwnerMismatch(t *testing.T) {
	svc := &RegistryService{}
	_, ok := svc.restoreTaskResultOwnerScope(
		"owner-a",
		taskCapabilityTerminalExec,
		[]byte(`{"session_id":"obx:owner-b:session-1","stdout":"ok"}`),
	)
	if ok {
		t.Fatalf("expected restore to fail for mismatched owner scope")
	}
}

func TestScopeTaskInputByOwnerPreservesTerminalResourceHeaders(t *testing.T) {
	svc := &RegistryService{}

	scoped, err := svc.scopeTaskInputByOwner(
		taskCapabilityTerminalResource,
		"owner-a",
		[]byte(`{"session_id":"session-1","file_path":"/tmp/report.zip","action":"export","signed_url":"https://uploads.example.com/put","headers":{"Authorization":"Bearer ignored","Content-Length":"123","Content-MD5":"abc","Content-Type":"application/zip","Host":"uploads.example.com","Transfer-Encoding":"chunked","X-Amz-Acl":"public-read","x-amz-meta-job":"job-1"}}`),
	)
	if err != nil {
		t.Fatalf("scope task input: %v", err)
	}

	var payload terminalResourceScopedPayload
	if err := json.Unmarshal(scoped, &payload); err != nil {
		t.Fatalf("decode scoped payload: %v", err)
	}
	if payload.SessionID != "obx:owner-a:session-1" {
		t.Fatalf("unexpected scoped session_id: %q", payload.SessionID)
	}
	if payload.Headers["x-amz-acl"] != "public-read" {
		t.Fatalf("expected terminalResource headers to be preserved, got %#v", payload.Headers)
	}
	if payload.Headers["x-amz-meta-job"] != "job-1" {
		t.Fatalf("expected x-amz-* header to be preserved, got %#v", payload.Headers)
	}
	if payload.Headers["Content-Type"] != "application/zip" {
		t.Fatalf("expected Content-Type header to be preserved, got %#v", payload.Headers)
	}
	if payload.Headers["Content-MD5"] != "abc" {
		t.Fatalf("expected Content-MD5 header to be preserved, got %#v", payload.Headers)
	}
	if _, ok := payload.Headers["Authorization"]; ok {
		t.Fatalf("expected Authorization header to be filtered, got %#v", payload.Headers)
	}
	if _, ok := payload.Headers["Content-Length"]; ok {
		t.Fatalf("expected Content-Length header to be filtered, got %#v", payload.Headers)
	}
	if _, ok := payload.Headers["Host"]; ok {
		t.Fatalf("expected Host header to be filtered, got %#v", payload.Headers)
	}
	if _, ok := payload.Headers["Transfer-Encoding"]; ok {
		t.Fatalf("expected Transfer-Encoding header to be filtered, got %#v", payload.Headers)
	}
}
