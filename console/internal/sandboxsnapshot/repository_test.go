package sandboxsnapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
)

func testRepository(t *testing.T) (*Repository, *persistence.DB, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "snapshot.db")
	db, err := persistence.Open(context.Background(), persistence.Options{Path: file, HashKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := NewRepository(db.SQL)
	if err = r.RegisterRunning(context.Background(), "owner", "session", "worker-a", testSpec()); err != nil {
		t.Fatal(err)
	}
	return r, db, file
}
func testSpec() RestoreSpec {
	return RestoreSpec{Runtime: "boxlite", RuntimeVersion: "0.10.6", Platform: "linux/arm64", Format: "boxlite-archive-v1", Image: "python@sha256:" + strings.Repeat("a", 64), MemoryMiB: 512, CPUs: 1}
}
func request(kind Kind, key string) BeginRequest {
	return BeginRequest{OwnerID: "owner", SessionID: "session", Kind: kind, IdempotencyKey: key, Parameters: json.RawMessage(`{"lease_ttl_sec":300}`), DeadlineUnixMS: time.Now().Add(time.Hour).UnixMilli()}
}
func begin(t *testing.T, r *Repository, kind Kind, key string) Operation {
	t.Helper()
	op, err := r.Begin(context.Background(), request(kind, key))
	if err != nil {
		t.Fatal(err)
	}
	return op
}
func testUpload(t *testing.T, id string) objectstore.SnapshotUpload {
	t.Helper()
	s, err := objectstore.NewSnapshotStore(objectstore.SnapshotConfig{Config: objectstore.Config{Endpoint: "https://store.example.com", Region: "test", BucketName: "snapshots", AccessKey: "ak", SecretKey: "sk"}, Prefix: "snapshots", MaxBytes: 10 << 30, PartSize: 5 << 20, PresignTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("snapshot"))
	ref, err := s.Object("owner", "session", id, 8, hex.EncodeToString(hash[:]))
	if err != nil {
		t.Fatal(err)
	}
	return objectstore.SnapshotUpload{Object: ref, UploadID: "upload-" + id, PartSize: 5 << 20}
}
func verified(t *testing.T, r *Repository, op Operation, id string) {
	t.Helper()
	ctx := context.Background()
	upload := testUpload(t, id)
	if err := r.ReserveUpload(ctx, op, upload, 100<<30); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordPart(ctx, op, id, objectstore.SnapshotPart{Number: 1, ETag: "etag", Size: 8}); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkVerified(ctx, op, id, Manifest{Version: 1, Archive: upload.Object, RestoreSpec: testSpec()}, ""); err != nil {
		t.Fatal(err)
	}
}
func suspend(t *testing.T, r *Repository, key, id string) Operation {
	t.Helper()
	op := begin(t, r, Suspend, key)
	verified(t, r, op, id)
	if err := r.CommitSuspend(context.Background(), op, id, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	return op
}
func resume(t *testing.T, r *Repository, key, worker string) Operation {
	t.Helper()
	op := begin(t, r, Resume, key)
	if err := r.SetRestoreTarget(context.Background(), op, worker); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitRestore(context.Background(), op, worker); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	return op
}

func TestVerifiedSnapshotCommitAndCrossWorkerOwnership(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	op := begin(t, r, Suspend, "suspend")
	upload := testUpload(t, "snap-a")
	if err := r.ReserveUpload(ctx, op, upload, 100); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitSuspend(ctx, op, "snap-a", time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("unverified commit: %v", err)
	}
	before, _ := r.Session(ctx, "owner", "session")
	if before.LatestSnapshotID != "" || before.State != Snapshotting || before.WorkerID != "worker-a" {
		t.Fatalf("published unverified snapshot: %+v", before)
	}
	if err := r.MarkVerified(ctx, op, "snap-a", Manifest{Version: 1, Archive: upload.Object, RestoreSpec: testSpec()}, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitSuspend(ctx, op, "snap-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitSuspend(ctx, op, "snap-a", time.Hour); err != nil {
		t.Fatalf("response-loss commit retry: %v", err)
	}
	session, _ := r.Session(ctx, "owner", "session")
	if session.State != Suspended || session.WorkerID != "" || session.LatestSnapshotID != "snap-a" || session.OperationID != op.ID {
		t.Fatalf("bad suspended state: %+v", session)
	}
	if _, err := r.Begin(ctx, request(Resume, "too-early")); !errors.Is(err, ErrConflict) {
		t.Fatalf("source cleanup not acknowledged: %v", err)
	}
	if err := r.Finish(ctx, op); err != nil {
		t.Fatal(err)
	}
	restore := begin(t, r, Resume, "restore")
	if restore.Generation != op.Generation+1 {
		t.Fatal("generation not advanced")
	}
	if err := r.CommitRestore(ctx, restore, "worker-b"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unreserved restore: %v", err)
	}
	if err := r.SetRestoreTarget(ctx, restore, "worker-b"); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitRestore(ctx, restore, "worker-a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong target committed: %v", err)
	}
	if err := r.CommitRestore(ctx, restore, "worker-b"); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitSuspend(ctx, op, "snap-a", time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("old generation changed owner: %v", err)
	}
	if err := r.Finish(ctx, restore); err != nil {
		t.Fatal(err)
	}
	final, _ := r.Session(ctx, "owner", "session")
	if final.WorkerID != "worker-b" || final.Generation != 2 || final.OperationID != "" || final.LatestSnapshotID != "snap-a" {
		t.Fatalf("bad restored state: %+v", final)
	}
}

func TestConcurrentAdmissionAndIdempotencyAliases(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	ids := make(chan string, 12)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			op, err := r.Begin(ctx, request(Suspend, fmt.Sprintf("key-%d", i)))
			if err != nil {
				errs <- err
			} else {
				ids <- op.ID
			}
		}(i)
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var id string
	for value := range ids {
		if id != "" && id != value {
			t.Fatal("created concurrent operations")
		}
		id = value
	}
	op, err := r.Operation(ctx, "owner", "session", id)
	if err != nil {
		t.Fatal(err)
	}
	verified(t, r, op, "snap-one")
	if err = r.CommitSuspend(ctx, op, "snap-one", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err = r.Finish(ctx, op); err != nil {
		t.Fatal(err)
	}
	resume(t, r, "wake", "worker-b")
	// Every joined key remains attached to the original operation across cycles.
	for i := 0; i < 12; i++ {
		prior, err := r.Begin(ctx, request(Suspend, fmt.Sprintf("key-%d", i)))
		if err != nil || prior.ID != id {
			t.Fatalf("alias was forgotten: %s %v", prior.ID, err)
		}
	}
	changed := request(Suspend, "key-0")
	changed.Parameters = json.RawMessage(`{"lease_ttl_sec":600}`)
	if _, err = r.Begin(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different request accepted: %v", err)
	}
}

func TestRestartPreservesUploadingAndFailedOperations(t *testing.T) {
	r, db, file := testRepository(t)
	ctx := context.Background()
	op := begin(t, r, Suspend, "suspend")
	upload := testUpload(t, "snap-a")
	if err := r.ReserveUpload(ctx, op, upload, 100); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordPart(ctx, op, "snap-a", objectstore.SnapshotPart{Number: 1, ETag: "part-etag", Size: 8}); err != nil {
		t.Fatal(err)
	}
	if err := r.FailAndHold(ctx, op, "timeout", "worker status unknown"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := persistence.Open(ctx, persistence.Options{Path: file, HashKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r = NewRepository(reopened.SQL)
	ops, err := r.PendingOperations(ctx)
	if err != nil || len(ops) != 1 || ops[0].Status != "failed" {
		t.Fatalf("lost uncertain operation: %+v %v", ops, err)
	}
	parts, err := r.Parts(ctx, "owner", "session", "snap-a")
	if err != nil || len(parts) != 1 || parts[0].ETag != "part-etag" {
		t.Fatalf("lost multipart state: %+v %v", parts, err)
	}
	if _, err = r.Begin(ctx, request(Suspend, "retry-too-early")); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown source assumed running: %v", err)
	}
	if err = r.ResolveFailed(ctx, op, Suspended); !errors.Is(err, ErrConflict) {
		t.Fatalf("uncommitted snapshot assumed durable: %v", err)
	}
	if err = r.ResolveFailed(ctx, op, Running); err != nil {
		t.Fatal(err)
	}
	ops, err = r.PendingOperations(ctx)
	if err != nil || len(ops) != 0 {
		t.Fatalf("resolved failure still pending: %+v %v", ops, err)
	}
	if _, err = r.Begin(ctx, request(Suspend, "retry-after-reconcile")); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaCountsReservationsAndGCProtectsReferencedSnapshots(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	op := begin(t, r, Suspend, "first")
	if err := r.ReserveUpload(ctx, op, testUpload(t, "snap-a"), 7); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota not enforced: %v", err)
	}
	verified(t, r, op, "snap-a")
	if err := r.CommitSuspend(ctx, op, "snap-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, op); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(time.Hour)
	if err := r.TombstoneSnapshot(ctx, "owner", "session", "snap-a", cutoff); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleted latest snapshot: %v", err)
	}
	resume(t, r, "wake", "worker-b")
	next := begin(t, r, Suspend, "second")
	if err := r.ReserveUpload(ctx, next, testUpload(t, "snap-b"), 15); !errors.Is(err, ErrQuota) {
		t.Fatalf("old snapshot not counted: %v", err)
	}
	verified(t, r, next, "snap-b")
	if err := r.CommitSuspend(ctx, next, "snap-b", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := r.TombstoneSnapshot(ctx, "owner", "session", "snap-a", cutoff); !errors.Is(err, ErrConflict) {
		t.Fatalf("GC raced active operation: %v", err)
	}
	if err := r.Finish(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := r.TombstoneSnapshot(ctx, "owner", "session", "snap-a", cutoff); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkSnapshotDeleted(ctx, "owner", "session", "snap-a"); err != nil {
		t.Fatal(err)
	}
	if err := r.TombstoneSnapshot(ctx, "owner", "session", "snap-a", cutoff); err != nil {
		t.Fatalf("GC retry failed: %v", err)
	}
	if err := r.MarkSnapshotDeleted(ctx, "owner", "session", "snap-a"); err != nil {
		t.Fatalf("delete acknowledgement retry failed: %v", err)
	}
	kept, err := r.Snapshot(ctx, "owner", "session", "snap-b")
	if err != nil || kept.Status != "ready" {
		t.Fatalf("GC removed current snapshot: %+v %v", kept, err)
	}
}

func TestAccountScopeAndManifestBinding(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	op := begin(t, r, Suspend, "key")
	upload := testUpload(t, "snap-a")
	if err := r.ReserveUpload(ctx, op, upload, 100); err != nil {
		t.Fatal(err)
	}
	for _, lookup := range []func() error{
		func() error { _, err := r.Session(ctx, "other", "session"); return err },
		func() error { _, err := r.Operation(ctx, "other", "session", op.ID); return err },
		func() error { _, err := r.Snapshot(ctx, "other", "session", "snap-a"); return err },
		func() error { _, err := r.Parts(ctx, "other", "session", "snap-a"); return err },
	} {
		if err := lookup(); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-account lookup: %v", err)
		}
	}
	manifest := Manifest{Version: 1, Archive: upload.Object, RestoreSpec: testSpec()}
	manifest.Archive.SHA256 = strings.Repeat("b", 64)
	if err := r.MarkVerified(ctx, op, "snap-a", manifest, ""); err == nil {
		t.Fatal("accepted mismatched archive digest")
	}
	manifest.Archive = upload.Object
	manifest.RestoreSpec.Platform = "linux/amd64"
	if err := r.MarkVerified(ctx, op, "snap-a", manifest, ""); err == nil {
		t.Fatal("accepted wrong platform")
	}
	altered := op
	altered.OwnerID = "other"
	if err := r.FailAndHold(ctx, altered, "failure", "message"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-account state update: %v", err)
	}
}

func TestOperationParameterValidation(t *testing.T) {
	r, _, _ := testRepository(t)
	for _, bad := range []string{`{"x":1} garbage`, `{} {}`, `{"x":`, ``} {
		req := request(Suspend, "bad")
		req.Parameters = json.RawMessage(bad)
		if _, err := r.Begin(context.Background(), req); err == nil {
			t.Fatalf("accepted invalid JSON: %q", bad)
		}
	}
	a := request(Suspend, "canonical")
	a.Parameters = json.RawMessage(`{"b":2,"a":1}`)
	op, err := r.Begin(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	a.Parameters = json.RawMessage("{\n\"a\":1,\"b\":2}")
	same, err := r.Begin(context.Background(), a)
	if err != nil || same.ID != op.ID {
		t.Fatalf("canonical retry: %+v %v", same, err)
	}
}

func TestFailedActivationKeepsSnapshotAndRetentionForRetry(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	suspend(t, r, "sleep", "snap-a")
	before, _ := r.Session(ctx, "owner", "session")
	op := begin(t, r, Resume, "wake")
	if err := r.SetRestoreTarget(ctx, op, "worker-b"); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitRestore(ctx, op, "worker-b"); err != nil {
		t.Fatal(err)
	}
	if err := r.FailAndHold(ctx, op, "timeout", "activation uncertain"); err != nil {
		t.Fatal(err)
	}
	if err := r.ResolveFailed(ctx, op, Suspended); err != nil {
		t.Fatal(err)
	}
	after, _ := r.Session(ctx, "owner", "session")
	if after.WorkerID != "" || after.RetentionExpiresUnixMS != before.RetentionExpiresUnixMS || after.LatestSnapshotID != "snap-a" {
		t.Fatalf("lost retention or snapshot: %+v", after)
	}
	next := begin(t, r, Resume, "wake-again")
	if next.Generation <= op.Generation {
		t.Fatal("reused failed ownership generation")
	}
}

func TestExpiredSuspendedSessionCannotResume(t *testing.T) {
	r, _, _ := testRepository(t)
	suspend(t, r, "sleep", "snap-a")
	before, _ := r.Session(context.Background(), "owner", "session")
	r.now = func() time.Time { return time.UnixMilli(before.RetentionExpiresUnixMS + 1) }
	req := request(Resume, "expired")
	req.DeadlineUnixMS = r.now().Add(time.Hour).UnixMilli()
	if _, err := r.Begin(context.Background(), req); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired snapshot admitted: %v", err)
	}
}
