package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

func setupSnapshotRouteTest(t *testing.T) (*RegistryService, *activeSession, *activeSession, string) {
	t.Helper()
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	svc.proxyEnabled = true
	hello := snapshotTestHello("worker-a")
	hello.Capabilities = append(hello.Capabilities, &registryv1.CapabilityDeclaration{Name: taskCapabilityTerminalResource}, &registryv1.CapabilityDeclaration{Name: taskCapabilityTerminalLeaseRenew})
	a := addSnapshotTestWorker(t, svc, hello)
	b := addSnapshotTestWorker(t, svc, snapshotTestHello("worker-b"))
	a.proxyEndpoint, b.proxyEndpoint = "10.0.0.2:8091", "10.0.0.3:8091"
	a.routeTokenKey, b.routeTokenKey = []byte(strings.Repeat("a", 32)), []byte(strings.Repeat("b", 32))
	scopedID := scopeTerminalSessionID("owner", "session")
	svc.bindTerminalSessionRoute(scopedID, "worker-a", time.Now())
	if confirmed, err := svc.commitConfirmedTerminalSessionRoute(scopedID, "worker-a", 0, time.Now().Add(time.Hour).UnixMilli(), time.Now()); err != nil || !confirmed {
		t.Fatalf("seed route: %t %v", confirmed, err)
	}
	if err := svc.AdoptSnapshotSession(context.Background(), "owner", "session", "worker-a", snapshotTestSpec()); err != nil {
		t.Fatal(err)
	}
	return svc, a, b, scopedID
}

func beginRouteSnapshotTest(t *testing.T, svc *RegistryService, kind sandboxsnapshot.Kind, key string) sandboxsnapshot.Operation {
	t.Helper()
	op, err := svc.BeginSnapshotOperation(context.Background(), sandboxsnapshot.BeginRequest{OwnerID: "owner", SessionID: "session", Kind: kind, IdempotencyKey: key, Parameters: json.RawMessage(`{}`), DeadlineUnixMS: time.Now().Add(time.Hour).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func commitRouteSnapshotTest(t *testing.T, svc *RegistryService, op sandboxsnapshot.Operation) {
	t.Helper()
	store, err := objectstore.NewSnapshotStore(objectstore.SnapshotConfig{Config: objectstore.Config{Endpoint: "https://storage.example.com", Region: "test", BucketName: "snapshots", AccessKey: "ak", SecretKey: "sk"}, Prefix: "snapshots", MaxBytes: 10 << 30, PartSize: 5 << 20, PresignTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Object("owner", "session", "snapshot", 8, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := svc.snapshotRepository
	if err := r.ReserveUpload(ctx, op, objectstore.SnapshotUpload{Object: object, UploadID: "upload", PartSize: 5 << 20, ExpandedBytes: 8}, 1<<30); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkVerified(ctx, op, "snapshot", sandboxsnapshot.Manifest{Version: 1, ExpandedBytes: 8, Archive: object, RestoreSpec: snapshotTestSpec()}, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitSuspend(ctx, op, "snapshot", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, op); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotLifecycleGatesCommandsRenewalAndPreview(t *testing.T) {
	svc, source, target, scopedID := setupSnapshotRouteTest(t)
	ctx := context.Background()
	assertClosed := func() {
		t.Helper()
		_, err := svc.SubmitTask(ctx, SubmitTaskRequest{OwnerID: "owner", Capability: taskCapabilityTerminalExec, InputJSON: json.RawMessage(`{"session_id":"session","command":"true","create_if_missing":true}`), Mode: TaskModeSync, Timeout: time.Second})
		if !isCommandErrorCode(err, sessionStateConflictCode) {
			t.Fatalf("task admission bypassed lifecycle gate: %v", err)
		}
		for _, capability := range []string{taskCapabilityTerminalExec, taskCapabilityTerminalResource, taskCapabilityTerminalLeaseRenew} {
			payload, _ := json.Marshal(map[string]any{"session_id": scopedID, "command": "true", "file_path": "/data", "create_if_missing": true, "lease_ttl_sec": 300})
			_, err := svc.dispatchCommand(ctx, capability, payload, time.Second, dispatchOptions{ownerID: "owner"})
			if !isCommandErrorCode(err, sessionStateConflictCode) {
				t.Fatalf("%s bypassed lifecycle gate: %v", capability, err)
			}
		}
		if _, err := svc.RenewTerminalSessionLease(ctx, "owner", "session", 300); !isCommandErrorCode(err, sessionStateConflictCode) {
			t.Fatalf("renewal bypassed lifecycle gate: %v", err)
		}
		if _, err := svc.ResolveProxySession("owner", "session", time.Now()); err == nil {
			t.Fatal("preview opened before activation")
		}
		if _, err := svc.AuthorizeProxyRoute(ctx, "worker-a", scopedID, 8080, time.Now().Add(time.Hour), time.Now()); err == nil {
			t.Fatal("cached preview bypassed lifecycle gate")
		}
		if len(source.commandOutbound) != 0 || len(target.commandOutbound) != 0 {
			t.Fatal("closed session dispatched a command")
		}
	}
	suspend := beginRouteSnapshotTest(t, svc, sandboxsnapshot.Suspend, "suspend")
	assertClosed()
	if candidates, err := svc.beginTerminalSessionRecoveryWithError("worker-a", time.Now()); err != nil || len(candidates) != 0 {
		t.Fatalf("ordinary reconnect would restart frozen source: %v %v", candidates, err)
	}
	commitRouteSnapshotTest(t, svc, suspend)
	// Deliberately retain the old in-memory route: the database gate must win.
	assertClosed()
	if err := svc.RefreshSnapshotRoute(ctx, "owner", "session"); err != nil {
		t.Fatal(err)
	}
	resume := beginRouteSnapshotTest(t, svc, sandboxsnapshot.Resume, "resume")
	assertClosed()
	if err := svc.snapshotRepository.SetRestoreTarget(ctx, resume, "worker-b"); err != nil {
		t.Fatal(err)
	}
	if err := svc.snapshotRepository.CommitRestore(ctx, resume, "worker-b", time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := svc.RefreshSnapshotRoute(ctx, "owner", "session"); err != nil {
		t.Fatal(err)
	}
	assertClosed()
	if err := svc.snapshotRepository.Finish(ctx, resume); err != nil {
		t.Fatal(err)
	}
	proxy, err := svc.ResolveProxySession("owner", "session", time.Now())
	if err != nil || proxy.WorkerID != "worker-b" || proxy.ScopedSessionID != scopedID {
		t.Fatalf("preview did not follow restored target: %+v %v", proxy, err)
	}
	finished := make(chan error, 1)
	payload, _ := json.Marshal(map[string]any{"session_id": scopedID, "command": "true", "create_if_missing": true})
	go func() {
		outcome, err := svc.dispatchCommand(ctx, taskCapabilityTerminalExec, payload, time.Second, dispatchOptions{ownerID: "owner"})
		if err == nil && !isCommandErrorCode(outcome.err, terminalSessionUnavailableCode) {
			err = errors.New("missing persistent runtime was not reported as unavailable")
		}
		finished <- err
	}()
	dispatch := receiveCommandDispatch(t, target)
	var bound struct {
		Generation      int64 `json:"generation"`
		CreateIfMissing bool  `json:"create_if_missing"`
	}
	if err := json.Unmarshal(dispatch.PayloadJson, &bound); err != nil || bound.Generation != resume.Generation || bound.CreateIfMissing {
		t.Fatalf("restore command lacks execution fence: %+v %v", bound, err)
	}
	// The backend disappearing cannot cause a blank sandbox or erase the stable
	// snapshot. The ordinary session-not-found result becomes unavailability.
	target.resolvePending(&registryv1.CommandResult{CommandId: dispatch.CommandId, Error: &registryv1.CommandError{Code: terminalSessionNotFoundCode, Message: "missing runtime"}})
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("command did not finish")
	}
	session, err := svc.snapshotRepository.Session(ctx, "owner", "session")
	if err != nil || session.LatestSnapshotID != "snapshot" {
		t.Fatalf("lost stable recovery point: %+v %v", session, err)
	}
	if _, err := svc.dispatchCommand(ctx, taskCapabilityTerminalExec, payload, time.Second, dispatchOptions{ownerID: "owner"}); !isCommandErrorCode(err, terminalSessionUnavailableCode) {
		t.Fatalf("missing runtime retried as fresh session: %v", err)
	}
}

func TestSnapshotLateCommandCannotExtendAnotherGeneration(t *testing.T) {
	svc, source, _, scopedID := setupSnapshotRouteTest(t)
	ctx := context.Background()
	payload, _ := json.Marshal(map[string]any{"session_id": scopedID, "command": "true"})
	finished := make(chan error, 1)
	go func() {
		_, err := svc.dispatchCommand(ctx, taskCapabilityTerminalExec, payload, 10*time.Second, dispatchOptions{ownerID: "owner"})
		finished <- err
	}()
	dispatch := receiveCommandDispatch(t, source)
	suspend := beginRouteSnapshotTest(t, svc, sandboxsnapshot.Suspend, "suspend")
	commitRouteSnapshotTest(t, svc, suspend)
	resume := beginRouteSnapshotTest(t, svc, sandboxsnapshot.Resume, "resume")
	if err := svc.snapshotRepository.SetRestoreTarget(ctx, resume, "worker-a"); err != nil {
		t.Fatal(err)
	}
	lease := time.Now().Add(time.Hour).UnixMilli()
	if err := svc.snapshotRepository.CommitRestore(ctx, resume, "worker-a", lease); err != nil {
		t.Fatal(err)
	}
	if err := svc.RefreshSnapshotRoute(ctx, "owner", "session"); err != nil {
		t.Fatal(err)
	}
	if err := svc.snapshotRepository.Finish(ctx, resume); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]any{"session_id": scopedID, "lease_expires_unix_ms": time.Now().Add(4 * time.Hour).UnixMilli()})
	source.resolvePending(&registryv1.CommandResult{CommandId: dispatch.CommandId, PayloadJson: result})
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late result did not finish")
	}
	route, err := svc.snapshotRepository.ExecutionRoute(ctx, scopedID)
	if err != nil || route.LeaseExpiresUnixMS != lease {
		t.Fatalf("old generation extended new route: %+v %v", route, err)
	}
}

func TestSnapshotUnavailableDatabaseCannotOpenExecution(t *testing.T) {
	svc, source, _, scopedID := setupSnapshotRouteTest(t)
	if err := svc.store.Persistence().Close(); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"session_id": scopedID, "command": "true"})
	_, err := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, payload, time.Second, dispatchOptions{ownerID: "owner"})
	if err == nil || errors.Is(err, ErrNoCapabilityWorker) || len(source.commandOutbound) != 0 {
		t.Fatalf("unreadable lifecycle gate admitted execution: %v", err)
	}
}

func TestSnapshotLateRenewalCannotClaimANewLease(t *testing.T) {
	svc, source, _, scopedID := setupSnapshotRouteTest(t)
	ctx := context.Background()
	before, err := svc.snapshotRepository.ExecutionRoute(ctx, scopedID)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := svc.RenewTerminalSessionLease(ctx, "owner", "session", 300)
		finished <- err
	}()
	dispatch := receiveCommandDispatch(t, source)
	beginRouteSnapshotTest(t, svc, sandboxsnapshot.Suspend, "suspend")
	result, _ := json.Marshal(map[string]any{"session_id": scopedID, "lease_expires_unix_ms": time.Now().Add(4 * time.Hour).UnixMilli()})
	source.resolvePending(&registryv1.CommandResult{CommandId: dispatch.CommandId, PayloadJson: result})
	select {
	case err := <-finished:
		if !isCommandErrorCode(err, sessionStateConflictCode) {
			t.Fatalf("late renewal claimed success during lifecycle transition: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late renewal did not finish")
	}
	after, err := svc.snapshotRepository.ExecutionRoute(ctx, scopedID)
	if err != nil || before.LeaseExpiresUnixMS != after.LeaseExpiresUnixMS {
		t.Fatalf("late renewal extended frozen source: %+v %v", after, err)
	}
}
