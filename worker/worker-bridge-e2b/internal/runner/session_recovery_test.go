package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b"
)

func TestE2BTerminalRecoveryMetadataUsesFullHashWithoutSessionID(t *testing.T) {
	sessionID := "owner-secret:session-secret"
	metadata := terminalSessionMetadata(sessionID)
	hash := metadata[terminalSessionMetadataKey]
	if len(hash) != 64 || strings.Contains(hash, "owner-secret") || hash != terminalSessionIDHash(sessionID) {
		t.Fatalf("metadata exposed or did not deterministically map the session: %#v", metadata)
	}
	if metadata[terminalSessionSchemaKey] != terminalSessionSchemaVersion ||
		metadata[terminalSessionWorkerMetadataKey] != terminalSessionWorkerMetadata {
		t.Fatalf("metadata markers missing: %#v", metadata)
	}
}

func TestE2BTerminalRecoveryUsesMetadataAndFreshConnection(t *testing.T) {
	sessionID := "owner-a:session-a"
	wantMetadata := terminalSessionMetadata(sessionID)
	var listedMetadata map[string]string
	var connectedID string
	var connectedTimeout int
	backend := &fakeE2BBackend{
		listFn: func(_ context.Context, metadata map[string]string) ([]e2b.SandboxInfo, error) {
			listedMetadata = metadata
			return []e2b.SandboxInfo{{
				ID:       "sandbox-a",
				State:    "running",
				Metadata: wantMetadata,
			}}, nil
		},
		connectFn: func(_ context.Context, sandboxID string, timeout int) (*e2b.Sandbox, error) {
			connectedID = sandboxID
			connectedTimeout = timeout
			return &e2b.Sandbox{ID: sandboxID, Domain: "e2b.app", AccessToken: "fresh-token"}, nil
		},
	}
	manager := newTestTerminalManager(backend, 1)
	manager.preserveOnClose = true
	defer manager.Close()

	lease := time.Now().Add(10 * time.Minute).Truncate(time.Millisecond)
	results := manager.Recover(context.Background(), []*registryv1.TerminalSessionRecoveryCandidate{
		{SessionId: sessionID, LeaseExpiresUnixMs: lease.UnixMilli()},
	})
	if len(results) != 1 || results[0].GetStatus() != registryv1.TerminalSessionRecoveryResult_RECOVERED {
		t.Fatalf("unexpected recovery result: %#v", results)
	}
	if listedMetadata[terminalSessionMetadataKey] != wantMetadata[terminalSessionMetadataKey] ||
		listedMetadata[terminalSessionSchemaKey] != terminalSessionSchemaVersion {
		t.Fatalf("unexpected metadata filter: %#v", listedMetadata)
	}
	if connectedID != "sandbox-a" || connectedTimeout < 599 || connectedTimeout > 601 {
		t.Fatalf("unexpected reconnect: id=%q timeout=%d", connectedID, connectedTimeout)
	}
	manager.mu.Lock()
	recovered := manager.sessions[sessionID]
	manager.mu.Unlock()
	if recovered == nil || recovered.sandbox.AccessToken != "fresh-token" ||
		!recovered.confirmedLeaseExpiresAt.Equal(lease) || recovered.inflight != 0 {
		t.Fatalf("unexpected recovered session: %#v", recovered)
	}
}

func TestE2BTerminalRecoveryRejectsDuplicateMetadataMatches(t *testing.T) {
	sessionID := "owner-a:session-duplicate"
	metadata := terminalSessionMetadata(sessionID)
	connectCalled := false
	killed := map[string]bool{}
	backend := &fakeE2BBackend{
		listFn: func(_ context.Context, _ map[string]string) ([]e2b.SandboxInfo, error) {
			return []e2b.SandboxInfo{
				{ID: "sandbox-a", Metadata: metadata},
				{ID: "sandbox-b", Metadata: metadata},
			}, nil
		},
		connectFn: func(_ context.Context, sandboxID string, timeout int) (*e2b.Sandbox, error) {
			connectCalled = true
			return nil, nil
		},
		killFn: func(_ context.Context, sandboxID string) error {
			killed[sandboxID] = true
			return nil
		},
	}
	manager := newTestTerminalManager(backend, 1)
	defer manager.Close()

	results := manager.Recover(context.Background(), []*registryv1.TerminalSessionRecoveryCandidate{
		{SessionId: sessionID, LeaseExpiresUnixMs: time.Now().Add(time.Minute).UnixMilli()},
	})
	if len(results) != 1 || results[0].GetStatus() != registryv1.TerminalSessionRecoveryResult_INVALID {
		t.Fatalf("unexpected recovery result: %#v", results)
	}
	if connectCalled {
		t.Fatal("duplicate metadata matches must not reconnect to an arbitrary sandbox")
	}
	if !killed["sandbox-a"] || !killed["sandbox-b"] {
		t.Fatalf("duplicate sandboxes were not isolated: %#v", killed)
	}
}

func TestE2BTerminalRecoveryNeverKillsNonCandidateSandbox(t *testing.T) {
	sessionID := "owner-a:session-safe-filter"
	killCalled := false
	backend := &fakeE2BBackend{
		listFn: func(_ context.Context, _ map[string]string) ([]e2b.SandboxInfo, error) {
			return []e2b.SandboxInfo{{
				ID: "unrelated-sandbox",
				Metadata: map[string]string{
					terminalSessionMetadataKey: "different-hash",
					terminalSessionSchemaKey:   terminalSessionSchemaVersion,
				},
			}}, nil
		},
		killFn: func(_ context.Context, _ string) error {
			killCalled = true
			return nil
		},
	}
	manager := newTestTerminalManager(backend, 1)
	defer manager.Close()
	results := manager.Recover(context.Background(), []*registryv1.TerminalSessionRecoveryCandidate{{
		SessionId: sessionID, LeaseExpiresUnixMs: time.Now().Add(time.Minute).UnixMilli(),
	}})
	if len(results) != 1 || results[0].GetStatus() != registryv1.TerminalSessionRecoveryResult_INVALID {
		t.Fatalf("unexpected recovery result: %#v", results)
	}
	if killCalled {
		t.Fatal("non-candidate E2B sandbox was killed")
	}
}

func TestE2BTerminalCreationAddsRecoveryMetadata(t *testing.T) {
	var metadata map[string]string
	backend := &fakeE2BBackend{
		createWithMetadataFn: func(_ context.Context, _ string, _ int, values map[string]string) (*e2b.Sandbox, error) {
			metadata = values
			return &e2b.Sandbox{ID: "sandbox-created", Domain: "test"}, nil
		},
	}
	manager := newTestTerminalManager(backend, 1)
	defer manager.Close()

	result, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "pwd",
		SessionID:       "owner-a:session-create",
		CreateIfMissing: true,
		LeaseTTLSec:     intPointer(10),
	})
	if err != nil {
		t.Fatalf("create terminal session: %v", err)
	}
	if !result.Created || metadata[terminalSessionWorkerMetadataKey] != terminalSessionWorkerMetadata ||
		metadata[terminalSessionMetadataKey] != terminalSessionIDHash("owner-a:session-create") ||
		metadata[terminalSessionSchemaKey] != terminalSessionSchemaVersion {
		t.Fatalf("unexpected recovery metadata: %#v", metadata)
	}
}

func TestE2BTerminalRecoveryIsIdempotent(t *testing.T) {
	sessionID := "owner-a:session-idempotent"
	metadata := terminalSessionMetadata(sessionID)
	listCalls := 0
	connectCalls := 0
	backend := &fakeE2BBackend{
		listFn: func(_ context.Context, _ map[string]string) ([]e2b.SandboxInfo, error) {
			listCalls++
			return []e2b.SandboxInfo{{ID: "sandbox-a", Metadata: metadata}}, nil
		},
		connectFn: func(_ context.Context, sandboxID string, _ int) (*e2b.Sandbox, error) {
			connectCalls++
			return &e2b.Sandbox{ID: sandboxID, AccessToken: "fresh-token"}, nil
		},
	}
	manager := newTestTerminalManager(backend, 1)
	defer manager.Close()
	lease := time.Now().Add(time.Minute).Truncate(time.Millisecond)
	candidates := []*registryv1.TerminalSessionRecoveryCandidate{{SessionId: sessionID, LeaseExpiresUnixMs: lease.UnixMilli()}}
	for range 2 {
		results := manager.Recover(context.Background(), candidates)
		if len(results) != 1 || results[0].GetStatus() != registryv1.TerminalSessionRecoveryResult_RECOVERED {
			t.Fatalf("unexpected recovery result: %#v", results)
		}
	}
	if listCalls != 1 || connectCalls != 1 || manager.ActiveSessionCount() != 1 {
		t.Fatalf("recovery was not idempotent: list=%d connect=%d active=%d", listCalls, connectCalls, manager.ActiveSessionCount())
	}
}

func TestE2BTerminalRecoveryReportsMissingAndSkipsExpiredCandidate(t *testing.T) {
	listCalls := 0
	backend := &fakeE2BBackend{
		listFn: func(_ context.Context, _ map[string]string) ([]e2b.SandboxInfo, error) {
			listCalls++
			return nil, nil
		},
	}
	manager := newTestTerminalManager(backend, 1)
	defer manager.Close()
	results := manager.Recover(context.Background(), []*registryv1.TerminalSessionRecoveryCandidate{
		{SessionId: "owner-a:session-missing", LeaseExpiresUnixMs: time.Now().Add(time.Minute).UnixMilli()},
		{SessionId: "owner-a:session-expired", LeaseExpiresUnixMs: time.Now().Add(-time.Minute).UnixMilli()},
	})
	if results[0].GetStatus() != registryv1.TerminalSessionRecoveryResult_MISSING ||
		results[1].GetStatus() != registryv1.TerminalSessionRecoveryResult_INVALID {
		t.Fatalf("unexpected recovery results: %#v", results)
	}
	if listCalls != 1 || manager.ActiveSessionCount() != 0 {
		t.Fatalf("expired candidate contacted E2B or consumed capacity: list=%d active=%d", listCalls, manager.ActiveSessionCount())
	}
}

func TestConcurrentE2BTerminalRecoveryUsesOneReservation(t *testing.T) {
	sessionID := "owner-a:session-concurrent"
	metadata := terminalSessionMetadata(sessionID)
	backend := &fakeE2BBackend{
		listFn: func(_ context.Context, _ map[string]string) ([]e2b.SandboxInfo, error) {
			return []e2b.SandboxInfo{{ID: "sandbox-a", Metadata: metadata}}, nil
		},
		connectFn: func(_ context.Context, sandboxID string, _ int) (*e2b.Sandbox, error) {
			return &e2b.Sandbox{ID: sandboxID, AccessToken: "fresh-token"}, nil
		},
	}
	manager := newTestTerminalManager(backend, 1)
	defer manager.Close()
	candidate := []*registryv1.TerminalSessionRecoveryCandidate{{
		SessionId: sessionID, LeaseExpiresUnixMs: time.Now().Add(time.Minute).UnixMilli(),
	}}
	done := make(chan []*registryv1.TerminalSessionRecoveryResult, 2)
	for range 2 {
		go func() { done <- manager.Recover(context.Background(), candidate) }()
	}
	for range 2 {
		if result := <-done; len(result) != 1 || result[0].GetStatus() != registryv1.TerminalSessionRecoveryResult_RECOVERED {
			t.Fatalf("unexpected concurrent recovery result: %#v", result)
		}
	}
	if manager.ActiveSessionCount() != 1 {
		t.Fatalf("concurrent recovery reserved %d sessions, want 1", manager.ActiveSessionCount())
	}
}

func intPointer(value int) *int { return &value }
