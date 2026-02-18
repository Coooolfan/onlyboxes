package grpcserver

import "testing"

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
