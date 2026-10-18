package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
)

type SnapshotCleanupObjects interface {
	AbortAndConfirmSnapshotUpload(context.Context, objectstore.SnapshotUpload) error
	PurgeSnapshot(context.Context, objectstore.SnapshotObject, string) error
}
type SnapshotCleanerConfig struct {
	Interval, Grace, RetryInterval, CallTimeout time.Duration
	MaxJobs                                     int
}
type SnapshotCleaner struct {
	service    *RegistryService
	repository *sandboxsnapshot.Repository
	objects    SnapshotCleanupObjects
	config     SnapshotCleanerConfig
	mu         sync.Mutex
	jobs       map[string]bool
	cancel     context.CancelFunc
	done       chan struct{}
	wg         sync.WaitGroup
}

func NewSnapshotCleaner(service *RegistryService, objects SnapshotCleanupObjects, cfg SnapshotCleanerConfig) (*SnapshotCleaner, error) {
	if service == nil || service.snapshotRepository == nil || cfg.Interval < time.Second || cfg.Grace < time.Second || cfg.RetryInterval <= 0 || cfg.CallTimeout <= 0 || cfg.MaxJobs < 1 || cfg.MaxJobs > 64 {
		return nil, errors.New("invalid snapshot cleaner configuration")
	}
	return &SnapshotCleaner{service: service, repository: service.snapshotRepository, objects: objects, config: cfg, jobs: make(map[string]bool)}, nil
}
func (c *SnapshotCleaner) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return errors.New("snapshot cleaner already started")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	run, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.done = make(chan struct{})
	go c.loop(run)
	return nil
}
func (c *SnapshotCleaner) Stop(ctx context.Context) error {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	if cancel != nil {
		cancel()
	}
	c.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *SnapshotCleaner) launch(ctx context.Context, key string, work func(context.Context)) {
	c.mu.Lock()
	if ctx.Err() != nil || c.jobs[key] || len(c.jobs) >= c.config.MaxJobs {
		c.mu.Unlock()
		return
	}
	c.jobs[key] = true
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer func() { c.mu.Lock(); delete(c.jobs, key); c.mu.Unlock() }()
		work(ctx)
	}()
}
func (c *SnapshotCleaner) loop(ctx context.Context) {
	defer func() { c.wg.Wait(); close(c.done) }()
	ticker := time.NewTicker(c.config.RetryInterval)
	defer ticker.Stop()
	nextSweep := time.Time{}
	for {
		if ctx.Err() != nil {
			return
		}
		now := time.Now()

		if !now.Before(nextSweep) {
			c.launch(ctx, "snapshot-gc", func(ctx context.Context) {
				if err := c.sweep(ctx, time.Now()); err != nil && ctx.Err() == nil {
					slog.Error("snapshot cleanup sweep did not complete")
				}
			})
			nextSweep = now.Add(c.config.Interval)
		}
		jobs, err := c.repository.PendingDeletions(ctx, now, 100)
		if err != nil && ctx.Err() == nil {
			slog.Error("snapshot deletion queue unavailable")
		}
		for _, job := range jobs {
			job := job
			c.launch(ctx, job.ID(), func(ctx context.Context) { c.deleteSession(ctx, job) })
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (c *SnapshotCleaner) deleteSession(ctx context.Context, job sandboxsnapshot.DeletionJob) {
	code := ""
	session, err := c.repository.Session(ctx, job.OwnerID, job.SessionID)
	if err != nil || session.State != sandboxsnapshot.Deleting || session.Generation != job.FenceGeneration() {
		return
	}
	workers, err := c.repository.DeletionWorkers(ctx, job)
	if err != nil {
		return
	}
	for _, worker := range workers {
		if worker.Done {
			continue
		}
		call, stop := context.WithTimeout(ctx, c.config.CallTimeout)
		payload, _ := json.Marshal(wire.Control{PurgeSession: true, Spec: &session.Spec})
		control := &registryv1.SandboxPersistenceControl{RequestId: uuid.NewString(), OperationId: job.ID(), ScopedSessionId: session.ScopedSessionID, Generation: job.FenceGeneration(), Action: registryv1.SandboxPersistenceControl_CANCEL, DeadlineUnixMs: time.Now().Add(c.config.CallTimeout).UnixMilli(), PayloadJson: payload}
		report, err := c.service.DispatchPersistenceControl(call, worker.WorkerID, control)
		stop()
		var result wire.Result
		if err != nil {
			code = "no_worker"
			continue
		}
		if report.GetError() != nil || json.Unmarshal(report.GetPayloadJson(), &result) != nil || result.State != "deleted" {
			code = "cleanup_pending"
			continue
		}
		if err := c.repository.MarkDeletionWorkerDone(ctx, job, worker.WorkerID); err != nil {
			return
		}
	}
	native, err := c.repository.ConfirmNativeDeletion(ctx, job)
	if err != nil {
		return
	}
	if native {
		// Explicitly deleted sessions bypass age grace after native confirmation.
		if err := c.collect(ctx, time.Now(), job.OwnerID, job.SessionID); err != nil {
			code = "snapshot_storage_error"
		}
		finished, err := c.repository.FinishDeletion(ctx, job)
		if err == nil && finished {
			return
		}
	}
	if ctx.Err() == nil {
		_ = c.repository.RetryDeletion(ctx, job, code, time.Now().Add(c.config.RetryInterval))
	}
}
func (c *SnapshotCleaner) collect(ctx context.Context, now time.Time, owner, session string) error {
	snapshots, err := c.repository.CleanupSessionSnapshots(ctx, now.Add(-c.config.Grace), now, owner, session, 100)
	if err != nil {
		return err
	}
	var failures []error
	for _, snapshot := range snapshots {
		if owner != "" && (snapshot.OwnerID != owner || snapshot.SessionID != session) {
			continue
		}
		if err := c.purgeSnapshot(ctx, snapshot, now); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
func (c *SnapshotCleaner) purgeSnapshot(ctx context.Context, snapshot sandboxsnapshot.Snapshot, now time.Time) error {
	if err := c.repository.TombstoneSnapshot(ctx, snapshot.OwnerID, snapshot.SessionID, snapshot.ID, now.Add(-c.config.Grace)); err != nil {
		if errors.Is(err, sandboxsnapshot.ErrConflict) {
			return nil
		}
		return err
	}
	run, stop := context.WithTimeout(ctx, c.config.CallTimeout)
	defer stop()
	if c.objects == nil {
		_ = c.repository.RetrySnapshotCleanup(ctx, snapshot, time.Now().Add(c.config.RetryInterval))
		return ErrPersistenceUnavailable
	}
	if err := c.objects.AbortAndConfirmSnapshotUpload(run, snapshot.Upload); err != nil {
		_ = c.repository.RetrySnapshotCleanup(ctx, snapshot, time.Now().Add(c.config.RetryInterval))
		return err
	}
	object := snapshot.Upload.Object
	if snapshot.Manifest != nil {
		object = snapshot.Manifest.Archive
	}
	if err := c.objects.PurgeSnapshot(run, object, snapshot.ManifestVersionID); err != nil {
		_ = c.repository.RetrySnapshotCleanup(ctx, snapshot, time.Now().Add(c.config.RetryInterval))
		return err
	}
	return c.repository.MarkSnapshotDeleted(ctx, snapshot.OwnerID, snapshot.SessionID, snapshot.ID)
}
func (c *SnapshotCleaner) sweep(ctx context.Context, now time.Time) error {
	expired, err := c.repository.ExpiredSessions(ctx, now, 100)
	if err != nil {
		return err
	}
	for _, session := range expired {
		if err := c.service.ExpireSnapshotSession(ctx, session.OwnerID, session.SessionID); err != nil && !errors.Is(err, sandboxsnapshot.ErrConflict) && !errors.Is(err, sandboxsnapshot.ErrNotFound) {
			return err
		}
	}
	return c.collect(ctx, now, "", "")
}
