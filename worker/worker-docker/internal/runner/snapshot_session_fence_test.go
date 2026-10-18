package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

func TestSnapshotHeldSourceBlocksExecutionAndSurvivesLeaseAndClose(t *testing.T) {
	var removals atomic.Int32
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		if args[0] == "rm" {
			removals.Add(1)
		}
		return dockerCommandResult{}
	})
	m := newTerminalSessionManager(terminalSessionManagerConfig{LeaseMinSec: 1, LeaseDefaultSec: 60, LeaseMaxSec: 600})
	op := testSnapshotOperation()
	if _, err := m.Execute(context.Background(), terminalExecRequest{SessionID: op.SessionID, Command: "seed", CreateIfMissing: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.HoldSource(op.SessionID, op.ID, op.Generation); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []int64{0, 1, 2} {
		_, err := m.Execute(context.Background(), terminalExecRequest{SessionID: op.SessionID, Command: "must not run", Generation: generation, CreateIfMissing: true})
		var execErr *terminalExecError
		if !errors.As(err, &execErr) || execErr.Code() != "session_state_conflict" {
			t.Fatalf("generation %d bypassed hold: %v", generation, err)
		}
	}
	if _, err := m.RenewLease(context.Background(), terminalLeaseRenewPayload{SessionID: op.SessionID, LeaseTTLSec: 60, Generation: 1}); err == nil {
		t.Fatal("held source renewed lease")
	}
	if _, err := m.ResolveResource(context.Background(), terminalResourceRequest{SessionID: op.SessionID, FilePath: "/data", Generation: 1}); err == nil {
		t.Fatal("held source allowed file access")
	}
	m.mu.Lock()
	session := m.sessions[op.SessionID]
	session.leaseExpiresAt = time.Now().Add(-time.Second)
	proxy := session.proxyCtx
	m.mu.Unlock()
	if proxy.Err() == nil {
		t.Fatal("hold left established proxy requests open")
	}
	m.cleanupExpiredSessions()
	m.expireSessionLease(session)
	if m.ActiveSessionCount() != 1 {
		t.Fatal("held source stopped consuming capacity")
	}
	m.Close()
	if removals.Load() != 0 {
		t.Fatal("lease expiry or shutdown deleted unique source")
	}
}

func TestSnapshotSourceDrainFailureRetainsBackend(t *testing.T) {
	started := make(chan struct{})
	var removals atomic.Int32
	stubDockerConcurrency(t, func(ctx context.Context, args ...string) dockerCommandResult {
		if args[0] == "rm" {
			removals.Add(1)
		}
		if args[0] == "exec" && args[len(args)-1] == "block" {
			close(started)
			<-ctx.Done()
			return dockerCommandResult{Err: ctx.Err()}
		}
		return dockerCommandResult{}
	})
	m := newTerminalSessionManager(terminalSessionManagerConfig{})
	defer m.Close()
	op := testSnapshotOperation()
	if _, err := m.Execute(context.Background(), terminalExecRequest{SessionID: op.SessionID, Command: "seed", CreateIfMissing: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.Execute(ctx, terminalExecRequest{SessionID: op.SessionID, Command: "block"})
		done <- err
	}()
	<-started
	if err := m.HoldSource(op.SessionID, op.ID, op.Generation); err != nil {
		t.Fatal(err)
	}
	drainCtx, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := m.DrainPersistence(drainCtx, op.SessionID, op.ID, op.Generation); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("drain did not wait for accepted command", err)
	}
	stop()
	cancel()
	<-done
	if err := m.DrainPersistence(context.Background(), op.SessionID, op.ID, op.Generation); err == nil {
		t.Fatal("failed command was treated as a successful drain")
	}
	if removals.Load() != 0 || m.ActiveSessionCount() != 1 {
		t.Fatal("drain failure discarded source")
	}
}

func TestSnapshotJournalActivationRequiresGenerationAndExistingBackend(t *testing.T) {
	var executions atomic.Int32
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		if args[0] == "exec" {
			executions.Add(1)
		}
		return dockerCommandResult{}
	})
	m := newTerminalSessionManager(terminalSessionManagerConfig{})
	defer m.Close()
	j := testSnapshotJournal(t)
	op := testSnapshotOperation()
	if _, err := m.Execute(context.Background(), terminalExecRequest{SessionID: op.SessionID, Command: "seed", CreateIfMissing: true}); err != nil {
		t.Fatal(err)
	}
	freezeTestSnapshot(t, j, op)
	for _, phase := range []string{"rolling_back", "rolled_back"} {
		if err := j.Update(op.ID, func(o *snapshotOperation) error { o.Phase = phase; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.InstallSnapshotJournal(j); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []int64{0, 2} {
		if _, err := m.Execute(context.Background(), terminalExecRequest{SessionID: op.SessionID, Command: "stale", Generation: generation}); err == nil {
			t.Fatal("stale generation executed")
		}
	}
	if _, err := m.Execute(context.Background(), terminalExecRequest{SessionID: op.SessionID, Command: "active", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 2 {
		t.Fatal("unexpected executions", executions.Load())
	}
	m.mu.Lock()
	session := m.sessions[op.SessionID]
	delete(m.sessions, op.SessionID)
	m.stopSessionLeaseTimerLocked(session)
	m.mu.Unlock()
	if _, err := m.Execute(context.Background(), terminalExecRequest{SessionID: op.SessionID, Command: "blank", Generation: 1, CreateIfMissing: true}); err == nil {
		t.Fatal("persistent command created a blank replacement")
	}
}

func TestSnapshotRecoveryHintPreservesMissingJournalResource(t *testing.T) {
	op := testSnapshotOperation()
	name := terminalSessionResourceName(op.SessionID)
	var removals atomic.Int32
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "ps":
			return dockerCommandResult{Stdout: name + "\n"}
		case "rm":
			removals.Add(1)
		}
		return dockerCommandResult{}
	})
	m := newTerminalSessionManager(terminalSessionManagerConfig{})
	defer m.Close()
	hint := &registryv1.SandboxPersistenceRecoveryCandidate{ScopedSessionId: op.SessionID, OperationId: op.ID, Generation: op.Generation, Role: registryv1.SandboxPersistenceRecoveryCandidate_SOURCE}
	if err := m.ProtectPersistenceRecovery([]*registryv1.SandboxPersistenceRecoveryCandidate{hint}); err != nil {
		t.Fatal(err)
	}
	m.Recover(context.Background(), nil)
	if removals.Load() != 0 || m.ActiveSessionCount() != 1 {
		t.Fatal("ordinary reconnect deleted or stopped accounting for lifecycle source")
	}
	if err := recoverPersistenceSessionsFn(context.Background(), nil, []*registryv1.SandboxPersistenceRecoveryCandidate{hint}); err == nil {
		t.Fatal("unconfigured worker accepted lifecycle recovery")
	}
}

func TestSnapshotCompletedActivationHintProtectsOrphanWithoutStoppingLease(t *testing.T) {
	op := testSnapshotOperation()
	op.Kind = "adopt"
	op.Phase = "active"
	op.StagingBytes = 0
	j := testSnapshotJournal(t)
	if err := j.Claim(op); err != nil {
		t.Fatal(err)
	}
	name := terminalSessionResourceName(op.SessionID)
	var removals atomic.Int32
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		if args[0] == "ps" {
			return dockerCommandResult{Stdout: name}
		}
		if args[0] == "rm" {
			removals.Add(1)
		}
		return dockerCommandResult{}
	})
	m := newTerminalSessionManager(terminalSessionManagerConfig{})
	defer m.Close()
	if _, err := m.Execute(context.Background(), terminalExecRequest{SessionID: op.SessionID, Command: "seed", CreateIfMissing: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.InstallSnapshotJournal(j); err != nil {
		t.Fatal(err)
	}
	hint := &registryv1.SandboxPersistenceRecoveryCandidate{ScopedSessionId: op.SessionID, OperationId: op.ID, Generation: 1, Role: registryv1.SandboxPersistenceRecoveryCandidate_TARGET, SnapshotCommitted: true}
	if err := m.ProtectPersistenceRecovery([]*registryv1.SandboxPersistenceRecoveryCandidate{hint}); err != nil {
		t.Fatal(err)
	}
	m.cleanupOrphanContainers(context.Background(), nil)
	if removals.Load() != 0 {
		t.Fatal("activation/Finish race deleted ready owner")
	}
	m.mu.Lock()
	held := m.persistenceHeldLocked(op.SessionID)
	active := m.persistenceFences[op.SessionID].active
	m.mu.Unlock()
	if held || !active {
		t.Fatal("completed activation lost execution or hard-lease handling")
	}
	if _, err := m.Execute(context.Background(), terminalExecRequest{SessionID: op.SessionID, Command: "active", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(m.persistenceFences[op.SessionID].containerID, "bb") {
		t.Fatal("journal lost immutable backend identity")
	}
}

func TestSnapshotHeldReservationPreventsOrdinarySessionOverbooking(t *testing.T) {
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult { return dockerCommandResult{} })
	m := newTerminalSessionManager(terminalSessionManagerConfig{MaxActiveSessions: 1})
	defer m.Close()
	op := testSnapshotOperation()
	if err := m.ProtectPersistenceRecovery([]*registryv1.SandboxPersistenceRecoveryCandidate{{ScopedSessionId: op.SessionID, OperationId: op.ID, Generation: op.Generation, Role: registryv1.SandboxPersistenceRecoveryCandidate_TARGET}}); err != nil {
		t.Fatal(err)
	}
	_, err := m.Execute(context.Background(), terminalExecRequest{Command: "new", CreateIfMissing: true})
	var execErr *terminalExecError
	if !errors.As(err, &execErr) || execErr.Code() != terminalExecCodeSessionCapacityExceeded {
		t.Fatalf("held reservation did not participate in local admission: %v", err)
	}
}

func TestSnapshotRecoveryRejectsDifferentBackendWithSameSessionName(t *testing.T) {
	op := testSnapshotOperation()
	op.Kind = "adopt"
	op.Phase = "active"
	op.StagingBytes = 0
	j := testSnapshotJournal(t)
	if err := j.Claim(op); err != nil {
		t.Fatal(err)
	}
	labels, _ := json.Marshal(map[string]string{terminalExecSessionLabelKey: terminalSessionIDHash(op.SessionID), terminalExecSchemaLabelKey: terminalExecSchemaVersion})
	var removals atomic.Int32
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		if args[0] == "rm" {
			removals.Add(1)
		}
		if args[0] == "ps" {
			return dockerCommandResult{Stdout: terminalSessionResourceName(op.SessionID)}
		}
		if args[0] == "inspect" && args[2] == "{{.Id}}" {
			return dockerCommandResult{Stdout: strings.Repeat("d", 64)}
		}
		return dockerCommandResult{Stdout: string(labels) + "\trunning"}
	})
	m := newTerminalSessionManager(terminalSessionManagerConfig{})
	defer m.Close()
	if err := m.InstallSnapshotJournal(j); err != nil {
		t.Fatal(err)
	}
	status, retry := m.recoverOneAttempt(context.Background(), op.SessionID, terminalSessionResourceName(op.SessionID), time.Now().Add(time.Hour))
	results := m.Recover(context.Background(), []*registryv1.TerminalSessionRecoveryCandidate{{SessionId: op.SessionID, Generation: 1, LeaseExpiresUnixMs: time.Now().Add(time.Hour).UnixMilli()}})
	if len(results) != 1 || results[0].Status != registryv1.TerminalSessionRecoveryResult_INVALID {
		t.Fatal("invalid backend was adopted")
	}
	if status != registryv1.TerminalSessionRecoveryResult_INVALID || retry || removals.Load() != 0 {
		t.Fatalf("recovery adopted or deleted an unowned same-name backend: %v retry=%v removals=%v", status, retry, removals.Load())
	}
}

func TestSnapshotPersistentExpiryStopsExecutionAndRetainsRootFS(t *testing.T) {
	op := testSnapshotOperation()
	op.Kind = "adopt"
	op.Phase = "active"
	op.StagingBytes = 0
	j := testSnapshotJournal(t)
	if err := j.Claim(op); err != nil {
		t.Fatal(err)
	}
	var stops, removals atomic.Int32
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		if args[0] == "stop" {
			if args[len(args)-1] != op.ContainerID {
				t.Error("expiry used a reusable container name")
			}
			stops.Add(1)
		}
		if args[0] == "rm" {
			removals.Add(1)
		}
		return dockerCommandResult{}
	})
	m := newTerminalSessionManager(terminalSessionManagerConfig{})
	defer m.Close()
	if _, err := m.Execute(context.Background(), terminalExecRequest{SessionID: op.SessionID, Command: "seed", CreateIfMissing: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.InstallSnapshotJournal(j); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.sessions[op.SessionID].leaseExpiresAt = time.Now().Add(-time.Second)
	m.mu.Unlock()
	m.cleanupExpiredSessions()
	if stops.Load() != 1 || removals.Load() != 0 {
		t.Fatalf("persistent lease expiry destroyed data or left execution running: stops=%d rm=%d", stops.Load(), removals.Load())
	}
	if m.ActiveSessionCount() != 1 {
		t.Fatal("retained rootfs silently released its reservation")
	}
	m.forceRemoveContainer(terminalSessionResourceName(op.SessionID))
	if removals.Load() != 0 {
		t.Fatal("ordinary name cleanup bypassed persistence ownership")
	}
}
