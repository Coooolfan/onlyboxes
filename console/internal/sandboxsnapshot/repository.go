package sandboxsnapshot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
)

type Repository struct {
	db  *sql.DB
	now func() time.Time
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db, now: time.Now} }
func (r *Repository) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type rowScanner interface{ Scan(...any) error }
type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const sessionColumns = `owner_id,session_id,state,generation,restore_spec_json,worker_id,latest_snapshot_id,operation_id,retention_expires_unix_ms,created_at_unix_ms,updated_at_unix_ms`

func scanSession(row rowScanner) (Session, error) {
	var s Session
	var spec string
	err := row.Scan(&s.OwnerID, &s.SessionID, &s.State, &s.Generation, &spec, &s.WorkerID, &s.LatestSnapshotID, &s.OperationID, &s.RetentionExpiresUnixMS, &s.CreatedAtUnixMS, &s.UpdatedAtUnixMS)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	if err != nil {
		return s, err
	}
	err = json.Unmarshal([]byte(spec), &s.Spec)
	return s, err
}
func sessionIn(ctx context.Context, q queryer, owner, id string) (Session, error) {
	return scanSession(q.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sandbox_persistent_sessions WHERE owner_id=? AND session_id=?`, owner, id))
}
func (r *Repository) Session(ctx context.Context, owner, id string) (Session, error) {
	return sessionIn(ctx, r.db, owner, id)
}

// RegisterRunning records the already-existing execution instance. No worker
// command is dispatched by this method, and an existing record is never replaced.
func (r *Repository) RegisterRunning(ctx context.Context, owner, id, worker string, spec RestoreSpec) error {
	if owner == "" || id == "" || worker == "" {
		return errors.New("running snapshot session requires owner, session and worker")
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	payload, _ := json.Marshal(spec)
	now := r.now().UnixMilli()
	_, err := r.db.ExecContext(ctx, `INSERT INTO sandbox_persistent_sessions (`+sessionColumns+`) VALUES (?,?,'running',1,?,?,'','',0,?,?)`, owner, id, string(payload), worker, now, now)
	return err
}

const operationColumns = `operation_id,owner_id,session_id,kind,idempotency_key,request_sha256,parameters_json,generation,status,phase,source_worker_id,target_worker_id,error_code,error_message,deadline_unix_ms,created_at_unix_ms,updated_at_unix_ms`

func scanOperation(row rowScanner) (Operation, error) {
	var o Operation
	var parameters string
	err := row.Scan(&o.ID, &o.OwnerID, &o.SessionID, &o.Kind, &o.IdempotencyKey, &o.RequestSHA256, &parameters, &o.Generation, &o.Status, &o.Phase, &o.SourceWorkerID, &o.TargetWorkerID, &o.ErrorCode, &o.ErrorMessage, &o.DeadlineUnixMS, &o.CreatedAtUnixMS, &o.UpdatedAtUnixMS)
	o.Parameters = json.RawMessage(parameters)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return o, err
}
func operationIn(ctx context.Context, q queryer, owner, session, id string) (Operation, error) {
	return scanOperation(q.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM sandbox_persistence_operations WHERE owner_id=? AND session_id=? AND operation_id=?`, owner, session, id))
}
func (r *Repository) Operation(ctx context.Context, owner, session, id string) (Operation, error) {
	return operationIn(ctx, r.db, owner, session, id)
}

// Begin persists operation ownership before any backend side effect. Concurrent
// equivalent requests join the same operation, even with different retry keys.
func (r *Repository) Begin(ctx context.Context, request BeginRequest) (Operation, error) {
	now := r.now().UnixMilli()
	hash, parameters, err := request.validate(now)
	if err != nil {
		return Operation{}, err
	}
	var result Operation
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		prior, err := scanOperation(tx.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM sandbox_persistence_operations WHERE operation_id=(SELECT operation_id FROM sandbox_persistence_requests WHERE owner_id=? AND session_id=? AND kind=? AND idempotency_key=?)`, request.OwnerID, request.SessionID, request.Kind, request.IdempotencyKey))
		if err == nil {
			if prior.RequestSHA256 != hash {
				return ErrIdempotencyConflict
			}
			result = prior
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		session, err := sessionIn(ctx, tx, request.OwnerID, request.SessionID)
		if err != nil {
			return err
		}
		if session.OperationID != "" {
			current, err := operationIn(ctx, tx, session.OwnerID, session.SessionID, session.OperationID)
			if err != nil {
				return err
			}
			if current.Kind == request.Kind && current.RequestSHA256 == hash && (current.Status == "pending" || current.Status == "running") {
				result = current
				return rememberRequest(ctx, tx, request, hash, current.ID)
			}
			return ErrConflict
		}
		from, to, phase := Running, Snapshotting, "draining"
		generation := session.Generation
		if request.Kind == Resume {
			from, to, phase = Suspended, Restoring, "scheduling"
			generation++
			if session.RetentionExpiresUnixMS > 0 && session.RetentionExpiresUnixMS <= now {
				return ErrConflict
			}
			if session.LatestSnapshotID == "" {
				return ErrConflict
			}
		}
		if session.State != from {
			if (request.Kind == Suspend && session.State == Suspended) || (request.Kind == Resume && session.State == Running) {
				return ErrAlreadyInState
			}
			return ErrConflict
		}
		result = Operation{ID: uuid.NewString(), OwnerID: session.OwnerID, SessionID: session.SessionID, Kind: request.Kind, IdempotencyKey: request.IdempotencyKey, RequestSHA256: hash, Parameters: parameters, Generation: generation, Status: "pending", Phase: phase, SourceWorkerID: session.WorkerID, DeadlineUnixMS: request.DeadlineUnixMS, CreatedAtUnixMS: now, UpdatedAtUnixMS: now}
		_, err = tx.ExecContext(ctx, `INSERT INTO sandbox_persistence_operations (`+operationColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,'','','',?,?,?)`, result.ID, result.OwnerID, result.SessionID, result.Kind, result.IdempotencyKey, result.RequestSHA256, string(result.Parameters), result.Generation, result.Status, result.Phase, result.SourceWorkerID, result.DeadlineUnixMS, now, now)
		if err != nil {
			return err
		}
		if err = rememberRequest(ctx, tx, request, hash, result.ID); err != nil {
			return err
		}
		changed, err := tx.ExecContext(ctx, `UPDATE sandbox_persistent_sessions SET state=?,generation=?,operation_id=?,updated_at_unix_ms=? WHERE owner_id=? AND session_id=? AND state=? AND generation=? AND operation_id=''`, to, generation, result.ID, now, session.OwnerID, session.SessionID, from, session.Generation)
		return requireChanged(changed, err)
	})
	return result, err
}

func rememberRequest(ctx context.Context, tx *sql.Tx, request BeginRequest, hash, id string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO sandbox_persistence_requests(owner_id,session_id,kind,idempotency_key,request_sha256,operation_id) VALUES (?,?,?,?,?,?)`, request.OwnerID, request.SessionID, request.Kind, request.IdempotencyKey, hash, id)
	return err
}

func requireChanged(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

func currentIn(ctx context.Context, tx *sql.Tx, op Operation) (Session, Operation, error) {
	current, err := operationIn(ctx, tx, op.OwnerID, op.SessionID, op.ID)
	if err != nil {
		return Session{}, current, err
	}
	session, err := sessionIn(ctx, tx, op.OwnerID, op.SessionID)
	if err != nil {
		return session, current, err
	}
	if session.OperationID != op.ID || session.Generation != op.Generation || current.Generation != op.Generation || (current.Status != "pending" && current.Status != "running") {
		return session, current, ErrConflict
	}
	return session, current, nil
}

func (r *Repository) SetRestoreTarget(ctx context.Context, op Operation, worker string) error {
	if op.Kind != Resume || worker == "" {
		return ErrConflict
	}
	return r.transaction(ctx, func(tx *sql.Tx) error {
		session, current, err := currentIn(ctx, tx, op)
		if err != nil {
			return err
		}
		if session.State != Restoring || current.Kind != Resume || (current.TargetWorkerID != "" && current.TargetWorkerID != worker) {
			return ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_persistence_operations SET target_worker_id=?,status='running',phase='downloading',updated_at_unix_ms=? WHERE operation_id=?`, worker, r.now().UnixMilli(), op.ID)
		return err
	})
}

// ReserveUpload accounts for all non-deleted snapshots, including in-flight
// uploads. The multipart ID and part plan are stable across Console restarts.
func (r *Repository) ReserveUpload(ctx context.Context, op Operation, upload objectstore.SnapshotUpload, quota int64) error {
	object := upload.Object
	if err := object.Validate(); err != nil {
		return err
	}
	if op.Kind != Suspend || object.OwnerID != op.OwnerID || object.SessionID != op.SessionID || object.SnapshotID == "" || object.Size <= 0 || quota <= 0 || upload.UploadID == "" || upload.PartSize < objectstore.MinSnapshotPartSize || upload.PartSize > objectstore.MaxSnapshotPartSize || (object.Size-1)/upload.PartSize+1 > objectstore.MaxSnapshotParts {
		return ErrConflict
	}
	encoded, err := json.Marshal(upload)
	if err != nil {
		return err
	}
	return r.transaction(ctx, func(tx *sql.Tx) error {
		session, current, err := currentIn(ctx, tx, op)
		if err != nil {
			return err
		}
		if session.State != Snapshotting || current.Kind != Suspend {
			return ErrConflict
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT upload_json FROM sandbox_snapshots WHERE operation_id=?`, op.ID).Scan(&existing)
		if err == nil {
			if existing == string(encoded) {
				return nil
			}
			return ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var used int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes),0) FROM sandbox_snapshots WHERE owner_id=? AND status!='deleted'`, op.OwnerID).Scan(&used); err != nil {
			return err
		}
		if object.Size > quota || used > quota-object.Size {
			return ErrQuota
		}
		now := r.now().UnixMilli()
		_, err = tx.ExecContext(ctx, `INSERT INTO sandbox_snapshots (snapshot_id,owner_id,session_id,operation_id,generation,status,size_bytes,upload_json,created_at_unix_ms,updated_at_unix_ms) VALUES (?,?,?,?,?,'uploading',?,?,?,?)`, object.SnapshotID, op.OwnerID, op.SessionID, op.ID, op.Generation, object.Size, string(encoded), now, now)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_persistence_operations SET status='running',phase='uploading',updated_at_unix_ms=? WHERE operation_id=?`, now, op.ID)
		return err
	})
}

const snapshotColumns = `snapshot_id,owner_id,session_id,operation_id,generation,status,upload_json,manifest_json,manifest_version_id,created_at_unix_ms,updated_at_unix_ms`

func scanSnapshot(row rowScanner) (Snapshot, error) {
	var s Snapshot
	var upload, manifest string
	err := row.Scan(&s.ID, &s.OwnerID, &s.SessionID, &s.OperationID, &s.Generation, &s.Status, &upload, &manifest, &s.ManifestVersionID, &s.CreatedAtUnixMS, &s.UpdatedAtUnixMS)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal([]byte(upload), &s.Upload); err != nil {
		return s, err
	}
	if manifest != "" {
		err = json.Unmarshal([]byte(manifest), &s.Manifest)
	}
	return s, err
}
func snapshotIn(ctx context.Context, q queryer, owner, session, id string) (Snapshot, error) {
	return scanSnapshot(q.QueryRowContext(ctx, `SELECT `+snapshotColumns+` FROM sandbox_snapshots WHERE owner_id=? AND session_id=? AND snapshot_id=?`, owner, session, id))
}
func (r *Repository) Snapshot(ctx context.Context, owner, session, id string) (Snapshot, error) {
	return snapshotIn(ctx, r.db, owner, session, id)
}

func (r *Repository) RecordPart(ctx context.Context, op Operation, snapshotID string, part objectstore.SnapshotPart) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		_, _, err := currentIn(ctx, tx, op)
		if err != nil {
			return err
		}
		snapshot, err := snapshotIn(ctx, tx, op.OwnerID, op.SessionID, snapshotID)
		if err != nil {
			return err
		}
		if snapshot.OperationID != op.ID || snapshot.Status != "uploading" {
			return ErrConflict
		}
		upload := snapshot.Upload
		count := (upload.Object.Size-1)/upload.PartSize + 1
		expected := upload.PartSize
		if int64(part.Number) == count {
			expected = upload.Object.Size - int64(part.Number-1)*upload.PartSize
		}
		if part.Number < 1 || int64(part.Number) > count || part.ETag == "" || part.Size != expected {
			return errors.New("part does not match reserved snapshot plan")
		}
		// The same part number can be retried while uploading; completion verifies the
		// persisted ETag against the object store before it makes the object immutable.
		_, err = tx.ExecContext(ctx, `INSERT INTO sandbox_snapshot_parts(snapshot_id,part_number,etag,size_bytes) VALUES (?,?,?,?) ON CONFLICT(snapshot_id,part_number) DO UPDATE SET etag=excluded.etag,size_bytes=excluded.size_bytes`, snapshotID, part.Number, part.ETag, part.Size)
		return err
	})
}

func (r *Repository) Parts(ctx context.Context, owner, session, id string) ([]objectstore.SnapshotPart, error) {
	if _, err := r.Snapshot(ctx, owner, session, id); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT part_number,etag,size_bytes FROM sandbox_snapshot_parts WHERE snapshot_id=? ORDER BY part_number`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parts []objectstore.SnapshotPart
	for rows.Next() {
		var p objectstore.SnapshotPart
		if err = rows.Scan(&p.Number, &p.ETag, &p.Size); err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, rows.Err()
}

// MarkVerified is called only after remote object readback and immutable manifest
// storage succeed. It never accepts a caller-created download URL or ETag proof.
func sameSpec(a, b RestoreSpec) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func (r *Repository) MarkVerified(ctx context.Context, op Operation, snapshotID string, manifest Manifest, manifestVersionID string) error {
	if manifest.Version != 1 {
		return errors.New("unsupported snapshot manifest version")
	}
	if err := manifest.RestoreSpec.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(encoded) > objectstore.MaxSnapshotManifestBytes {
		return errors.New("snapshot manifest exceeds its limit")
	}
	return r.transaction(ctx, func(tx *sql.Tx) error {
		session, _, err := currentIn(ctx, tx, op)
		if err != nil {
			return err
		}
		if session.State != Snapshotting || !sameSpec(session.Spec, manifest.RestoreSpec) {
			return ErrConflict
		}
		snapshot, err := snapshotIn(ctx, tx, op.OwnerID, op.SessionID, snapshotID)
		if err != nil {
			return err
		}
		if snapshot.OperationID != op.ID || snapshot.Status != "uploading" {
			if snapshot.Status == "verified" && snapshot.Manifest != nil && reflect.DeepEqual(*snapshot.Manifest, manifest) && snapshot.ManifestVersionID == manifestVersionID {
				return nil
			}
			return ErrConflict
		}
		expected := snapshot.Upload.Object
		expected.VersionID = manifest.Archive.VersionID
		if !reflect.DeepEqual(expected, manifest.Archive) {
			return errors.New("verified manifest differs from reserved archive")
		}
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_snapshots SET status='verified',manifest_json=?,manifest_version_id=?,updated_at_unix_ms=? WHERE snapshot_id=?`, string(encoded), manifestVersionID, r.now().UnixMilli(), snapshotID)
		return err
	})
}

// CommitSuspend publishes only a verified snapshot. Source cleanup remains a
// separately acknowledged step; until then the operation retains session control.
func (r *Repository) CommitSuspend(ctx context.Context, op Operation, snapshotID string, retention time.Duration) error {
	if op.Kind != Suspend || retention <= 0 {
		return ErrConflict
	}
	return r.transaction(ctx, func(tx *sql.Tx) error {
		session, _, err := currentIn(ctx, tx, op)
		if err != nil {
			return err
		}
		if session.State == Suspended && session.LatestSnapshotID == snapshotID {
			return nil
		}
		if session.State != Snapshotting {
			return ErrConflict
		}
		snapshot, err := snapshotIn(ctx, tx, op.OwnerID, op.SessionID, snapshotID)
		if err != nil {
			return err
		}
		if snapshot.Status != "verified" || snapshot.OperationID != op.ID || snapshot.Generation != op.Generation {
			return ErrConflict
		}
		now := r.now()
		expires := now.Add(retention).UnixMilli()
		if expires <= now.UnixMilli() {
			return errors.New("snapshot retention overflows timestamp")
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sandbox_snapshots SET status='ready',updated_at_unix_ms=? WHERE snapshot_id=?`, now.UnixMilli(), snapshotID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sandbox_persistent_sessions SET state='suspended',worker_id='',latest_snapshot_id=?,retention_expires_unix_ms=?,updated_at_unix_ms=? WHERE owner_id=? AND session_id=?`, snapshotID, expires, now.UnixMilli(), op.OwnerID, op.SessionID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_persistence_operations SET phase='cleaning',status='running',updated_at_unix_ms=? WHERE operation_id=?`, now.UnixMilli(), op.ID)
		return err
	})
}

// CommitRestore records the selected new owner after backend readiness. A later
// activation acknowledgement is required before the operation is finished.
func (r *Repository) CommitRestore(ctx context.Context, op Operation, worker string) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		session, current, err := currentIn(ctx, tx, op)
		if err != nil {
			return err
		}
		if current.Kind != Resume || worker == "" || current.TargetWorkerID != worker {
			return ErrConflict
		}
		if session.State == Running && session.WorkerID == worker && current.Phase == "activating" {
			return nil
		}
		if session.State != Restoring {
			return ErrConflict
		}
		snapshot, err := snapshotIn(ctx, tx, op.OwnerID, op.SessionID, session.LatestSnapshotID)
		if err != nil {
			return err
		}
		if snapshot.Status != "ready" {
			return ErrConflict
		}
		now := r.now().UnixMilli()
		if _, err = tx.ExecContext(ctx, `UPDATE sandbox_persistent_sessions SET state='running',worker_id=?,updated_at_unix_ms=? WHERE owner_id=? AND session_id=?`, worker, now, op.OwnerID, op.SessionID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_persistence_operations SET phase='activating',status='running',updated_at_unix_ms=? WHERE operation_id=?`, now, op.ID)
		return err
	})
}

func (r *Repository) Finish(ctx context.Context, op Operation) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		prior, err := operationIn(ctx, tx, op.OwnerID, op.SessionID, op.ID)
		if err != nil {
			return err
		}
		if prior.Status == "succeeded" && prior.Generation == op.Generation {
			return nil
		}
		session, current, err := currentIn(ctx, tx, op)
		if err != nil {
			return err
		}
		if !((current.Kind == Suspend && session.State == Suspended && current.Phase == "cleaning") || (current.Kind == Resume && session.State == Running && current.Phase == "activating")) {
			return ErrConflict
		}
		now := r.now().UnixMilli()
		if _, err = tx.ExecContext(ctx, `UPDATE sandbox_persistence_operations SET status='succeeded',phase='completed',updated_at_unix_ms=? WHERE operation_id=?`, now, op.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_persistent_sessions SET operation_id='',retention_expires_unix_ms=CASE WHEN state='running' THEN 0 ELSE retention_expires_unix_ms END,updated_at_unix_ms=? WHERE owner_id=? AND session_id=?`, now, op.OwnerID, op.SessionID)
		return err
	})
}

// FailAndHold records uncertainty without declaring the source running or the
// target stopped. The coordinator must reconcile backend state before retrying.
func (r *Repository) FailAndHold(ctx context.Context, op Operation, code, message string) error {
	if code == "" {
		return errors.New("persistence failure code is required")
	}
	return r.transaction(ctx, func(tx *sql.Tx) error {
		_, _, err := currentIn(ctx, tx, op)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_persistence_operations SET status='failed',phase='reconciling',error_code=?,error_message=?,updated_at_unix_ms=? WHERE operation_id=?`, code, message, r.now().UnixMilli(), op.ID)
		return err
	})
}

// ResolveFailed requires a confirmed backend state: running after a failed
// suspend, suspended only after a failed restore target can no longer execute.
func (r *Repository) ResolveFailed(ctx context.Context, op Operation, confirmed State) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		session, err := sessionIn(ctx, tx, op.OwnerID, op.SessionID)
		if err != nil {
			return err
		}
		current, err := operationIn(ctx, tx, op.OwnerID, op.SessionID, op.ID)
		if err != nil {
			return err
		}
		if current.Status == "failed" && current.Phase == "resolved" && current.Generation == op.Generation {
			return nil
		}
		if session.OperationID != op.ID || session.Generation != op.Generation || current.Generation != op.Generation || current.Status != "failed" {
			return ErrConflict
		}
		if !((current.Kind == Suspend && session.State == Snapshotting && confirmed == Running) || (current.Kind == Suspend && session.State == Suspended && confirmed == Suspended) || (current.Kind == Resume && (session.State == Restoring || session.State == Running) && confirmed == Suspended)) {
			return ErrConflict
		}
		worker := ""
		if confirmed == Running {
			worker = current.SourceWorkerID
		}
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_persistent_sessions SET state=?,worker_id=?,operation_id='',updated_at_unix_ms=? WHERE owner_id=? AND session_id=?`, confirmed, worker, r.now().UnixMilli(), op.OwnerID, op.SessionID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_persistence_operations SET phase='resolved',updated_at_unix_ms=? WHERE operation_id=?`, r.now().UnixMilli(), op.ID)
		return err
	})
}

// PendingOperations includes failures whose backend state remains uncertain.
func (r *Repository) PendingOperations(ctx context.Context) ([]Operation, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+operationColumns+` FROM sandbox_persistence_operations WHERE status IN ('pending','running') OR (status='failed' AND phase='reconciling') ORDER BY created_at_unix_ms,operation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ops []Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

// TombstoneSnapshot atomically excludes new restore references before object GC.
func (r *Repository) TombstoneSnapshot(ctx context.Context, owner, session, id string, olderThan time.Time) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		current, err := snapshotIn(ctx, tx, owner, session, id)
		if err != nil {
			return err
		}
		if current.Status == "deleting" || current.Status == "deleted" {
			return nil
		}
		changed, err := tx.ExecContext(ctx, `UPDATE sandbox_snapshots SET status='deleting',updated_at_unix_ms=? WHERE owner_id=? AND session_id=? AND snapshot_id=? AND status IN ('uploading','verified','ready') AND updated_at_unix_ms<? AND NOT EXISTS (SELECT 1 FROM sandbox_persistent_sessions s WHERE s.latest_snapshot_id=sandbox_snapshots.snapshot_id OR (s.owner_id=sandbox_snapshots.owner_id AND s.session_id=sandbox_snapshots.session_id AND s.operation_id!=''))`, r.now().UnixMilli(), owner, session, id, olderThan.UnixMilli())
		return requireChanged(changed, err)
	})
}
func (r *Repository) MarkSnapshotDeleted(ctx context.Context, owner, session, id string) error {
	changed, err := r.db.ExecContext(ctx, `UPDATE sandbox_snapshots SET status='deleted',updated_at_unix_ms=? WHERE owner_id=? AND session_id=? AND snapshot_id=? AND status='deleting'`, r.now().UnixMilli(), owner, session, id)
	err = requireChanged(changed, err)
	if errors.Is(err, ErrConflict) {
		current, lookupErr := r.Snapshot(ctx, owner, session, id)
		if lookupErr != nil {
			return lookupErr
		}
		if current.Status == "deleted" {
			return nil
		}
	}
	return err
}

func (o Operation) String() string {
	return fmt.Sprintf("%s/%s generation=%d %s:%s", o.Kind, o.ID, o.Generation, o.Status, o.Phase)
}
