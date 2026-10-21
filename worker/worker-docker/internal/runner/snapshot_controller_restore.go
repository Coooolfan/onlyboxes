package runner

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

func (c *snapshotController) restore(ctx context.Context, op snapshotOperation, p wire.Control) (wire.Result, error) {
	if op.Phase == "ready" || op.Phase == "active" {
		if err := c.checkRunning(ctx, op); err != nil {
			return wire.Result{}, err
		}
		return snapshotOperationResult(op), nil
	}
	if op.Phase != "prepared" && op.Phase != "importing" {
		return wire.Result{}, errSnapshotJournalConflict
	}
	if op.Phase == "prepared" {
		if err := c.update(op.ID, func(o *snapshotOperation) error { o.Phase = "importing"; return nil }); err != nil {
			return wire.Result{}, err
		}
		op.Phase = "importing"
	}
	file, release, err := c.openArtifact(op.Artifact)
	if err == nil {
		err = verifySnapshotFile(ctx, file, *op.Archive)
		release()
	}
	if err != nil {
		if err := c.copyCached(ctx, op); err != nil {
			if p.Download == nil {
				return wire.Result{}, errSnapshotJournalConflict
			}
			if err := c.transfer.Download(ctx, *p.Download, *op.Archive, c.artifact(op.Artifact)); err != nil {
				return wire.Result{}, err
			}
		}
	}
	if !op.Verified {
		if err := c.update(op.ID, func(o *snapshotOperation) error {
			o.Verified = true
			o.CacheUntilUnixMS = time.Now().Add(c.config.CacheTTL).UnixMilli()
			return nil
		}); err != nil {
			return wire.Result{}, err
		}
	}
	file, release, err = c.openArtifact(op.Artifact)
	if err != nil {
		return wire.Result{}, err
	}
	defer release()
	if op.ImageID == "" {
		// The operation-specific tag makes a lost import acknowledgement
		// discoverable without constructing another running instance.
		var images bytesBuffer
		tag := c.imageTag(op)
		if err := dockerSnapshotCLI(ctx, nil, &images, "image", "ls", "--no-trunc", "--filter", "reference="+tag, "--format", "{{.ID}}"); err != nil {
			return wire.Result{}, err
		}
		image := strings.TrimSpace(images.String())
		if image == "" {
			image, err = c.runtime.Import(ctx, file, *op.Archive, op.Spec, tag)
			if err != nil {
				return wire.Result{}, err
			}
		}
		if !dockerSnapshotDigest.MatchString(image) {
			return wire.Result{}, errSnapshotJournalConflict
		}
		var metadata struct {
			Config struct{ Labels map[string]string }
		}
		if err := dockerSnapshotJSON(ctx, &metadata, "image", "inspect", "--format", "{{json .}}", image); err != nil {
			return wire.Result{}, err
		}
		if metadata.Config.Labels["onlyboxes.snapshot.sha256"] != op.Archive.SHA256 {
			return wire.Result{}, errSnapshotArchiveIntegrity
		}
		if err := c.update(op.ID, func(o *snapshotOperation) error { o.ImageID = image; return nil }); err != nil {
			return wire.Result{}, err
		}
		op.ImageID = image
	}
	if op.ContainerID == "" {
		id, err := snapshotContainerID(ctx, op.SessionID)
		if err != nil {
			return wire.Result{}, err
		}
		if id == "" {
			args, err := dockerSnapshotCreateArgs(terminalSessionResourceName(op.SessionID), op.SessionID, op.ImageID, op.ID, op.Generation, op.Spec, c.manager.dockerNetwork)
			if err != nil {
				return wire.Result{}, err
			}
			args = append([]string{"create", "--label", "onlyboxes.persistence=true", "--label", "onlyboxes.worker_id=" + c.workerID}, args[1:]...)
			if err := dockerSnapshotCLI(ctx, nil, io.Discard, args...); err != nil {
				return wire.Result{}, err
			}
			id, err = snapshotContainerID(ctx, op.SessionID)
			if err != nil || id == "" {
				return wire.Result{}, errSnapshotJournalConflict
			}
		}
		if err := checkSnapshotGeneration(ctx, id, op.SessionID, op.ID, op.Generation); err != nil {
			return wire.Result{}, err
		}
		if err := c.update(op.ID, func(o *snapshotOperation) error { o.ContainerID = id; return nil }); err != nil {
			return wire.Result{}, err
		}
		op.ContainerID = id
	}
	if op.LeaseExpiresUnixMS == 0 {
		lease := time.Now().Add(time.Duration(op.LeaseTTLSec) * time.Second).UnixMilli()
		if err := c.update(op.ID, func(o *snapshotOperation) error { o.LeaseExpiresUnixMS = lease; return nil }); err != nil {
			return wire.Result{}, err
		}
		op.LeaseExpiresUnixMS = lease
	}
	if op.LeaseExpiresUnixMS <= time.Now().UnixMilli() {
		return wire.Result{}, errSnapshotJournalConflict
	}
	var actual struct {
		Status string
		Paused bool
	}
	if err := dockerSnapshotJSON(ctx, &actual, "inspect", "--format", "{{json .State}}", op.ContainerID); err != nil {
		return wire.Result{}, err
	}
	// A running instance without its live guard receipt is an unknown outcome;
	// do not implicitly re-arm it through a repeated restore request.
	if actual.Status == "created" {
		if err := c.armExecutionLease(ctx, op); err != nil {
			return wire.Result{}, err
		}
	} else {
		if err := c.checkExecutionLease(ctx, op); err != nil {
			return wire.Result{}, err
		}
	}
	if actual.Status == "created" {
		if err := dockerSnapshotCLI(ctx, nil, io.Discard, "start", op.ContainerID); err != nil {
			return wire.Result{}, err
		}
	}
	if err := c.checkRunning(ctx, op); err != nil {
		return wire.Result{}, err
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "exec", op.ContainerID, "sh", "-c", "true"); err != nil {
		return wire.Result{}, err
	}
	if err := c.update(op.ID, func(o *snapshotOperation) error { o.Phase = "ready"; return nil }); err != nil {
		return wire.Result{}, err
	}
	if err := c.attach(ctx, op, false); err != nil {
		return wire.Result{}, err
	}
	return c.result(op.ID)
}

func (c *snapshotController) checkRunning(ctx context.Context, op snapshotOperation) error {
	if op.ContainerID == "" || op.LeaseExpiresUnixMS <= time.Now().UnixMilli() {
		return errSnapshotJournalConflict
	}
	if err := c.checkExecutionLease(ctx, op); err != nil {
		return err
	}
	operation := ""
	if op.Kind == "target" {
		operation = op.ID
	}
	if err := checkSnapshotGeneration(ctx, op.ContainerID, op.SessionID, operation, op.Generation); err != nil {
		return err
	}
	var state struct {
		Status string
		Paused bool
	}
	if err := dockerSnapshotJSON(ctx, &state, "inspect", "--format", "{{json .State}}", op.ContainerID); err != nil {
		return err
	}
	if state.Status != "running" || state.Paused {
		return errSnapshotJournalConflict
	}
	return nil
}

func (c *snapshotController) inspect(ctx context.Context, op snapshotOperation) (wire.Result, error) {
	if op.active() || op.Phase == "ready" {
		if err := c.checkRunning(ctx, op); err != nil {
			return wire.Result{}, err
		}
	}
	return snapshotOperationResult(op), nil
}

func (c *snapshotController) cancel(ctx context.Context, op snapshotOperation, p wire.Control) (wire.Result, error) {
	if op.Kind == "cancel" || op.terminal() {
		return snapshotOperationResult(op), nil
	}
	if op.Kind == "source" && p.Rollback && !op.Committed {
		if op.Phase != "preparing" && op.Phase != "frozen" && op.Phase != "rolling_back" && op.Phase != "rolled_back" {
			return wire.Result{}, errSnapshotJournalConflict
		}
		if op.Phase == "rolled_back" {
			if err := c.checkRunning(ctx, op); err != nil {
				return wire.Result{}, err
			}
		} else {
			lease := op.LeaseExpiresUnixMS
			if op.Phase != "rolling_back" {
				if lease <= time.Now().UnixMilli() {
					lease = time.Now().Add(time.Duration(c.manager.leaseDefaultSec) * time.Second).UnixMilli()
				}
				if err := c.update(op.ID, func(o *snapshotOperation) error { o.Phase = "rolling_back"; o.LeaseExpiresUnixMS = lease; return nil }); err != nil {
					return wire.Result{}, err
				}
			}
			if lease <= time.Now().UnixMilli() {
				return wire.Result{}, errSnapshotJournalConflict
			}
			op.Phase = "rolling_back"
			op.LeaseExpiresUnixMS = lease
			if err := c.armExecutionLease(ctx, op); err != nil {
				return wire.Result{}, err
			}
			if err := c.runtime.Thaw(ctx, op.ContainerID); err != nil {
				return wire.Result{}, err
			}
			if err := c.checkRunning(ctx, op); err != nil {
				return wire.Result{}, err
			}
			if err := c.update(op.ID, func(o *snapshotOperation) error { o.Phase = "rolled_back"; return nil }); err != nil {
				return wire.Result{}, err
			}
		}
		if err := c.attach(ctx, op, true); err != nil {
			return wire.Result{}, err
		}
		return c.result(op.ID)
	}
	if op.Phase == "active" && !p.Delete {
		return snapshotOperationResult(op), nil
	}
	if op.Kind != "target" || p.Rollback {
		return wire.Result{}, errSnapshotJournalConflict
	}
	if err := c.update(op.ID, func(o *snapshotOperation) error { o.Phase = "canceling"; return nil }); err != nil {
		return wire.Result{}, err
	}
	id := op.ContainerID
	if id == "" {
		var err error
		id, err = snapshotContainerID(ctx, op.SessionID)
		if err != nil {
			return wire.Result{}, err
		}
		if id != "" {
			if err := checkSnapshotGeneration(ctx, id, op.SessionID, op.ID, op.Generation); err != nil {
				return wire.Result{}, err
			}
		}
	}
	if id != "" {
		if err := c.runtime.Remove(ctx, id); err != nil {
			return wire.Result{}, err
		}
	}
	if err := c.forgetExecutionLease(ctx, op); err != nil {
		return wire.Result{}, err
	}
	c.detach(op)
	if err := c.update(op.ID, func(o *snapshotOperation) error { o.Phase = "canceled"; return nil }); err != nil {
		return wire.Result{}, err
	}
	_ = os.Remove(c.artifact(op.Artifact) + ".partial")
	return c.result(op.ID)
}

func (c *snapshotController) cancelUnknown(ctx context.Context, control *registryv1.SandboxPersistenceControl) error {
	id, err := snapshotContainerID(ctx, control.ScopedSessionId)
	if err != nil {
		return err
	}
	if id != "" {
		// Only a backend carrying this exact operation/generation can belong
		// to an unrecorded target. An unrelated same-name instance is untouched.
		if err := checkSnapshotGeneration(ctx, id, control.ScopedSessionId, control.OperationId, control.Generation); err == nil {
			if err := c.runtime.Remove(ctx, id); err != nil {
				return err
			}
		}
	}
	if err := c.journal.CancelUnknown(control.OperationId, control.ScopedSessionId, control.Generation); err != nil {
		return err
	}
	return c.manager.InstallSnapshotJournal(c.journal)
}

func (c *snapshotController) attach(ctx context.Context, op snapshotOperation, active bool) error {
	if active {
		if err := c.checkExecutionLease(ctx, op); err != nil {
			return err
		}
	}
	ip := ""
	if c.manager.dockerNetwork != "" {
		var err error
		ip, err = inspectTerminalContainerIP(ctx, op.ContainerID, c.manager.dockerNetwork)
		if err != nil {
			return err
		}
	}
	m := c.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.persistenceFailed {
		return errSnapshotJournalUnavailable
	}
	if local := m.sessions[op.SessionID]; local != nil {
		if local.persistenceContainerID != "" && local.persistenceContainerID != op.ContainerID {
			return errSnapshotJournalConflict
		}
		local.persistenceContainerID = op.ContainerID
		local.leaseExpiresAt = time.UnixMilli(op.LeaseExpiresUnixMS)
		if ip != "" {
			local.containerIP = ip
		}
	} else {
		ready := make(chan struct{})
		close(ready)
		proxy, cancel := context.WithCancel(context.Background())
		m.sessions[op.SessionID] = &terminalSession{sessionID: op.SessionID, containerName: terminalSessionResourceName(op.SessionID), persistenceContainerID: op.ContainerID, containerIP: ip, leaseExpiresAt: time.UnixMilli(op.LeaseExpiresUnixMS), ready: ready, capacityReserved: true, proxyCtx: proxy, proxyCancel: cancel}
		m.activeSessionReservations++
	}
	m.setPersistenceFenceLocked(op.SessionID, terminalPersistenceFence{operationID: op.ID, generation: op.Generation, containerID: op.ContainerID, active: active, held: !active, capacity: true})
	return nil
}

func (c *snapshotController) detach(op snapshotOperation) {
	m := c.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if local := m.sessions[op.SessionID]; local != nil {
		if local.persistenceContainerID != "" && local.persistenceContainerID != op.ContainerID {
			return
		}
		m.stopSessionLeaseTimerLocked(local)
		if local.proxyCancel != nil {
			local.proxyCancel()
		}
		delete(m.sessions, op.SessionID)
		if local.capacityReserved {
			local.capacityReserved = false
			m.activeSessionReservations--
		}
	}
}

func (c *snapshotController) recoverSourceRollback(ctx context.Context, control *registryv1.SandboxPersistenceControl, spec *wire.RestoreSpec) (snapshotOperation, error) {
	if spec == nil || spec.Validate() != nil {
		return snapshotOperation{}, errSnapshotJournalConflict
	}
	c.manager.mu.Lock()
	local := c.manager.sessions[control.ScopedSessionId]
	c.manager.mu.Unlock()
	if local != nil {
		_ = c.manager.HoldSource(control.ScopedSessionId, control.OperationId, control.Generation)
	}
	id, err := snapshotContainerID(ctx, control.ScopedSessionId)
	if err != nil || id == "" {
		return snapshotOperation{}, errSnapshotJournalConflict
	}
	if err := checkSnapshotGeneration(ctx, id, control.ScopedSessionId, "", control.Generation); err != nil {
		return snapshotOperation{}, err
	}
	c.manager.mu.Lock()
	expected := c.manager.persistenceBootstrap[control.ScopedSessionId]
	if local != nil && local.persistenceContainerID != "" {
		expected = local.persistenceContainerID
	}
	c.manager.mu.Unlock()
	if expected == "" {
		state, e := c.journal.Snapshot()
		if e != nil {
			return snapshotOperation{}, e
		}
		expected = state.Operations[state.Current[control.ScopedSessionId]].ContainerID
	}
	if expected == "" || expected != id {
		return snapshotOperation{}, errSnapshotJournalConflict
	}
	instance, err := c.runtime.DescribePersistent(ctx, id, control.ScopedSessionId, *spec)
	if err != nil {
		return snapshotOperation{}, err
	}
	op := snapshotOperation{ID: control.OperationId, SessionID: control.ScopedSessionId, Generation: control.Generation, Kind: "source", Phase: "preparing", Spec: instance.Spec, ContainerID: id, LeaseExpiresUnixMS: time.Now().Add(time.Duration(c.manager.leaseDefaultSec) * time.Second).UnixMilli(), Artifact: snapshotArtifactName(control.ScopedSessionId, control.OperationId, control.Generation)}
	if err := c.journal.Claim(op); err != nil {
		return snapshotOperation{}, err
	}
	if err := c.attach(ctx, op, false); err != nil {
		return snapshotOperation{}, err
	}
	return op, nil
}
