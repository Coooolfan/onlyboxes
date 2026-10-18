package sandboxsnapshot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
)

func seedExecutionRouteAndPreview(t *testing.T, r *Repository) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.db.ExecContext(ctx, `INSERT INTO accounts(account_id,username,username_key,password_hash,hash_algo,is_admin,created_at_unix_ms,updated_at_unix_ms) VALUES ('owner','owner','owner','hash','test',0,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.ExecContext(ctx, `INSERT INTO terminal_session_routes VALUES ('obx:owner:session','worker-a',?,?,?,?) ON CONFLICT(scoped_session_id) DO NOTHING`, time.Now().Add(time.Hour).UnixMilli(), 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.ExecContext(ctx, `INSERT INTO proxy_routes VALUES ('preview','owner','session','obx:owner:session','worker-a',8080,1,?)`, time.Now().Add(24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRoutePublicationIsAtomicAndPreservesPreview(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	seedExecutionRouteAndPreview(t, r)
	op := begin(t, r, Suspend, "suspend")
	verified(t, r, op, "snapshot")
	if _, err := r.db.ExecContext(ctx, `CREATE TRIGGER reject_suspend_route BEFORE DELETE ON terminal_session_routes BEGIN SELECT RAISE(ABORT,'injected delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitSuspend(ctx, op, "snapshot", time.Hour); err == nil {
		t.Fatal("suspend committed despite route deletion failure")
	}
	session, _ := r.Session(ctx, "owner", "session")
	snapshot, _ := r.Snapshot(ctx, "owner", "session", "snapshot")
	if session.State != Snapshotting || session.LatestSnapshotID != "" || snapshot.Status != "verified" {
		t.Fatalf("partial suspend commit: %+v %s", session, snapshot.Status)
	}
	if _, err := r.ExecutionRoute(ctx, session.ScopedSessionID); err != nil {
		t.Fatal("source route disappeared on rollback", err)
	}
	if _, err := r.db.ExecContext(ctx, `DROP TRIGGER reject_suspend_route`); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitSuspend(ctx, op, "snapshot", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecutionRoute(ctx, session.ScopedSessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("source route survived suspend: %v", err)
	}
	if _, err := r.db.ExecContext(ctx, `INSERT INTO terminal_session_routes VALUES ('obx:owner:session','worker-a',100000,1,1,1)`); err == nil {
		t.Fatal("stale source republished a suspended route")
	}
	if err := r.Finish(ctx, op); err != nil {
		t.Fatal(err)
	}
	resume := begin(t, r, Resume, "resume")
	if err := r.SetRestoreTarget(ctx, resume, "worker-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.ExecContext(ctx, `CREATE TRIGGER reject_restore_route BEFORE INSERT ON terminal_session_routes BEGIN SELECT RAISE(ABORT,'injected insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	lease := time.Now().Add(time.Hour).UnixMilli()
	if err := r.CommitRestore(ctx, resume, "worker-b", lease); err == nil {
		t.Fatal("restore committed despite route insertion failure")
	}
	session, _ = r.Session(ctx, "owner", "session")
	if session.State != Restoring || session.WorkerID != "" {
		t.Fatalf("partial restore commit: %+v", session)
	}
	if _, err := r.db.ExecContext(ctx, `DROP TRIGGER reject_restore_route`); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitRestore(ctx, resume, "worker-b", lease); err != nil {
		t.Fatal(err)
	}
	route, err := r.ExecutionRoute(ctx, session.ScopedSessionID)
	if err != nil || route.WorkerID != "worker-b" || route.LeaseExpiresUnixMS != lease {
		t.Fatalf("incorrect restored route: %+v %v", route, err)
	}
	var previewWorker string
	if err := r.db.QueryRowContext(ctx, `SELECT worker_id FROM proxy_routes WHERE route_key='preview'`).Scan(&previewWorker); err != nil || previewWorker != "worker-b" {
		t.Fatalf("preview was deleted or not rebound: %q %v", previewWorker, err)
	}
	if _, err := r.db.ExecContext(ctx, `UPDATE terminal_session_routes SET node_id='worker-a' WHERE scoped_session_id='obx:owner:session'`); err == nil {
		t.Fatal("stale worker replaced committed restore target")
	}
	if err := r.FailAndHold(ctx, resume, "timeout", "activation acknowledgement lost"); err != nil {
		t.Fatal(err)
	}
	if err := r.ResolveFailed(ctx, resume, Suspended, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecutionRoute(ctx, session.ScopedSessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed activation retained target route: %v", err)
	}
	if err := r.db.QueryRowContext(ctx, `SELECT worker_id FROM proxy_routes WHERE route_key='preview'`).Scan(&previewWorker); err != nil {
		t.Fatal("failed activation discarded stable preview", err)
	}
	resume2 := begin(t, r, Resume, "resume-again")
	if err := r.SetRestoreTarget(ctx, resume2, "worker-c"); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitRestore(ctx, resume2, "worker-c", lease); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, resume2); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitRestore(ctx, resume, "worker-b", lease); !errors.Is(err, ErrConflict) {
		t.Fatalf("previous generation republished route: %v", err)
	}
}

func TestAdoptRunningRequiresMatchingLiveRoute(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	if err := r.AdoptRunning(ctx, "owner", "missing", "worker", testSpec()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("adopted missing execution route: %v", err)
	}
	seedExecutionRouteAndPreview(t, r)
	if err := r.AdoptRunning(ctx, "owner", "session", "wrong-worker", testSpec()); !errors.Is(err, ErrConflict) {
		t.Fatalf("adopted a different worker's route: %v", err)
	}
	if err := r.AdoptRunning(ctx, "owner", "session", "worker-a", testSpec()); err != nil {
		t.Fatal("idempotent adoption", err)
	}
	spec := testSpec()
	spec.MemoryMiB++
	if err := r.AdoptRunning(ctx, "owner", "session", "worker-a", spec); !errors.Is(err, ErrConflict) {
		t.Fatalf("replaced immutable restore specification: %v", err)
	}
	if _, err := r.db.ExecContext(ctx, `INSERT INTO terminal_session_routes VALUES ('obx:owner:expired','worker-a',1,1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := r.AdoptRunning(ctx, "owner", "expired", "worker-a", testSpec()); !errors.Is(err, ErrConflict) {
		t.Fatalf("adopted expired route: %v", err)
	}
}

func TestActivationCannotOpenAnExpiredRoute(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	suspend(t, r, "suspend", "snapshot")
	op := begin(t, r, Resume, "resume")
	if err := r.SetRestoreTarget(ctx, op, "worker-b"); err != nil {
		t.Fatal(err)
	}
	lease := time.Now().Add(time.Hour).UnixMilli()
	if err := r.CommitRestore(ctx, op, "worker-b", lease); err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return time.UnixMilli(lease + 1) }
	if err := r.Finish(ctx, op); !errors.Is(err, ErrConflict) {
		t.Fatalf("opened execution after its lease expired: %v", err)
	}
	session, _ := r.Session(ctx, "owner", "session")
	if session.OperationID != op.ID {
		t.Fatal("expired activation cleared lifecycle gate")
	}
}

func TestSuspendCannotClaimAnExpiredSource(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	if _, err := r.db.ExecContext(ctx, `UPDATE terminal_session_routes SET lease_expires_unix_ms=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Begin(ctx, request(Suspend, "suspend")); !errors.Is(err, ErrConflict) {
		t.Fatalf("claimed expired source: %v", err)
	}
	session, err := r.Session(ctx, "owner", "session")
	if err != nil || session.State != Running || session.OperationID != "" {
		t.Fatalf("failed admission mutated lifecycle: %+v %v", session, err)
	}
}

func TestStartupRoutePruningPreservesSnapshotOperationAndPreview(t *testing.T) {
	r, db, path := testRepository(t)
	ctx := context.Background()
	seedExecutionRouteAndPreview(t, r)
	expires := time.Now().Add(time.Second)
	if _, err := r.db.ExecContext(ctx, `UPDATE terminal_session_routes SET lease_expires_unix_ms=?`, expires.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	op := begin(t, r, Suspend, "suspend")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(expires) + 10*time.Millisecond)
	reopened, err := persistence.Open(ctx, persistence.Options{Path: path, HashKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r = NewRepository(reopened.SQL)
	if _, err := r.ExecutionRoute(ctx, ScopeSessionID("owner", "session")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("startup retained expired runtime route: %v", err)
	}
	session, err := r.Session(ctx, "owner", "session")
	if err != nil || session.OperationID != op.ID || session.State != Snapshotting {
		t.Fatalf("startup lost durable lifecycle ownership: %+v %v", session, err)
	}
	var count int
	if err := reopened.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_routes WHERE route_key='preview'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("startup discarded stable preview: %d %v", count, err)
	}
}

func TestFailedSuspendRollbackRecreatesPrunedRouteAtomically(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	op := begin(t, r, Suspend, "suspend")
	if err := r.FailAndHold(ctx, op, "timeout", "uncertain source"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM terminal_session_routes`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.ExecContext(ctx, `CREATE TRIGGER reject_rollback_route BEFORE INSERT ON terminal_session_routes BEGIN SELECT RAISE(ABORT,'injected insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	lease := time.Now().Add(time.Hour).UnixMilli()
	if err := r.ResolveFailed(ctx, op, Running, lease); err == nil {
		t.Fatal("rollback opened execution without restoring the source route")
	}
	session, _ := r.Session(ctx, "owner", "session")
	if session.State != Snapshotting || session.OperationID != op.ID {
		t.Fatalf("failed route publication released ownership: %+v", session)
	}
	if _, err := r.db.ExecContext(ctx, `DROP TRIGGER reject_rollback_route`); err != nil {
		t.Fatal(err)
	}
	if err := r.ResolveFailed(ctx, op, Running, lease); err != nil {
		t.Fatal(err)
	}
	route, err := r.ExecutionRoute(ctx, ScopeSessionID("owner", "session"))
	if err != nil || route.WorkerID != "worker-a" || route.LeaseExpiresUnixMS != lease {
		t.Fatalf("rollback lost confirmed source lease: %+v %v", route, err)
	}
}

func TestReconcileActivationKeepsGateUntilSameGenerationFinishes(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	suspend(t, r, "suspend", "snapshot")
	op := begin(t, r, Resume, "resume")
	if err := r.SetRestoreTarget(ctx, op, "worker-b"); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitRestore(ctx, op, "worker-b", time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := r.FailAndHold(ctx, op, "timeout", "activation response lost"); err != nil {
		t.Fatal(err)
	}
	stale := op
	stale.Generation--
	if err := r.ReconcileActivation(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale generation reclaimed activation: %v", err)
	}
	if err := r.ReconcileActivation(ctx, op); err != nil {
		t.Fatal(err)
	}
	session, _ := r.Session(ctx, "owner", "session")
	if session.OperationID != op.ID || session.State != Running || session.WorkerID != "worker-b" {
		t.Fatalf("reconciliation opened gate or changed owner: %+v", session)
	}
	if err := r.Finish(ctx, op); err != nil {
		t.Fatal(err)
	}
	session, _ = r.Session(ctx, "owner", "session")
	if session.OperationID != "" {
		t.Fatal("activation acknowledgement did not release lifecycle gate")
	}
}
