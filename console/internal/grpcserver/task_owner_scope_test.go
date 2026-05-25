package grpcserver

import (
	"encoding/json"
	"testing"
)

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
		[]byte(`{"session_id":"session-1","file_path":"/tmp/report.zip","action":"export","signed_url":"https://uploads.example.com/put","headers":{"x-amz-acl":"public-read"}}`),
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
}
