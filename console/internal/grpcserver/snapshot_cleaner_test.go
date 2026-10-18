package grpcserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
)

type cleanerObjects struct {
	objects *coordinatorObjects
	mu      sync.Mutex
	reject  bool
	purges  int
}

func TestSnapshotCleanerDeletesNativeOnlySessionWithoutObjectStore(t *testing.T) {
	ctx := context.Background()
	svc, source, _, _ := setupSnapshotRouteTest(t)
	startCoordinatorActor(t, source, nil)
	if ok, err := svc.DeleteManagedSession(ctx, "owner", "session", time.Now()); err != nil || !ok {
		t.Fatal("logical deletion failed", ok, err)
	}
	if required, err := svc.snapshotRepository.RequiresStore(ctx); err != nil || required {
		t.Fatal("native-only cleanup unexpectedly requires storage", required, err)
	}
	cleaner, err := NewSnapshotCleaner(svc, nil, SnapshotCleanerConfig{Interval: time.Second, Grace: time.Hour, RetryInterval: time.Millisecond, CallTimeout: time.Second, MaxJobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cleaner.repository.PendingDeletions(ctx, time.Now().Add(time.Second), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatal("native cleanup was not queued", err)
	}
	cleaner.deleteSession(ctx, jobs[0])
	jobs, err = cleaner.repository.PendingDeletions(ctx, time.Now().Add(time.Second), 10)
	if err != nil || len(jobs) != 0 {
		t.Fatal("native cleanup needs an object store", jobs, err)
	}
}

func (o *cleanerObjects) AbortAndConfirmSnapshotUpload(context.Context, objectstore.SnapshotUpload) error {
	return nil
}
func (o *cleanerObjects) PurgeSnapshot(_ context.Context, object objectstore.SnapshotObject, _ string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.reject {
		return errors.New("provider error https://private.invalid?secret=signed-grant")
	}
	o.purges++
	o.objects.mu.Lock()
	defer o.objects.mu.Unlock()
	delete(o.objects.archives, object.SnapshotID)
	delete(o.objects.manifests, object.SnapshotID)
	return nil
}
func TestSnapshotCleanerRequiresNativeProofAndRetriesStorageAndOfflineCaches(t *testing.T) {
	ctx := context.Background()
	svc, a, b, _ := setupSnapshotRouteTest(t)
	startCoordinatorActor(t, a, []byte("snapshot"))
	target := startCoordinatorActor(t, b, nil)
	objects := newCoordinatorObjects(t)
	coordinator := startCoordinatorTest(t, svc, objects, coordinatorTestConfig())
	_, sleep, err := coordinator.Begin(ctx, "owner", "session", sandboxsnapshot.Suspend, "suspend", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorOperation(t, coordinator, sleep, "completed")
	a.lastHeartbeatUnixMs.Store(time.Now().Add(-time.Minute).UnixMilli())
	_, wake, err := coordinator.Begin(ctx, "owner", "session", sandboxsnapshot.Resume, "resume", 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorOperation(t, coordinator, wake, "completed")
	if ok, err := svc.DeleteManagedSession(ctx, "owner", "session", time.Now()); err != nil || !ok {
		t.Fatal("logical deletion failed", ok, err)
	}
	fake := &cleanerObjects{objects: objects}
	cleaner, err := NewSnapshotCleaner(svc, fake, SnapshotCleanerConfig{Interval: time.Second, Grace: 24 * time.Hour, RetryInterval: time.Millisecond, CallTimeout: time.Second, MaxJobs: 2})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cleaner.repository.PendingDeletions(ctx, time.Now().Add(time.Second), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatal("deletion was not durable", err)
	}
	job := jobs[0]
	target.mu.Lock()
	target.refuseCancel = true
	target.mu.Unlock()
	cleaner.deleteSession(ctx, job)
	stable, err := cleaner.repository.Session(ctx, "owner", "session")
	if err != nil {
		t.Fatal(err)
	}
	if stable.LatestSnapshotID != sleep.ID {
		t.Fatal("unconfirmed target cancellation released the only snapshot")
	}
	snapshot, err := cleaner.repository.Snapshot(ctx, "owner", "session", sleep.ID)
	if err != nil || snapshot.Status != "ready" {
		t.Fatal("unconfirmed native removal started object deletion", snapshot.Status, err)
	}
	fake.mu.Lock()
	purges := fake.purges
	fake.mu.Unlock()
	if purges != 0 {
		t.Fatal("storage cleanup preceded native proof")
	}
	target.mu.Lock()
	target.refuseCancel = false
	target.mu.Unlock()
	fake.mu.Lock()
	fake.reject = true
	fake.mu.Unlock()
	cleaner.deleteSession(ctx, job)
	stable, err = cleaner.repository.Session(ctx, "owner", "session")
	if err != nil {
		t.Fatal(err)
	}
	if stable.LatestSnapshotID != "" || stable.WorkerID != "" {
		t.Fatal("confirmed native removal did not clear restore references")
	}
	snapshot, err = cleaner.repository.Snapshot(ctx, "owner", "session", sleep.ID)
	if err != nil || snapshot.Status != "deleting" {
		t.Fatal("storage failure was not retryable", snapshot.Status, err)
	}
	fake.mu.Lock()
	fake.reject = false
	fake.mu.Unlock()
	time.Sleep(3 * time.Millisecond)
	cleaner.deleteSession(ctx, job)
	snapshot, err = cleaner.repository.Snapshot(ctx, "owner", "session", sleep.ID)
	if err != nil || snapshot.Status != "deleted" {
		t.Fatal("object cleanup did not release quota state", snapshot.Status, err)
	}
	// The stopped source owns only an old cache; its offline state must not keep
	// the shared archive alive after the active target has confirmed deletion.
	remaining, err := cleaner.repository.PendingDeletions(ctx, time.Now().Add(time.Second), 10)
	if err != nil || len(remaining) != 1 {
		t.Fatal("offline cache cleanup was silently discarded", err)
	}
	recovery, err := cleaner.repository.WorkerRecovery(ctx, "worker-a")
	if err != nil || len(recovery.Held) != 1 || recovery.Held[0].Generation != job.FenceGeneration() || recovery.Held[0].OperationID != job.ID() {
		t.Fatal("reconnect lost cache purge ownership", err)
	}
	a.lastHeartbeatUnixMs.Store(time.Now().UnixMilli())
	cleaner.deleteSession(ctx, job)
	remaining, err = cleaner.repository.PendingDeletions(ctx, time.Now().Add(time.Second), 10)
	if err != nil || len(remaining) != 0 {
		t.Fatal("cleanup did not finish after source reconnected", err)
	}
	if required, err := cleaner.repository.RequiresStore(ctx); err != nil || required {
		t.Fatal("finished cleanup retained remote storage work", required, err)
	}
}

func TestSnapshotCleanerRetiresExpiredSuspendedSessionWithoutDroppingHeldSource(t *testing.T) {
	ctx := context.Background()
	svc, a, _, _ := setupSnapshotRouteTest(t)
	source := startCoordinatorActor(t, a, []byte("snapshot"))
	objects := newCoordinatorObjects(t)
	coordinator := startCoordinatorTest(t, svc, objects, coordinatorTestConfig())
	_, op, err := coordinator.Begin(ctx, "owner", "session", sandboxsnapshot.Suspend, "suspend", time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorOperation(t, coordinator, op, "completed")
	if _, err := svc.store.Persistence().SQL.ExecContext(ctx, `UPDATE sandbox_persistent_sessions SET retention_expires_unix_ms=?`, time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	cleaner, err := NewSnapshotCleaner(svc, &cleanerObjects{objects: objects}, SnapshotCleanerConfig{Interval: time.Second, Grace: 24 * time.Hour, RetryInterval: 10 * time.Millisecond, CallTimeout: time.Second, MaxJobs: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := cleaner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := cleaner.Stop(stop); err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshots, err := cleaner.repository.Snapshot(ctx, "owner", "session", op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if snapshots.Status == "deleted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired session did not enter durable cleanup", snapshots.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := svc.GetManagedSession(ctx, "owner", "session", time.Now()); !errors.Is(err, sandboxsnapshot.ErrNotFound) {
		t.Fatal("expired stable session remained public", err)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.counts[registryv1.SandboxPersistenceControl_CANCEL] == 0 {
		t.Fatal("expired source cache was not purged")
	}
}
