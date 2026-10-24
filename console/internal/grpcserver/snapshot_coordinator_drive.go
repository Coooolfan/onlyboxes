package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
)

func sameSnapshotSpec(a, b wire.RestoreSpec) bool {
	left, e1 := json.Marshal(a)
	right, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && string(left) == string(right)
}

func (c *SnapshotCoordinator) drive(ctx context.Context, op sandboxsnapshot.Operation) error {
	session, err := c.repository.Session(ctx, op.OwnerID, op.SessionID)
	if err != nil {
		return err
	}
	if session.OperationID != op.ID || session.Generation != op.Generation {
		return sandboxsnapshot.ErrConflict
	}
	var parameters snapshotParameters
	if err = json.Unmarshal(op.Parameters, &parameters); err != nil {
		return err
	}
	if op.Kind == sandboxsnapshot.Suspend {
		return c.suspend(ctx, op, session, parameters)
	}
	if op.Kind == sandboxsnapshot.Resume {
		return c.resume(ctx, op, session, parameters)
	}
	return sandboxsnapshot.ErrConflict
}

func (c *SnapshotCoordinator) suspend(ctx context.Context, op sandboxsnapshot.Operation, session sandboxsnapshot.Session, parameters snapshotParameters) error {
	if parameters.RetentionTTLSec <= 0 {
		return ErrSnapshotParameters
	}
	if session.State == sandboxsnapshot.Suspended {
		if time.Now().UnixMilli() >= op.DeadlineUnixMS {
			return context.DeadlineExceeded
		}
		result, err := c.call(ctx, op, op.SourceWorkerID, registryv1.SandboxPersistenceControl_FINALIZE_SUSPEND, wire.Control{SnapshotCommitted: true}, false)
		if err != nil {
			return err
		}
		if result.State != "finalized" {
			return errSnapshotInProgress
		}
		return c.repository.Finish(ctx, op)
	}
	if session.State != sandboxsnapshot.Snapshotting {
		return sandboxsnapshot.ErrConflict
	}
	if time.Now().UnixMilli() >= op.DeadlineUnixMS {
		return context.DeadlineExceeded
	}
	prepared, err := c.call(ctx, op, op.SourceWorkerID, registryv1.SandboxPersistenceControl_PREPARE_SNAPSHOT, wire.Control{Spec: &session.Spec, MaxBytes: c.config.MaxBytes, MaxExpandedBytes: c.config.MaxExpandedBytes, DrainTimeoutSec: int(c.config.DrainTimeout / time.Second)}, false)
	if err != nil {
		return err
	}
	if prepared.State != "frozen" {
		return errSnapshotInProgress
	}
	if prepared.Archive == nil || prepared.Archive.Validate() != nil || prepared.Archive.SnapshotID != op.ID || prepared.Archive.Format != session.Spec.Format || prepared.Spec == nil || !sameSnapshotSpec(*prepared.Spec, session.Spec) {
		return objectstore.ErrSnapshotIntegrity
	}
	if prepared.Archive.Size > c.config.MaxBytes || prepared.Archive.ExpandedBytes > c.config.MaxExpandedBytes {
		return sandboxsnapshot.ErrQuota
	}
	snapshot, err := c.repository.Snapshot(ctx, op.OwnerID, op.SessionID, op.ID)
	if errors.Is(err, sandboxsnapshot.ErrNotFound) {
		object, err := c.objects.Object(op.OwnerID, op.SessionID, op.ID, prepared.Archive.Size, prepared.Archive.SHA256)
		if err != nil {
			return err
		}
		upload, err := c.objects.BeginUpload(ctx, object)
		if err != nil {
			return err
		}
		upload.ExpandedBytes = prepared.Archive.ExpandedBytes
		if err = c.repository.ReserveUpload(ctx, op, upload, c.config.AccountQuotaBytes); err != nil {
			// Abort failure leaves a non-completed multipart for reconciliation /
			// bucket incomplete-upload lifecycle; it never permits source cleanup.
			abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.ReconcileTimeout)
			_ = c.objects.AbortSnapshotUpload(abortCtx, upload)
			cancel()
			return err
		}
	} else if err != nil {
		return err
	}
	snapshot, err = c.repository.Snapshot(ctx, op.OwnerID, op.SessionID, op.ID)
	if err != nil {
		return err
	}
	if snapshot.Upload.Object.Size != prepared.Archive.Size || snapshot.Upload.Object.SHA256 != prepared.Archive.SHA256 || snapshot.Upload.ExpandedBytes != prepared.Archive.ExpandedBytes {
		return objectstore.ErrSnapshotIntegrity
	}
	if snapshot.Status == "uploading" {
		if err = c.upload(ctx, op, snapshot); err != nil {
			return err
		}
	}
	verifier := snapshotWorkerVerifier{coordinator: c, operation: op, worker: op.SourceWorkerID}
	publisher, err := sandboxsnapshot.NewPublisher(c.repository, snapshotRemoteObjects{coordinator: c, verifier: verifier}, verifier)
	if err != nil {
		return err
	}
	if _, err = publisher.Publish(ctx, op, op.ID, time.Duration(parameters.RetentionTTLSec)*time.Second); err != nil {
		return err
	}
	if err = c.service.RefreshSnapshotRoute(ctx, op.OwnerID, op.SessionID); err != nil {
		return err
	}
	// Re-enter the committed cleanup phase without exporting or uploading again.
	session.State = sandboxsnapshot.Suspended
	return c.suspend(ctx, op, session, parameters)
}

func (c *SnapshotCoordinator) upload(ctx context.Context, op sandboxsnapshot.Operation, snapshot sandboxsnapshot.Snapshot) error {
	parts, err := c.repository.Parts(ctx, op.OwnerID, op.SessionID, snapshot.ID)
	if err != nil {
		return err
	}
	completed := make(map[int32]bool, len(parts))
	for _, part := range parts {
		completed[part.Number] = true
	}
	count := (snapshot.Upload.Object.Size-1)/snapshot.Upload.PartSize + 1
	for number := int32(1); int64(number) <= count; {
		var batch []wire.UploadPart
		for int64(number) <= count && len(batch) < c.config.MultipartConcurrency {
			if !completed[number] {
				grant, err := c.objects.PresignPart(ctx, snapshot.Upload, number)
				if err != nil {
					return err
				}
				batch = append(batch, wire.UploadPart{Number: number, Grant: wire.TransferGrant(grant)})
			}
			number++
		}
		if len(batch) == 0 {
			continue
		}
		result, err := c.call(ctx, op, op.SourceWorkerID, registryv1.SandboxPersistenceControl_UPLOAD_SNAPSHOT, wire.Control{Parts: batch}, false)
		if err != nil {
			return err
		}
		if result.State != "frozen" || len(result.Parts) != len(batch) {
			return objectstore.ErrSnapshotIntegrity
		}
		allowed := make(map[int32]int64, len(batch))
		for _, part := range batch {
			allowed[part.Number] = part.Grant.Size
		}
		for _, part := range result.Parts {
			size, ok := allowed[part.Number]
			if !ok || part.Size != size || part.ETag == "" {
				return objectstore.ErrSnapshotIntegrity
			}
			delete(allowed, part.Number)
			if err = c.repository.RecordPart(ctx, op, snapshot.ID, objectstore.SnapshotPart{Number: part.Number, ETag: part.ETag, Size: part.Size}); err != nil {
				return err
			}
		}
	}
	return nil
}

type snapshotWorkerVerifier struct {
	coordinator *SnapshotCoordinator
	operation   sandboxsnapshot.Operation
	worker      string
}

func (v snapshotWorkerVerifier) VerifySnapshot(ctx context.Context, object objectstore.SnapshotObject) error {
	snapshot, err := v.coordinator.repository.Snapshot(ctx, v.operation.OwnerID, v.operation.SessionID, object.SnapshotID)
	if err != nil {
		return err
	}
	session, err := v.coordinator.repository.Session(ctx, v.operation.OwnerID, v.operation.SessionID)
	if err != nil {
		return err
	}
	grant, err := v.coordinator.objects.PresignSnapshotDownload(ctx, object, 0)
	if err != nil {
		return err
	}
	result, err := v.coordinator.call(ctx, v.operation, v.worker, registryv1.SandboxPersistenceControl_VERIFY_SNAPSHOT, wire.Control{Download: grantPointer(grant), Archive: &wire.Archive{SnapshotID: object.SnapshotID, Size: object.Size, SHA256: object.SHA256, Format: session.Spec.Format, ExpandedBytes: snapshot.Upload.ExpandedBytes}}, false)
	if err != nil {
		return err
	}
	if result.State != "verified" {
		return objectstore.ErrSnapshotIntegrity
	}
	return nil
}

func grantPointer(grant objectstore.TransferGrant) *wire.TransferGrant {
	value := wire.TransferGrant(grant)
	return &value
}

type snapshotRemoteObjects struct {
	coordinator *SnapshotCoordinator
	verifier    snapshotWorkerVerifier
}

func (o snapshotRemoteObjects) CompleteUpload(ctx context.Context, u objectstore.SnapshotUpload, parts []objectstore.SnapshotPart) (objectstore.SnapshotObject, error) {
	return o.coordinator.objects.CompleteUploadWithVerifier(ctx, u, parts, o.verifier.VerifySnapshot)
}
func (o snapshotRemoteObjects) PutSnapshotManifest(ctx context.Context, object objectstore.SnapshotObject, payload json.RawMessage) (string, error) {
	return o.coordinator.objects.PutSnapshotManifest(ctx, object, payload)
}

func (c *SnapshotCoordinator) resume(ctx context.Context, op sandboxsnapshot.Operation, session sandboxsnapshot.Session, parameters snapshotParameters) error {
	if parameters.LeaseTTLSec <= 0 {
		return ErrSnapshotParameters
	}
	if session.State == sandboxsnapshot.Running && op.Phase == "activating" {
		// A lost activation response or Console restart must finish the existing
		// instance, rather than constructing another one from the same snapshot.
		c.mu.Lock()
		reservation := c.reservations[op.ID]
		c.mu.Unlock()
		if reservation != nil && c.service.getSession(op.TargetWorkerID) != reservation.session {
			// The old connection cannot deliver controls. Confirm this exact
			// durable target on its replacement before retiring the old charge.
			observed, err := c.call(ctx, op, op.TargetWorkerID, registryv1.SandboxPersistenceControl_INSPECT, wire.Control{}, true)
			if err != nil {
				return err
			}
			if (observed.State != "ready" && observed.State != "active") || !validSnapshotInstance(observed, session.Spec) {
				return sandboxsnapshot.ErrConflict
			}
			c.releaseReservation(op.ID)
		}
		if time.Now().UnixMilli() >= op.DeadlineUnixMS {
			result, err := c.call(ctx, op, op.TargetWorkerID, registryv1.SandboxPersistenceControl_INSPECT, wire.Control{}, true)
			if err != nil {
				return err
			}
			if result.State != "active" {
				return context.DeadlineExceeded
			}
			if !validSnapshotInstance(result, session.Spec) {
				return objectstore.ErrSnapshotIntegrity
			}
		} else {
			result, err := c.call(ctx, op, op.TargetWorkerID, registryv1.SandboxPersistenceControl_ACTIVATE, wire.Control{}, false)
			if err != nil {
				return err
			}
			if result.State != "active" {
				return errSnapshotInProgress
			}
			if !validSnapshotInstance(result, session.Spec) {
				return objectstore.ErrSnapshotIntegrity
			}
		}
		if err := c.service.RefreshSnapshotRoute(ctx, op.OwnerID, op.SessionID); err != nil {
			return err
		}
		if err := c.repository.Finish(ctx, op); err != nil {
			return err
		}
		c.releaseReservation(op.ID)
		return nil
	}
	if session.State != sandboxsnapshot.Restoring {
		return sandboxsnapshot.ErrConflict
	}
	if time.Now().UnixMilli() >= op.DeadlineUnixMS {
		return context.DeadlineExceeded
	}
	snapshot, err := c.repository.Snapshot(ctx, op.OwnerID, op.SessionID, session.LatestSnapshotID)
	if err != nil {
		return err
	}
	if snapshot.Status != "ready" || snapshot.Manifest == nil || snapshot.Manifest.ExpandedBytes <= 0 || !sameSnapshotSpec(snapshot.Manifest.RestoreSpec, session.Spec) {
		return objectstore.ErrSnapshotIntegrity
	}
	object := snapshot.Manifest.Archive
	archive := wire.Archive{SnapshotID: object.SnapshotID, Size: object.Size, SHA256: object.SHA256, Format: session.Spec.Format, ExpandedBytes: snapshot.Manifest.ExpandedBytes}
	if archive.Validate() != nil || archive.Size > 1<<60 || archive.ExpandedBytes > 1<<60 {
		return objectstore.ErrSnapshotIntegrity
	}
	if op.TargetWorkerID == "" {
		previous, err := c.repository.Operation(ctx, op.OwnerID, op.SessionID, snapshot.OperationID)
		if err != nil {
			return err
		}
		reservation, err := c.service.ReserveSnapshotRestore(RestorePlacement{Spec: session.Spec, StagingBytes: archive.Size + archive.ExpandedBytes, PreferredWorkerID: previous.SourceWorkerID})
		if err != nil {
			return err
		}
		if err = c.repository.SetRestoreTarget(ctx, op, reservation.WorkerID()); err != nil {
			reservation.Release()
			return err
		}
		op.TargetWorkerID = reservation.WorkerID()
		c.mu.Lock()
		c.reservations[op.ID] = reservation
		c.mu.Unlock()
	}
	// An existing target may have outlived this Console process or connection.
	// Inspect its durable operation before sending more side effects. A missing
	// journal cannot authorize reconstructing a second instance under this claim.
	c.mu.Lock()
	reservation := c.reservations[op.ID]
	c.mu.Unlock()
	var ready wire.Result
	if reservation == nil || c.service.getSession(op.TargetWorkerID) != reservation.session {
		observed, err := c.call(ctx, op, op.TargetWorkerID, registryv1.SandboxPersistenceControl_INSPECT, wire.Control{}, false)
		if err != nil {
			return err
		}
		if observed.Spec == nil || !sameSnapshotSpec(*observed.Spec, session.Spec) || (observed.State != "prepared" && observed.State != "ready") {
			return sandboxsnapshot.ErrConflict
		}
		if reservation != nil {
			// The new connection confirmed the same journal; its heartbeat and
			// local admission now own accounting for these existing resources.
			c.releaseReservation(op.ID)
		}
		if observed.State == "ready" {
			ready = observed
		}
	}
	if ready.State != "ready" {
		prepared, err := c.call(ctx, op, op.TargetWorkerID, registryv1.SandboxPersistenceControl_PREPARE_RESTORE, wire.Control{Spec: &session.Spec, Archive: &archive, MaxBytes: archive.Size, MaxExpandedBytes: archive.ExpandedBytes, StagingBytes: archive.Size + archive.ExpandedBytes, LeaseTTLSec: parameters.LeaseTTLSec}, false)
		if err != nil {
			return err
		}
		if prepared.State != "prepared" && prepared.State != "ready" {
			return errSnapshotInProgress
		}
		grant, err := c.objects.PresignSnapshotDownload(ctx, object, 0)
		if err != nil {
			return err
		}
		ready, err = c.call(ctx, op, op.TargetWorkerID, registryv1.SandboxPersistenceControl_RESTORE, wire.Control{Download: grantPointer(grant), Archive: &archive}, false)
		if err != nil {
			return err
		}
		if ready.State != "ready" {
			return errSnapshotInProgress
		}
	}
	if !validSnapshotInstance(ready, session.Spec) {
		return objectstore.ErrSnapshotIntegrity
	}
	if err = c.repository.CommitRestore(ctx, op, op.TargetWorkerID, ready.LeaseExpiresUnixMS); err != nil {
		return err
	}
	if err = c.service.RefreshSnapshotRoute(ctx, op.OwnerID, op.SessionID); err != nil {
		return err
	}
	session.State = sandboxsnapshot.Running
	op.Phase = "activating"
	return c.resume(ctx, op, session, parameters)
}

func (c *SnapshotCoordinator) releaseReservation(id string) {
	c.mu.Lock()
	reservation := c.reservations[id]
	delete(c.reservations, id)
	c.mu.Unlock()
	if reservation != nil {
		reservation.Release()
	}
}

func (c *SnapshotCoordinator) reconcile(ctx context.Context, op sandboxsnapshot.Operation) error {
	current, err := c.repository.Operation(ctx, op.OwnerID, op.SessionID, op.ID)
	if err != nil {
		return err
	}
	op = current
	session, err := c.repository.Session(ctx, op.OwnerID, op.SessionID)
	if err != nil {
		return err
	}
	if session.OperationID != op.ID || session.Generation != op.Generation {
		return sandboxsnapshot.ErrConflict
	}
	confirmed := sandboxsnapshot.Suspended
	var lease int64
	if op.Kind == sandboxsnapshot.Suspend && session.State == sandboxsnapshot.Suspended {
		result, err := c.call(ctx, op, op.SourceWorkerID, registryv1.SandboxPersistenceControl_FINALIZE_SUSPEND, wire.Control{SnapshotCommitted: true}, true)
		if err != nil {
			return err
		}
		if result.State != "finalized" {
			return errSnapshotInProgress
		}
	} else if op.Kind == sandboxsnapshot.Suspend {
		result, err := c.call(ctx, op, op.SourceWorkerID, registryv1.SandboxPersistenceControl_CANCEL, wire.Control{Rollback: true, Spec: &session.Spec}, true)
		if err != nil {
			return err
		}
		if result.State != "running" || !validSnapshotInstance(result, session.Spec) {
			return errSnapshotInProgress
		}
		confirmed = sandboxsnapshot.Running
		lease = result.LeaseExpiresUnixMS
	} else if op.Kind == sandboxsnapshot.Resume && op.TargetWorkerID != "" {
		if session.State == sandboxsnapshot.Running {
			observed, err := c.call(ctx, op, op.TargetWorkerID, registryv1.SandboxPersistenceControl_INSPECT, wire.Control{}, true)
			if err != nil {
				return err
			}
			if observed.State == "active" {
				if observed.Spec == nil || !sameSnapshotSpec(*observed.Spec, session.Spec) || observed.InstanceID == "" {
					return objectstore.ErrSnapshotIntegrity
				}
				route, err := c.repository.ExecutionRoute(ctx, session.ScopedSessionID)
				if err != nil && !errors.Is(err, sandboxsnapshot.ErrNotFound) {
					return err
				}
				if err == nil && route.WorkerID == op.TargetWorkerID && route.LeaseExpiresUnixMS > time.Now().UnixMilli() && observed.LeaseExpiresUnixMS > time.Now().UnixMilli() {
					if err = c.repository.ReconcileActivation(ctx, op); err != nil {
						return err
					}
					return nil
				}
				// A worker observation cannot revive an expired published lease.
				// Cancel this target before restoring suspended ownership below.
			}
		}
		result, err := c.call(ctx, op, op.TargetWorkerID, registryv1.SandboxPersistenceControl_CANCEL, wire.Control{Delete: session.State == sandboxsnapshot.Running}, true)
		if err != nil {
			return err
		}
		if result.State != "canceled" {
			return errSnapshotInProgress
		}
	}
	if err = c.repository.ResolveFailed(ctx, op, confirmed, lease); err != nil {
		return err
	}
	if err = c.service.RefreshSnapshotRoute(ctx, op.OwnerID, op.SessionID); err != nil {
		return err
	}
	c.releaseReservation(op.ID)
	return nil
}

func validSnapshotInstance(result wire.Result, spec wire.RestoreSpec) bool {
	return result.Spec != nil && sameSnapshotSpec(*result.Spec, spec) && result.LeaseExpiresUnixMS > time.Now().UnixMilli() && result.InstanceID != ""
}
