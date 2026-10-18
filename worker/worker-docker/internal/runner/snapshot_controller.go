package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

type snapshotControllerConfig struct {
	MaxInflight         int
	TmpQuotaBytes       int64
	CacheMaxBytes       int64
	CacheTTL            time.Duration
	DrainTimeout        time.Duration
	TransferConcurrency int
	TransferIdleTimeout time.Duration
}

type snapshotActiveAction struct {
	operation  string
	generation int64
	cancel     context.CancelFunc
}

type snapshotController struct {
	ctx         context.Context
	manager     *terminalSessionManager
	journal     *snapshotJournal
	runtime     *dockerSnapshotRuntime
	transfer    *snapshotTransfer
	config      snapshotControllerConfig
	mu          sync.Mutex
	admissionMu sync.Mutex
	actions     map[string]snapshotActiveAction
	pins        map[string]int
	closed      bool
	wg          sync.WaitGroup
}

func newSnapshotController(ctx context.Context, m *terminalSessionManager, j *snapshotJournal, r *dockerSnapshotRuntime, cfg snapshotControllerConfig) (*snapshotController, error) {
	if m == nil || j == nil || r == nil || cfg.MaxInflight < 1 || cfg.MaxInflight > 64 || cfg.TmpQuotaBytes <= 0 || cfg.TmpQuotaBytes > 1<<60 || cfg.CacheMaxBytes < 0 || cfg.CacheMaxBytes > cfg.TmpQuotaBytes || cfg.CacheTTL < time.Second || cfg.DrainTimeout < time.Second {
		return nil, errSnapshotRuntimeUnsupported
	}
	transfer, err := newSnapshotTransfer(cfg.TransferConcurrency, cfg.TransferIdleTimeout)
	if err != nil {
		return nil, err
	}
	c := &snapshotController{ctx: ctx, manager: m, journal: j, runtime: r, transfer: transfer, config: cfg, actions: map[string]snapshotActiveAction{}, pins: map[string]int{}}
	if err := m.InstallSnapshotJournal(j); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.persistenceLeaseWriter = c.extendLease
	m.mu.Unlock()
	return c, nil
}

func (c *snapshotController) Handle(control *registryv1.SandboxPersistenceControl) *registryv1.SandboxPersistenceReport {
	report := &registryv1.SandboxPersistenceReport{RequestId: control.GetRequestId(), OperationId: control.GetOperationId(), ScopedSessionId: control.GetScopedSessionId(), Generation: control.GetGeneration(), Action: control.GetAction(), Completed: true}
	finish := func(result wire.Result, err error) *registryv1.SandboxPersistenceReport {
		if errors.Is(err, errSnapshotTransferRetry) {
			result = wire.Result{State: "busy"}
			err = nil
		}
		if err != nil {
			code := "snapshot_storage_error"
			var terminalErr *terminalExecError
			if errors.Is(err, errSnapshotJournalUnavailable) {
				c.manager.mu.Lock()
				c.manager.persistenceFailed = true
				c.manager.mu.Unlock()
			}
			switch {
			case errors.As(err, &terminalErr):
				code = terminalErr.Code()
			case errors.Is(err, errSnapshotJournalConflict):
				code = "session_state_conflict"
			case errors.Is(err, errSnapshotJournalUnavailable):
				code = "persistence_unavailable"
			case errors.Is(err, errSnapshotArchiveIntegrity):
				code = "snapshot_integrity_error"
			case errors.Is(err, errSnapshotArchiveQuota):
				code = "snapshot_quota_exceeded"
			case errors.Is(err, errSnapshotRuntimeUnsupported):
				code = "persistence_unavailable"
			}
			report.Error = &registryv1.CommandError{Code: code, Message: "worker could not complete the sandbox lifecycle action"}
		} else {
			report.PayloadJson, _ = json.Marshal(result)
		}
		return report
	}
	if control == nil || !snapshotOperationID.MatchString(control.GetRequestId()) || !snapshotOperationID.MatchString(control.GetOperationId()) || !validPersistenceScope(control.GetScopedSessionId()) || control.GetGeneration() <= 0 || control.GetDeadlineUnixMs() <= time.Now().UnixMilli() || len(control.GetPayloadJson()) > 64<<10 {
		return finish(wire.Result{}, errSnapshotJournalConflict)
	}
	var payload wire.Control
	decoder := json.NewDecoder(bytes.NewReader(control.PayloadJson))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil {
		return finish(wire.Result{}, errSnapshotJournalConflict)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return finish(wire.Result{}, errSnapshotJournalConflict)
	}
	ctx, cancel := context.WithDeadline(c.ctx, time.UnixMilli(control.DeadlineUnixMs))
	defer cancel()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return finish(wire.Result{}, errSnapshotJournalUnavailable)
	}
	if action, busy := c.actions[control.ScopedSessionId]; busy {
		if control.Action == registryv1.SandboxPersistenceControl_CANCEL && action.operation == control.OperationId && action.generation == control.Generation {
			action.cancel()
		}
		c.mu.Unlock()
		return finish(wire.Result{State: "busy"}, nil)
	}
	c.actions[control.ScopedSessionId] = snapshotActiveAction{control.OperationId, control.Generation, cancel}
	c.wg.Add(1)
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.actions, control.ScopedSessionId); c.mu.Unlock(); c.wg.Done() }()
	result, err := c.execute(ctx, control, payload)
	return finish(result, err)
}

func (c *snapshotController) execute(ctx context.Context, control *registryv1.SandboxPersistenceControl, p wire.Control) (wire.Result, error) {
	state, err := c.journal.Snapshot()
	if err != nil {
		return wire.Result{}, err
	}
	op, known := state.Operations[control.OperationId]
	if known && (op.SessionID != control.ScopedSessionId || op.Generation != control.Generation) {
		return wire.Result{}, errSnapshotJournalConflict
	}
	current := state.Operations[state.Current[control.ScopedSessionId]]
	if current.ID != "" && current.Generation > control.Generation {
		return wire.Result{}, errSnapshotJournalConflict
	}
	if known && state.Current[op.SessionID] != op.ID {
		if op.terminal() && (control.Action == registryv1.SandboxPersistenceControl_INSPECT || control.Action == registryv1.SandboxPersistenceControl_CANCEL || control.Action == registryv1.SandboxPersistenceControl_FINALIZE_SUSPEND) {
			return snapshotOperationResult(op), nil
		}
		return wire.Result{}, errSnapshotJournalConflict
	}
	switch control.Action {
	case registryv1.SandboxPersistenceControl_INSPECT:
		if known {
			return c.inspect(ctx, op)
		}
		if control.Generation != 1 || current.ID != "" {
			return wire.Result{State: "missing"}, nil
		}
		instance, err := c.runtime.Describe(ctx, terminalSessionResourceName(control.ScopedSessionId), control.ScopedSessionId)
		if err != nil {
			return wire.Result{}, err
		}
		if err := c.journal.Probe(control.ScopedSessionId, instance.ID); err != nil {
			return wire.Result{}, err
		}
		c.manager.mu.Lock()
		local := c.manager.sessions[control.ScopedSessionId]
		if local == nil || local.destroying || !local.leaseExpiresAt.After(time.Now()) {
			c.manager.mu.Unlock()
			return wire.Result{}, errSnapshotJournalConflict
		}
		if c.manager.persistenceBootstrap == nil {
			c.manager.persistenceBootstrap = map[string]string{}
		}
		c.manager.persistenceBootstrap[control.ScopedSessionId] = instance.ID
		local.persistenceContainerID = instance.ID
		lease := local.leaseExpiresAt.UnixMilli()
		c.manager.mu.Unlock()
		return wire.Result{State: "running", Spec: &instance.Spec, InstanceID: instance.ID, LeaseExpiresUnixMS: lease}, nil
	case registryv1.SandboxPersistenceControl_PREPARE_SNAPSHOT:
		return c.prepareSource(ctx, control, p, op, known)
	case registryv1.SandboxPersistenceControl_UPLOAD_SNAPSHOT:
		if !known || op.Kind != "source" || op.Phase != "frozen" || op.Archive == nil {
			return wire.Result{}, errSnapshotJournalConflict
		}
		file, release, err := c.openArtifact(op.Artifact)
		if err != nil {
			return wire.Result{}, err
		}
		defer release()
		parts, err := c.transfer.Upload(ctx, file, *op.Archive, p.Parts)
		result := snapshotOperationResult(op)
		result.Parts = parts
		return result, err
	case registryv1.SandboxPersistenceControl_VERIFY_SNAPSHOT:
		if !known || op.Kind != "source" || op.Phase != "frozen" || op.Archive == nil || p.Archive == nil || *op.Archive != *p.Archive || p.Download == nil {
			return wire.Result{}, errSnapshotJournalConflict
		}
		if err := c.transfer.VerifyRemote(ctx, *p.Download, *op.Archive); err != nil {
			return wire.Result{}, err
		}
		if err := c.update(op.ID, func(o *snapshotOperation) error { o.RemoteVerified = true; return nil }); err != nil {
			return wire.Result{}, err
		}
		result := snapshotOperationResult(op)
		result.State = "verified"
		return result, nil
	case registryv1.SandboxPersistenceControl_FINALIZE_SUSPEND:
		if !known || op.Kind != "source" || !p.SnapshotCommitted || !op.RemoteVerified {
			return wire.Result{}, errSnapshotJournalConflict
		}
		if op.Phase == "finalized" {
			return snapshotOperationResult(op), nil
		}
		if op.Phase != "frozen" && op.Phase != "committed" {
			return wire.Result{}, errSnapshotJournalConflict
		}
		if err := c.update(op.ID, func(o *snapshotOperation) error { o.Phase = "committed"; o.Committed = true; return nil }); err != nil {
			return wire.Result{}, err
		}
		if err := c.runtime.Remove(ctx, op.ContainerID); err != nil {
			return wire.Result{}, err
		}
		c.detach(op)
		if err := c.update(op.ID, func(o *snapshotOperation) error {
			o.Phase = "finalized"
			o.CacheUntilUnixMS = time.Now().Add(c.config.CacheTTL).UnixMilli()
			return nil
		}); err != nil {
			return wire.Result{}, err
		}
		if err := c.evictCache(); err != nil {
			return wire.Result{}, err
		}
		return c.result(op.ID)
	case registryv1.SandboxPersistenceControl_PREPARE_RESTORE:
		return c.prepareTarget(ctx, control, p, op, known)
	case registryv1.SandboxPersistenceControl_RESTORE:
		if !known || op.Kind != "target" || p.Archive == nil || op.Archive == nil || *p.Archive != *op.Archive {
			return wire.Result{}, errSnapshotJournalConflict
		}
		return c.restore(ctx, op, p)
	case registryv1.SandboxPersistenceControl_ACTIVATE:
		if !known || op.Kind != "target" || (op.Phase != "ready" && op.Phase != "active") || op.LeaseExpiresUnixMS <= time.Now().UnixMilli() {
			return wire.Result{}, errSnapshotJournalConflict
		}
		if err := c.checkRunning(ctx, op); err != nil {
			return wire.Result{}, err
		}
		if err := c.update(op.ID, func(o *snapshotOperation) error { o.Phase = "active"; return nil }); err != nil {
			return wire.Result{}, err
		}
		if err := c.attach(ctx, op, true); err != nil {
			return wire.Result{}, err
		}
		return c.result(op.ID)
	case registryv1.SandboxPersistenceControl_CANCEL:
		if !known {
			if p.Rollback {
				recovered, err := c.recoverSourceRollback(ctx, control, p.Spec)
				if err != nil {
					return wire.Result{}, err
				}
				return c.cancel(ctx, recovered, p)
			}
			if err := c.cancelUnknown(ctx, control); err != nil {
				return wire.Result{}, err
			}
			return wire.Result{State: "canceled"}, nil
		}
		return c.cancel(ctx, op, p)
	default:
		return wire.Result{}, errSnapshotJournalConflict
	}
}

func snapshotOperationResult(op snapshotOperation) wire.Result {
	phase := op.Phase
	switch phase {
	case "preparing", "rolling_back", "canceling":
		phase = "busy"
	case "importing":
		phase = "prepared"
	case "rolled_back":
		phase = "running"
	}
	result := wire.Result{State: phase, Archive: op.Archive, InstanceID: op.ContainerID, LeaseExpiresUnixMS: op.LeaseExpiresUnixMS}
	if op.Kind != "cancel" {
		spec := op.Spec
		result.Spec = &spec
	}
	return result
}
func (c *snapshotController) result(id string) (wire.Result, error) {
	state, err := c.journal.Snapshot()
	if err != nil {
		return wire.Result{}, err
	}
	return snapshotOperationResult(state.Operations[id]), nil
}
func (c *snapshotController) update(id string, change func(*snapshotOperation) error) error {
	if err := c.journal.Update(id, change); err != nil {
		return err
	}
	return c.manager.InstallSnapshotJournal(c.journal)
}
func (c *snapshotController) artifact(name string) string {
	return filepath.Join(c.journal.root, "artifacts", name)
}
func (c *snapshotController) openArtifact(name string) (*os.File, func(), error) {
	if name == "" || filepath.Base(name) != name {
		return nil, nil, errSnapshotArchiveIntegrity
	}
	c.mu.Lock()
	file, err := os.OpenFile(c.artifact(name), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err == nil {
		c.pins[name]++
	}
	c.mu.Unlock()
	if err != nil {
		return nil, nil, errSnapshotArchiveIntegrity
	}
	return file, func() { _ = file.Close(); c.mu.Lock(); c.pins[name]--; c.mu.Unlock() }, nil
}

func (c *snapshotController) reserve(ctx context.Context, stageBytes int64) error {
	if stageBytes <= 0 || stageBytes > c.config.TmpQuotaBytes {
		return errSnapshotArchiveQuota
	}
	state, err := c.journal.Snapshot()
	if err != nil {
		return err
	}
	var reserved int64
	var inflight int
	for _, id := range state.Current {
		op := state.Operations[id]
		if op.held() {
			inflight++
			if op.StagingBytes > c.config.TmpQuotaBytes-reserved {
				return errSnapshotArchiveQuota
			}
			reserved += op.StagingBytes
		}
	}
	if inflight >= c.config.MaxInflight {
		return newTerminalExecError("no_capacity", "snapshot operation capacity is exhausted")
	}
	used, err := c.artifactBytes()
	if err != nil {
		return err
	}
	if used > c.config.TmpQuotaBytes-reserved || stageBytes > c.config.TmpQuotaBytes-reserved-used {
		if err := c.evictCacheBudget(0); err != nil {
			return err
		}
		used, err = c.artifactBytes()
		if err != nil {
			return err
		}
		if used > c.config.TmpQuotaBytes-reserved || stageBytes > c.config.TmpQuotaBytes-reserved-used {
			return errSnapshotArchiveQuota
		}
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(c.journal.root, &fs) != nil {
		return errSnapshotJournalUnavailable
	}
	free := uint64(fs.Bavail) * uint64(fs.Bsize)
	if uint64(stageBytes) > free {
		return errSnapshotArchiveQuota
	}
	var output bytesBuffer
	if err := dockerSnapshotCLI(ctx, nil, &output, "run", "--rm", "--network", "none", "--read-only", "--entrypoint", "df", c.runtime.helperImage, "-Pk", "/"); err != nil {
		return err
	}
	daemonFree, err := parseSnapshotSpace(output.String())
	if err != nil {
		return err
	}
	if stageBytes > daemonFree {
		return errSnapshotArchiveQuota
	}
	return nil
}

type bytesBuffer = bytes.Buffer

func (c *snapshotController) artifactBytes() (int64, error) {
	entries, err := os.ReadDir(filepath.Join(c.journal.root, "artifacts"))
	if err != nil {
		return 0, errSnapshotJournalUnavailable
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return 0, errSnapshotJournalUnavailable
		}
		if info.Size() > c.config.TmpQuotaBytes-total {
			return c.config.TmpQuotaBytes, nil
		}
		total += info.Size()
	}
	return total, nil
}

func (c *snapshotController) prepareSource(ctx context.Context, control *registryv1.SandboxPersistenceControl, p wire.Control, op snapshotOperation, known bool) (wire.Result, error) {
	if p.Spec == nil || p.Spec.Validate() != nil || p.Spec.Runtime != "docker" || p.MaxBytes <= 0 || p.MaxExpandedBytes <= 0 || p.MaxBytes > 1<<60 || p.MaxExpandedBytes > 1<<60 {
		return wire.Result{}, errSnapshotJournalConflict
	}
	if known {
		if op.Kind != "source" || !snapshotSpecEqual(op.Spec, *p.Spec) {
			return wire.Result{}, errSnapshotJournalConflict
		}
		if op.Phase == "frozen" {
			var ids bytes.Buffer
			if err := dockerSnapshotCLI(ctx, nil, &ids, "ps", "-a", "--no-trunc", "--filter", "id="+op.ContainerID, "--format", "{{.ID}}"); err != nil {
				return wire.Result{}, err
			}
			if strings.TrimSpace(ids.String()) != "" {
				var state struct {
					Status string
					Paused bool
				}
				if err := dockerSnapshotJSON(ctx, &state, "inspect", "--format", "{{json .State}}", op.ContainerID); err != nil {
					return wire.Result{}, err
				}
				if state.Status == "running" && !state.Paused {
					return wire.Result{}, errSnapshotJournalConflict
				}
			}
			file, release, err := c.openArtifact(op.Artifact)
			if err != nil {
				return wire.Result{}, err
			}
			defer release()
			if err := verifySnapshotFile(ctx, file, *op.Archive); err != nil {
				return wire.Result{}, err
			}
			return snapshotOperationResult(op), nil
		}
		if op.Phase != "preparing" {
			return wire.Result{}, errSnapshotJournalConflict
		}
	}
	if !known {
		claimErr := func() error {
			c.admissionMu.Lock()
			defer c.admissionMu.Unlock()
			stage := p.MaxBytes + p.MaxBytes/5
			if err := c.reserve(ctx, stage); err != nil {
				return err
			}
			if err := c.manager.HoldSource(control.ScopedSessionId, control.OperationId, control.Generation); err != nil {
				return err
			}
			instance, err := c.runtime.DescribePersistent(ctx, terminalSessionResourceName(control.ScopedSessionId), control.ScopedSessionId, *p.Spec)
			if err != nil {
				return err
			}
			if err := checkSnapshotGeneration(ctx, instance.ID, control.ScopedSessionId, "", control.Generation); err != nil {
				return err
			}
			c.manager.mu.Lock()
			lease := c.manager.sessions[control.ScopedSessionId].leaseExpiresAt.UnixMilli()
			expected := c.manager.persistenceBootstrap[control.ScopedSessionId]
			c.manager.mu.Unlock()
			if expected != "" && expected != instance.ID {
				return errSnapshotJournalConflict
			}
			op = snapshotOperation{ID: control.OperationId, SessionID: control.ScopedSessionId, Generation: control.Generation, Kind: "source", Phase: "preparing", Spec: *p.Spec, ContainerID: instance.ID, LeaseExpiresUnixMS: lease, StagingBytes: stage, Artifact: snapshotArtifactName(control.ScopedSessionId, control.OperationId, control.Generation)}
			if err := c.journal.Claim(op); err != nil {
				return err
			}
			if err := c.manager.InstallSnapshotJournal(c.journal); err != nil {
				return err
			}
			return nil
		}()
		if claimErr != nil {
			return wire.Result{}, claimErr
		}
	}

	c.manager.mu.Lock()
	missing := c.manager.sessions[op.SessionID] == nil
	c.manager.mu.Unlock()
	if missing {
		if err := c.attach(ctx, op, false); err != nil {
			return wire.Result{}, err
		}
	}
	drain := c.config.DrainTimeout
	if p.DrainTimeoutSec > 0 && time.Duration(p.DrainTimeoutSec)*time.Second < drain {
		drain = time.Duration(p.DrainTimeoutSec) * time.Second
	}
	drainCtx, cancel := context.WithTimeout(ctx, drain)
	err := c.manager.DrainPersistence(drainCtx, op.SessionID, op.ID, op.Generation)
	cancel()
	if err != nil {
		return wire.Result{}, err
	}
	instance, err := c.runtime.DescribePersistent(ctx, op.ContainerID, op.SessionID, op.Spec)
	if err != nil {
		return wire.Result{}, err
	}
	instance, err = c.runtime.Freeze(ctx, instance)
	if err != nil {
		return wire.Result{}, err
	}
	archive, name, err := c.runtime.Export(ctx, instance, op.ID, filepath.Join(c.journal.root, "artifacts"), p.MaxBytes, p.MaxExpandedBytes)
	if err != nil {
		return wire.Result{}, err
	}
	if err := os.Rename(name, c.artifact(op.Artifact)); err != nil {
		return wire.Result{}, errSnapshotJournalUnavailable
	}
	if err := syncSnapshotDirectory(filepath.Join(c.journal.root, "artifacts")); err != nil {
		return wire.Result{}, err
	}
	if err := c.update(op.ID, func(o *snapshotOperation) error {
		o.Phase = "frozen"
		o.Archive = &archive
		o.Verified = true
		return nil
	}); err != nil {
		return wire.Result{}, err
	}
	return c.result(op.ID)
}

func (c *snapshotController) prepareTarget(ctx context.Context, control *registryv1.SandboxPersistenceControl, p wire.Control, op snapshotOperation, known bool) (wire.Result, error) {
	if p.Spec == nil || p.Spec.Validate() != nil || p.Spec.Runtime != "docker" || p.Spec.RuntimeVersion != c.runtime.version || p.Spec.Platform != c.runtime.platform || p.Archive == nil || p.Archive.Validate() != nil || p.Archive.Format != p.Spec.Format || p.Archive.Size > 1<<60 || p.Archive.ExpandedBytes > 1<<60 {
		return wire.Result{}, errSnapshotJournalConflict
	}
	if _, err := c.manager.resolveLeaseDuration(&p.LeaseTTLSec); err != nil {
		return wire.Result{}, errSnapshotJournalConflict
	}
	if known {
		if op.Kind != "target" || !snapshotSpecEqual(op.Spec, *p.Spec) || op.Archive == nil || *op.Archive != *p.Archive || op.LeaseTTLSec != p.LeaseTTLSec {
			return wire.Result{}, errSnapshotJournalConflict
		}
		if op.Phase == "prepared" || op.Phase == "importing" || op.Phase == "ready" {
			return snapshotOperationResult(op), nil
		}
		return wire.Result{}, errSnapshotJournalConflict
	}
	c.admissionMu.Lock()
	defer c.admissionMu.Unlock()
	stage := p.Archive.Size*2 + p.Archive.ExpandedBytes
	stage += stage / 5
	if err := c.reserve(ctx, stage); err != nil {
		return wire.Result{}, err
	}
	if id, err := snapshotContainerID(ctx, control.ScopedSessionId); err != nil || id != "" {
		if err != nil {
			return wire.Result{}, err
		}
		return wire.Result{}, errSnapshotJournalConflict
	}
	c.manager.mu.Lock()
	prior := c.manager.persistenceFences[control.ScopedSessionId]
	alreadyReserved := prior.capacity && prior.operationID == control.OperationId && prior.generation == control.Generation
	if !alreadyReserved && c.manager.maxActiveSessions > 0 && c.manager.activeSessionCountLocked() >= c.manager.maxActiveSessions {
		c.manager.mu.Unlock()
		return wire.Result{}, newTerminalExecError("session_capacity_exceeded", "terminal session capacity exceeded")
	}
	c.manager.setPersistenceFenceLocked(control.ScopedSessionId, terminalPersistenceFence{operationID: control.OperationId, generation: control.Generation, held: true, capacity: true})
	c.manager.mu.Unlock()
	op = snapshotOperation{ID: control.OperationId, SessionID: control.ScopedSessionId, Generation: control.Generation, Kind: "target", Phase: "prepared", Spec: *p.Spec, Archive: p.Archive, StagingBytes: stage, LeaseTTLSec: p.LeaseTTLSec, Artifact: snapshotArtifactName(control.ScopedSessionId, control.OperationId, control.Generation)}
	if err := c.journal.Claim(op); err != nil {
		return wire.Result{}, err
	}
	if err := c.manager.InstallSnapshotJournal(c.journal); err != nil {
		return wire.Result{}, err
	}
	return c.result(op.ID)
}

func syncSnapshotDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return errSnapshotJournalUnavailable
	}
	defer file.Close()
	if file.Sync() != nil {
		return errSnapshotJournalUnavailable
	}
	return nil
}

func snapshotContainerID(ctx context.Context, scope string) (string, error) {
	var out bytes.Buffer
	if err := dockerSnapshotCLI(ctx, nil, &out, "ps", "-a", "--no-trunc", "--filter", "name=^/"+terminalSessionResourceName(scope)+"$", "--format", "{{.ID}}"); err != nil {
		return "", err
	}
	id := strings.TrimSpace(out.String())
	if id != "" && !dockerSnapshotDigest.MatchString("sha256:"+id) {
		return "", errSnapshotJournalConflict
	}
	return id, nil
}
func checkSnapshotGeneration(ctx context.Context, id, scope, operation string, generation int64) error {
	var container dockerSnapshotContainer
	if err := dockerSnapshotJSON(ctx, &container, "inspect", "--format", "{{json .}}", id); err != nil {
		return err
	}
	labels := container.Config.Labels
	if labels[terminalExecSessionLabelKey] != terminalSessionIDHash(scope) || labels[terminalExecSchemaLabelKey] != terminalExecSchemaVersion || labels["onlyboxes.managed"] != "true" {
		return errSnapshotJournalConflict
	}
	g := labels["onlyboxes.generation"]
	if g != strconv.FormatInt(generation, 10) && !(generation == 1 && g == "" && operation == "") {
		return errSnapshotJournalConflict
	}
	if operation != "" && labels["onlyboxes.persistence.operation_id"] != operation {
		return errSnapshotJournalConflict
	}
	return nil
}

func (c *snapshotController) imageTag(op snapshotOperation) string {
	hash := sha256.Sum256([]byte(op.SessionID + "\x00" + op.ID + "\x00" + strconv.FormatInt(op.Generation, 10)))
	return "onlyboxes-snapshot-" + hex.EncodeToString(hash[:]) + ":rootfs"
}

func (c *snapshotController) extendLease(scope string, generation int64, until time.Time) error {
	state, err := c.journal.Snapshot()
	if err != nil {
		return err
	}
	op := state.Operations[state.Current[scope]]
	if !op.active() || op.Generation != generation {
		return errSnapshotJournalConflict
	}
	if until.UnixMilli() <= op.LeaseExpiresUnixMS {
		return nil
	}
	return c.journal.Update(op.ID, func(o *snapshotOperation) error { o.LeaseExpiresUnixMS = until.UnixMilli(); return nil })
}

func (c *snapshotController) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	for _, action := range c.actions {
		action.cancel()
	}
	c.mu.Unlock()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		c.transfer.client.CloseIdleConnections()
		return nil
	}
}
