package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

// Worker Run owns this process-local binding, just like the ordinary executor
// bindings. Admission occurs on the receive goroutine before asynchronous I/O.
var workerSnapshots atomic.Pointer[snapshotController]

type snapshotConnectionContextKey struct{}

func (c *snapshotController) beginConnection(ctx context.Context, id string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errSnapshotJournalUnavailable
	}
	c.connection = id
	c.recovering = true
	for _, action := range c.actions {
		action.cancel()
	}
	c.mu.Unlock()
	c.manager.mu.Lock()
	c.manager.persistenceDisconnected = true
	c.manager.mu.Unlock()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		c.mu.Lock()
		idle := len(c.actions) == 0
		c.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *snapshotController) recoveredConnection(id string) {
	c.mu.Lock()
	current := c.connection == id && !c.closed
	if current {
		c.recovering = false
	}
	c.mu.Unlock()
	if current {
		c.manager.mu.Lock()
		c.manager.persistenceDisconnected = false
		c.manager.mu.Unlock()
	}
}
func (c *snapshotController) endConnection(id string) {
	c.mu.Lock()
	if c.connection != id {
		c.mu.Unlock()
		return
	}
	c.connection = ""
	c.recovering = false
	for _, action := range c.actions {
		if action.connection == id {
			action.cancel()
		}
	}
	c.mu.Unlock()
	c.manager.mu.Lock()
	c.manager.persistenceDisconnected = true
	for scope, session := range c.manager.sessions {
		if _, persistent := c.manager.persistenceFences[scope]; persistent && session.proxyCancel != nil {
			session.proxyCancel()
		}
	}
	c.manager.mu.Unlock()
}

func (c *snapshotController) Recover(ctx context.Context, ordinary []*registryv1.TerminalSessionRecoveryCandidate, held []*registryv1.SandboxPersistenceRecoveryCandidate) error {
	if err := c.manager.InstallSnapshotJournal(c.journal); err != nil {
		return err
	}
	if err := c.manager.ProtectPersistenceRecovery(held); err != nil {
		return err
	}
	state, err := c.journal.Snapshot()
	if err != nil {
		return err
	}
	for _, candidate := range ordinary {
		if candidate.GetGeneration() == 0 {
			continue
		}
		op := state.Operations[state.Current[candidate.SessionId]]
		if op.ID == "" && candidate.Generation == 1 {
			id := state.Probes[candidate.SessionId]
			if id == "" {
				return errSnapshotJournalConflict
			}
			if err := checkSnapshotGeneration(ctx, id, candidate.SessionId, "", 1); err != nil {
				return err
			}
			instance, err := c.runtime.Describe(ctx, id, candidate.SessionId)
			if err != nil {
				return err
			}
			op = snapshotOperation{ID: "adopt-" + uuid.NewString(), SessionID: candidate.SessionId, Generation: 1, Kind: "adopt", Phase: "active", Spec: instance.Spec, ContainerID: id, LeaseExpiresUnixMS: candidate.LeaseExpiresUnixMs}
			if err := c.journal.Claim(op); err != nil {
				return err
			}
		}
		if op.ID == "" || !op.active() || op.Generation != candidate.Generation || candidate.LeaseExpiresUnixMs <= time.Now().UnixMilli() {
			return errSnapshotJournalConflict
		}
		operation := ""
		if op.Kind == "target" {
			operation = op.ID
		}
		if err := checkSnapshotGeneration(ctx, op.ContainerID, op.SessionID, operation, op.Generation); err != nil {
			return err
		}
		recovered := op
		recovered.LeaseExpiresUnixMS = candidate.LeaseExpiresUnixMs
		if err := c.checkExecutionLease(ctx, recovered); err != nil {
			return err
		}
		if err := c.journal.Update(op.ID, func(o *snapshotOperation) error { o.LeaseExpiresUnixMS = candidate.LeaseExpiresUnixMs; return nil }); err != nil {
			return err
		}
		op.LeaseExpiresUnixMS = candidate.LeaseExpiresUnixMs
		if err := c.runtime.Thaw(ctx, op.ContainerID); err != nil {
			return err
		}
		if err := c.attach(ctx, op, true); err != nil {
			return err
		}
	}
	for _, hint := range held {
		op, known := state.Operations[hint.OperationId]
		if known {
			if op.SessionID != hint.ScopedSessionId || op.Generation != hint.Generation {
				return errSnapshotJournalConflict
			}
			if op.ContainerID == "" || op.terminal() || op.Kind == "delete" {
				continue
			}
			var ids bytesBuffer
			if err := dockerSnapshotCLI(ctx, nil, &ids, "ps", "-a", "--no-trunc", "--filter", "id="+op.ContainerID, "--format", "{{.ID}}"); err != nil {
				return err
			}
			if ids.Len() == 0 {
				continue
			}
			if err := checkSnapshotGeneration(ctx, op.ContainerID, op.SessionID, "", op.Generation); err != nil {
				return err
			}
			if err := c.attach(ctx, op, op.active()); err != nil {
				return err
			}
			continue
		}
		if hint.Role != registryv1.SandboxPersistenceRecoveryCandidate_SOURCE {
			continue
		}
		prior := state.Operations[state.Current[hint.ScopedSessionId]]
		id := state.Probes[hint.ScopedSessionId]
		if prior.Generation == hint.Generation && prior.ContainerID != "" {
			id = prior.ContainerID
		}
		if id == "" {
			continue
		} // The hint protects unknown resources; it cannot create one.
		if err := checkSnapshotGeneration(ctx, id, hint.ScopedSessionId, "", hint.Generation); err != nil {
			return err
		}
		var instance dockerSnapshotInstance
		if prior.ID != "" {
			instance, err = c.runtime.DescribePersistent(ctx, id, hint.ScopedSessionId, prior.Spec)
		} else {
			instance, err = c.runtime.Describe(ctx, id, hint.ScopedSessionId)
		}
		if err != nil {
			return err
		}
		view := snapshotOperation{ID: hint.OperationId, SessionID: hint.ScopedSessionId, Generation: hint.Generation, Kind: "source", Phase: "preparing", Spec: instance.Spec, ContainerID: id, LeaseExpiresUnixMS: prior.LeaseExpiresUnixMS}
		if err := c.attach(ctx, view, false); err != nil {
			return err
		}
	}
	return nil
}

func (c *snapshotController) Usage() *registryv1.SandboxPersistenceUsage {
	state, err := c.journal.Snapshot()
	if err != nil {
		return &registryv1.SandboxPersistenceUsage{Inflight: int32(c.config.MaxInflight)}
	}
	var reserved int64
	var inflight int32
	for _, id := range state.Current {
		op := state.Operations[id]
		if op.held() {
			inflight++
			if op.StagingBytes > c.config.TmpQuotaBytes-reserved {
				return &registryv1.SandboxPersistenceUsage{Inflight: int32(c.config.MaxInflight)}
			}
			reserved += op.StagingBytes
		}
	}
	used, err := c.artifactBytes()
	if err != nil {
		return &registryv1.SandboxPersistenceUsage{Inflight: int32(c.config.MaxInflight)}
	}
	available := max(int64(0), c.config.TmpQuotaBytes-reserved-used)
	var fs syscall.Statfs_t
	if syscall.Statfs(c.journal.root, &fs) != nil {
		available = 0
	} else {
		free := uint64(fs.Bavail) * uint64(fs.Bsize)
		if free < uint64(available) {
			available = int64(free)
		}
	}
	return &registryv1.SandboxPersistenceUsage{Inflight: inflight, AvailableStagingBytes: available}
}
func (c *snapshotController) Capabilities() *registryv1.SandboxPersistenceCapabilities {
	usage := c.Usage()
	return &registryv1.SandboxPersistenceCapabilities{Enabled: true, Runtime: "docker", RuntimeVersion: c.runtime.version, GuestPlatform: c.runtime.platform, ReadFormats: []string{wire.DockerRootFSFormat}, WriteFormats: []string{wire.DockerRootFSFormat}, MaxInflight: int32(c.config.MaxInflight), AvailableStagingBytes: usage.AvailableStagingBytes}
}

func (c *snapshotController) maintain(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	deletionSweeps := make(map[string]time.Time)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		c.mu.Lock()
		recovering := c.recovering || c.closed
		c.mu.Unlock()
		if recovering {
			continue
		}
		state, err := c.journal.Snapshot()
		if err != nil {
			continue
		}
		for _, id := range state.Current {
			op := state.Operations[id]
			if op.Kind == "delete" && op.Phase == "deleted" {
				c.reconcileDeletedOperation(op, time.Now(), deletionSweeps)
				continue
			}
			if op.LeaseExpiresUnixMS <= 0 || op.LeaseExpiresUnixMS > time.Now().UnixMilli() {
				continue
			}
			if op.Kind == "target" && (op.Phase == "ready" || op.Phase == "importing") {
				body := []byte(`{}`)
				c.Handle(&registryv1.SandboxPersistenceControl{RequestId: uuid.NewString(), OperationId: op.ID, ScopedSessionId: op.SessionID, Generation: op.Generation, Action: registryv1.SandboxPersistenceControl_CANCEL, DeadlineUnixMs: time.Now().Add(10 * time.Second).UnixMilli(), PayloadJson: body})
			} else if op.active() {
				c.mu.Lock()
				busy := c.closed || c.recovering || c.actions[op.SessionID].cancel != nil
				if !busy {
					c.actions[op.SessionID] = snapshotActiveAction{operation: op.ID, generation: op.Generation, cancel: func() {}}
					c.wg.Add(1)
				}
				c.mu.Unlock()
				if !busy {
					c.expireActive(ctx, op)
					c.mu.Lock()
					delete(c.actions, op.SessionID)
					c.mu.Unlock()
					c.wg.Done()
				}
			}
		}
		_ = c.evictCache()
	}
}

// Native imports accepted before cancellation can finish after purge ACK.
// Keep checking durable tombstones independently of Console cleanup jobs.
func (c *snapshotController) reconcileDeletedOperation(op snapshotOperation, now time.Time, next map[string]time.Time) {
	if now.Before(next[op.ID]) {
		return
	}
	next[op.ID] = now.Add(time.Minute)
	body, _ := json.Marshal(wire.Control{PurgeSession: true, Spec: &op.Spec})
	run := c.admit("", &registryv1.SandboxPersistenceControl{RequestId: uuid.NewString(), OperationId: op.ID, ScopedSessionId: op.SessionID, Generation: op.Generation, Action: registryv1.SandboxPersistenceControl_CANCEL, DeadlineUnixMs: now.Add(10 * time.Second).UnixMilli(), PayloadJson: body})
	go run()
}

func snapshotHeartbeatUsage() *registryv1.SandboxPersistenceUsage {
	if controller := workerSnapshots.Load(); controller != nil {
		return controller.Usage()
	}
	return nil
}

func (c *snapshotController) stopForShutdown(ctx context.Context) error {
	state, err := c.journal.Snapshot()
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range state.Current {
		op := state.Operations[id]
		if op.ContainerID == "" || op.terminal() {
			continue
		}
		// Paused sources already have all writers stopped. Do not thaw them
		// merely to stop them; unfinished preparation can still be running.
		var ids bytesBuffer
		if err := dockerSnapshotCLI(ctx, nil, &ids, "ps", "-a", "--no-trunc", "--filter", "id="+op.ContainerID, "--format", "{{.ID}}"); err != nil {
			failures = append(failures, err)
			continue
		}
		if ids.Len() == 0 {
			continue
		}
		var native struct {
			Running bool
			Paused  bool
		}
		if err := dockerSnapshotJSON(ctx, &native, "inspect", "--format", "{{json .State}}", op.ContainerID); err != nil {
			failures = append(failures, err)
			continue
		}
		if native.Running && !native.Paused {
			if err := dockerSnapshotCLI(ctx, nil, io.Discard, "stop", "--signal", "SIGKILL", "--timeout", "0", op.ContainerID); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func (c *snapshotController) expireActive(ctx context.Context, op snapshotOperation) {
	m := c.manager
	m.mu.Lock()
	local := m.sessions[op.SessionID]
	if local != nil {
		m.mu.Unlock()
		m.expireSessionLease(local)
		return
	}
	state, err := c.journal.Snapshot()
	if err != nil {
		m.mu.Unlock()
		return
	}
	current := state.Operations[state.Current[op.SessionID]]
	valid := current.ID == op.ID && current.Generation == op.Generation && current.active() && current.LeaseExpiresUnixMS <= time.Now().UnixMilli()
	m.mu.Unlock()
	if !valid {
		return
	}
	stop, cancel := context.WithTimeout(ctx, terminalExecCleanupTimeout)
	defer cancel()
	_ = dockerSnapshotCLI(stop, nil, io.Discard, "stop", "--signal", "SIGKILL", "--timeout", "0", op.ContainerID)
}
