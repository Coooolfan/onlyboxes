package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
)

var ErrPersistenceUnavailable = errors.New("sandbox persistence is unavailable")
var ErrSnapshotParameters = errors.New("invalid snapshot operation parameters")
var errSnapshotInProgress = errors.New("worker lifecycle action is still in progress")

type SnapshotObjects interface {
	Object(string, string, string, int64, string) (objectstore.SnapshotObject, error)
	BeginUpload(context.Context, objectstore.SnapshotObject) (objectstore.SnapshotUpload, error)
	PresignPart(context.Context, objectstore.SnapshotUpload, int32) (objectstore.TransferGrant, error)
	PresignSnapshotDownload(context.Context, objectstore.SnapshotObject, int64) (objectstore.TransferGrant, error)
	CompleteUploadWithVerifier(context.Context, objectstore.SnapshotUpload, []objectstore.SnapshotPart, func(context.Context, objectstore.SnapshotObject) error) (objectstore.SnapshotObject, error)
	PutSnapshotManifest(context.Context, objectstore.SnapshotObject, json.RawMessage) (string, error)
	AbortSnapshotUpload(context.Context, objectstore.SnapshotUpload) error
}

type SnapshotCoordinatorConfig struct {
	MaxBytes                int64
	MaxExpandedBytes        int64
	AccountQuotaBytes       int64
	OperationTimeout        time.Duration
	RetentionTTL            time.Duration
	MaxRetentionTTL         time.Duration
	RetryInterval           time.Duration
	ReconcileTimeout        time.Duration
	DrainTimeout            time.Duration
	MultipartConcurrency    int
	MaxConcurrentOperations int
}

type snapshotParameters struct {
	RetentionTTLSec int64 `json:"retention_ttl_sec,omitempty"`
	LeaseTTLSec     int   `json:"lease_ttl_sec,omitempty"`
}

type SnapshotCoordinator struct {
	service      *RegistryService
	repository   *sandboxsnapshot.Repository
	objects      SnapshotObjects
	config       SnapshotCoordinatorConfig
	mu           sync.Mutex
	runCtx       context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	jobs         map[string]bool
	reservations map[string]*PersistenceReservation
	wg           sync.WaitGroup
}

func NewSnapshotCoordinator(service *RegistryService, objects SnapshotObjects, cfg SnapshotCoordinatorConfig) (*SnapshotCoordinator, error) {
	if service == nil || service.snapshotRepository == nil || objects == nil || cfg.MaxBytes <= 0 || cfg.MaxExpandedBytes <= 0 || cfg.MaxExpandedBytes > 1<<60 || cfg.MaxBytes > 1<<60 || cfg.AccountQuotaBytes < cfg.MaxBytes || cfg.OperationTimeout < time.Second || cfg.RetentionTTL < time.Second || cfg.MaxRetentionTTL < cfg.RetentionTTL || cfg.RetryInterval <= 0 || cfg.ReconcileTimeout <= 0 || cfg.DrainTimeout <= 0 || cfg.MultipartConcurrency < 1 || cfg.MultipartConcurrency > 16 || cfg.MaxConcurrentOperations < 1 {
		return nil, errors.New("invalid sandbox lifecycle coordinator configuration")
	}
	return &SnapshotCoordinator{service: service, repository: service.snapshotRepository, objects: objects, config: cfg, jobs: make(map[string]bool), reservations: make(map[string]*PersistenceReservation)}, nil
}

// Start returns after recovery admission is installed. Accepted HTTP requests
// use this server context rather than the original request's cancellation.
func (c *SnapshotCoordinator) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runCtx != nil {
		return errors.New("snapshot coordinator already started")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := c.repository.PendingOperations(ctx); err != nil {
		return err
	}
	c.runCtx, c.cancel = context.WithCancel(ctx)
	c.done = make(chan struct{})
	go c.loop(c.runCtx)
	return nil
}

func (c *SnapshotCoordinator) Stop(ctx context.Context) error {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	if cancel == nil {
		c.mu.Unlock()
		return nil
	}
	// Serialize cancellation with launch admission before loop waits for jobs.
	cancel()
	c.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *SnapshotCoordinator) context() (context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runCtx == nil || c.runCtx.Err() != nil {
		return nil, ErrPersistenceUnavailable
	}
	return c.runCtx, nil
}

func (c *SnapshotCoordinator) loop(ctx context.Context) {
	defer func() { c.wg.Wait(); close(c.done) }()
	ticker := time.NewTicker(c.config.RetryInterval)
	defer ticker.Stop()
	for {
		ops, err := c.repository.PendingOperations(ctx)
		if err == nil {
			for _, op := range ops {
				c.launch(op)
			}
		} else if ctx.Err() == nil {
			slog.Error("snapshot recovery metadata unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *SnapshotCoordinator) launch(op sandboxsnapshot.Operation) {
	c.mu.Lock()
	if c.runCtx == nil || c.runCtx.Err() != nil || c.jobs[op.ID] || len(c.jobs) >= c.config.MaxConcurrentOperations {
		c.mu.Unlock()
		return
	}
	c.jobs[op.ID] = true
	ctx := c.runCtx
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer func() { c.mu.Lock(); delete(c.jobs, op.ID); c.mu.Unlock() }()
		current, err := c.repository.Operation(ctx, op.OwnerID, op.SessionID, op.ID)
		if err != nil {
			return
		}
		if current.Status == "failed" {
			_ = c.reconcile(ctx, current)
			return
		}
		if current.Status != "pending" && current.Status != "running" {
			return
		}
		driveCtx := ctx
		cancel := func() {}
		if time.Now().UnixMilli() < current.DeadlineUnixMS {
			driveCtx, cancel = context.WithDeadline(ctx, time.UnixMilli(current.DeadlineUnixMS))
		}
		err = c.drive(driveCtx, current)
		cancel()
		if err != nil && ctx.Err() == nil {
			if c.retryable(err) && time.Now().UnixMilli() < current.DeadlineUnixMS {
				return
			}
			if c.retryable(err) && time.Now().UnixMilli() >= current.DeadlineUnixMS {
				err = context.DeadlineExceeded
			}
			code, message := snapshotFailure(err)
			if c.repository.FailAndHold(ctx, current, code, message) == nil {
				_ = c.reconcile(ctx, current)
			}
		}
	}()
}

func (c *SnapshotCoordinator) retryable(err error) bool {
	if errors.Is(err, errSnapshotInProgress) {
		return true
	}
	var workerErr *CommandExecutionError
	if errors.As(err, &workerErr) {
		return false
	}
	var transportErr *PersistenceControlError
	return errors.As(err, &transportErr)
}

func snapshotFailure(err error) (string, string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", "sandbox lifecycle operation timed out; backend reconciliation is required"
	}
	if errors.Is(err, sandboxsnapshot.ErrQuota) {
		return "snapshot_quota_exceeded", "snapshot storage quota exceeded"
	}
	if errors.Is(err, objectstore.ErrSnapshotIntegrity) {
		return "snapshot_integrity_error", "snapshot archive verification failed"
	}
	if errors.Is(err, ErrNoCapabilityWorker) {
		return "no_worker", "no compatible online worker is available"
	}
	if errors.Is(err, ErrNoWorkerCapacity) {
		return "no_capacity", "compatible worker operation capacity is exhausted"
	}
	var workerErr *CommandExecutionError
	if errors.As(err, &workerErr) {
		switch workerErr.Code {
		case "invalid_payload", "no_capacity", "session_capacity_exceeded", "snapshot_quota_exceeded", "snapshot_integrity_error", "session_state_conflict", "snapshot_storage_error", "persistence_unavailable":
			return workerErr.Code, "worker could not complete the sandbox lifecycle action"
		}
	}
	return "snapshot_storage_error", "sandbox lifecycle action failed; backend reconciliation is required"
}

// Begin opts in the first suspend after reading the existing worker's immutable
// restore spec. Describe is read-only; side effects start only after durable Begin.
func (c *SnapshotCoordinator) Begin(ctx context.Context, owner, id string, kind sandboxsnapshot.Kind, key string, retention time.Duration, leaseTTLSec int) (sandboxsnapshot.Session, *sandboxsnapshot.Operation, error) {
	if _, err := c.context(); err != nil {
		return sandboxsnapshot.Session{}, nil, err
	}
	if owner == "" || id == "" || len(key) == 0 || len(key) > 256 || (kind != sandboxsnapshot.Suspend && kind != sandboxsnapshot.Resume) {
		return sandboxsnapshot.Session{}, nil, ErrSnapshotParameters
	}
	p := snapshotParameters{}
	if kind == sandboxsnapshot.Suspend {
		if retention == 0 {
			retention = c.config.RetentionTTL
		}
		if retention < time.Second || retention > c.config.MaxRetentionTTL || retention%time.Second != 0 {
			return sandboxsnapshot.Session{}, nil, ErrSnapshotParameters
		}
		p.RetentionTTLSec = int64(retention / time.Second)
	} else {
		if leaseTTLSec <= 0 || leaseTTLSec > (1<<31-1) {
			return sandboxsnapshot.Session{}, nil, ErrSnapshotParameters
		}
		p.LeaseTTLSec = leaseTTLSec
	}
	session, err := c.repository.Session(ctx, owner, id)
	if errors.Is(err, sandboxsnapshot.ErrNotFound) && kind == sandboxsnapshot.Suspend {
		if adoptErr := c.adoptForSuspend(ctx, owner, id); adoptErr != nil {
			// Another request may have adopted and claimed the same session while
			// inspection was in flight. Trust that durable record and let Begin apply
			// its account-scoped idempotency/conflict rules; never adopt a replacement.
			if _, lookupErr := c.repository.Session(ctx, owner, id); lookupErr != nil {
				return session, nil, adoptErr
			}
		}
	} else if err != nil {
		return session, nil, err
	}
	parameters, _ := json.Marshal(p)
	op, err := c.service.BeginSnapshotOperation(ctx, sandboxsnapshot.BeginRequest{OwnerID: owner, SessionID: id, Kind: kind, IdempotencyKey: key, Parameters: parameters, DeadlineUnixMS: time.Now().Add(c.config.OperationTimeout).UnixMilli()})
	if errors.Is(err, sandboxsnapshot.ErrAlreadyInState) {
		session, e := c.repository.Session(ctx, owner, id)
		return session, nil, e
	}
	if err != nil {
		return session, nil, err
	}
	session, err = c.repository.Session(ctx, owner, id)
	if err != nil {
		return session, nil, err
	}
	if op.Status == "pending" || op.Status == "running" {
		c.launch(op)
	}
	return session, &op, nil
}

func (c *SnapshotCoordinator) adoptForSuspend(ctx context.Context, owner, id string) error {
	view, ok := c.service.GetTerminalSession(owner, id, time.Now())
	if !ok {
		return sandboxsnapshot.ErrNotFound
	}
	node := c.service.getSession(view.WorkerID)
	if !c.service.persistenceSessionOnline(node) {
		return ErrPersistenceUnavailable
	}
	node.persistence.mu.Lock()
	caps := node.persistence.capabilities
	enabled := caps.GetEnabled()
	node.persistence.mu.Unlock()
	if !enabled {
		return ErrPersistenceUnavailable
	}
	opID, err := c.service.newCommandIDFn()
	if err != nil {
		return err
	}
	describe := sandboxsnapshot.Operation{ID: opID, OwnerID: owner, SessionID: id, Generation: 1, DeadlineUnixMS: time.Now().Add(c.config.ReconcileTimeout).UnixMilli()}
	result, err := c.call(ctx, describe, view.WorkerID, registryv1.SandboxPersistenceControl_INSPECT, wire.Control{}, false)
	if err != nil {
		return err
	}
	if result.State != "running" || result.Spec == nil || result.Spec.Validate() != nil || caps.GetRuntime() != result.Spec.Runtime || caps.GetRuntimeVersion() != result.Spec.RuntimeVersion || caps.GetGuestPlatform() != result.Spec.Platform || !persistenceFormatSupported(caps.GetWriteFormats(), result.Spec.Format) {
		return ErrPersistenceUnavailable
	}
	if err = c.service.AdoptSnapshotSession(ctx, owner, id, view.WorkerID, *result.Spec); err != nil {
		return err
	}
	return nil
}

func (c *SnapshotCoordinator) call(ctx context.Context, op sandboxsnapshot.Operation, worker string, action registryv1.SandboxPersistenceControl_Action, payload wire.Control, cleanup bool) (wire.Result, error) {
	if op.Kind != "" {
		current, err := c.repository.Operation(ctx, op.OwnerID, op.SessionID, op.ID)
		if err != nil {
			return wire.Result{}, err
		}
		session, err := c.repository.Session(ctx, op.OwnerID, op.SessionID)
		if err != nil {
			return wire.Result{}, err
		}
		if session.OperationID != op.ID || session.Generation != op.Generation || current.Generation != op.Generation || (current.Status != "pending" && current.Status != "running" && !(cleanup && current.Status == "failed")) {
			return wire.Result{}, sandboxsnapshot.ErrConflict
		}
	} else if action != registryv1.SandboxPersistenceControl_INSPECT {
		return wire.Result{}, sandboxsnapshot.ErrConflict
	}
	deadline := time.UnixMilli(op.DeadlineUnixMS)
	if cleanup {
		deadline = time.Now().Add(c.config.ReconcileTimeout)
	}
	if !deadline.After(time.Now()) {
		return wire.Result{}, context.DeadlineExceeded
	}
	requestID, err := c.service.newCommandIDFn()
	if err != nil {
		return wire.Result{}, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return wire.Result{}, err
	}
	control := &registryv1.SandboxPersistenceControl{RequestId: requestID, OperationId: op.ID, ScopedSessionId: sandboxsnapshot.ScopeSessionID(op.OwnerID, op.SessionID), Generation: op.Generation, Action: action, DeadlineUnixMs: deadline.UnixMilli(), PayloadJson: encoded}
	c.mu.Lock()
	reservation := c.reservations[op.ID]
	c.mu.Unlock()
	var report *registryv1.SandboxPersistenceReport
	if reservation != nil && reservation.WorkerID() == worker && !cleanup && action != registryv1.SandboxPersistenceControl_INSPECT {
		report, err = reservation.Dispatch(ctx, control)
	} else {
		report, err = c.service.DispatchPersistenceControl(ctx, worker, control)
	}
	if err != nil {
		return wire.Result{}, err
	}
	var result wire.Result
	if err = json.Unmarshal(report.PayloadJson, &result); err != nil || result.State == "" {
		return result, errors.New("worker returned invalid lifecycle metadata")
	}
	if result.State == "busy" {
		return result, errSnapshotInProgress
	}
	return result, nil
}
