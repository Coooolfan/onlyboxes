package runner

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

// purgeSession is authorized only after Console has tombstoned the stable
// session and advanced generation. It writes that fence before native removal.
func (c *snapshotController) purgeSession(ctx context.Context, control *registryv1.SandboxPersistenceControl, p wire.Control, state snapshotJournalState) (wire.Result, error) {
	if control.Generation <= 1 || p.Rollback || p.Delete {
		return wire.Result{}, errSnapshotJournalConflict
	}
	current := state.Operations[state.Current[control.ScopedSessionId]]
	validSpec := p.Spec != nil && p.Spec.Validate() == nil && p.Spec.Runtime == "docker"
	if current.ID == control.OperationId {
		if current.Kind != "delete" || (p.Spec == nil && (current.Phase != "deleted" || current.SpecSHA256 == "")) || (p.Spec != nil && (!validSpec || !current.matchesSpec(*p.Spec))) {
			return wire.Result{}, errSnapshotJournalConflict
		}

	} else {
		if !validSpec || (current.ID != "" && (current.Generation >= control.Generation || (current.Kind != "cancel" && !current.matchesSpec(*p.Spec)))) {
			return wire.Result{}, errSnapshotJournalConflict
		}
		id := current.ContainerID
		if id == "" {
			id = state.Probes[control.ScopedSessionId]
		}
		found, err := snapshotContainerID(ctx, control.ScopedSessionId)
		if err != nil {
			return wire.Result{}, err
		}

		markerID := ""
		if found != "" {
			var native dockerSnapshotContainer
			if err := dockerSnapshotJSON(ctx, &native, "inspect", "--format", "{{json .}}", found); err != nil {
				return wire.Result{}, err
			}
			if deletionMarker(native, control.ScopedSessionId, control.OperationId, control.Generation) {
				markerID = found
			} else {
				if id != "" && found != id {
					return wire.Result{}, errSnapshotJournalConflict
				}
				if err := c.checkPurgeIdentity(ctx, found, control.ScopedSessionId, control.Generation); err != nil {
					return wire.Result{}, err
				}
				id = found
			}
		}
		current = snapshotOperation{ID: control.OperationId, SessionID: control.ScopedSessionId, Generation: control.Generation, Kind: "delete", Phase: "deleting", Spec: *p.Spec, ContainerID: id, TombstoneID: markerID}
		if err := c.journal.Claim(current); err != nil {
			return wire.Result{}, err
		}
		if err := c.manager.InstallSnapshotJournal(c.journal); err != nil {
			return wire.Result{}, err
		}
	}
	if current.ContainerID != "" {
		// Remove uses an immutable ID and confirms an already-missing instance.
		if err := c.runtime.Remove(ctx, current.ContainerID); err != nil {
			return wire.Result{}, err
		}
	}
	if err := c.forgetExecutionLease(ctx, current); err != nil {
		return wire.Result{}, err
	}
	if err := c.ensureDeletionNameFence(ctx, &current); err != nil {
		return wire.Result{}, err
	}
	c.detachDeletedScope(current.SessionID)
	// Refresh after admission: current operation metadata and all old artifacts
	// stay durable until their individual removals have succeeded.
	state, err := c.journal.Snapshot()
	if err != nil {
		return wire.Result{}, err
	}
	for _, op := range state.Operations {
		if op.SessionID != current.SessionID || op.Generation >= current.Generation {
			continue
		}
		if op.Kind == "source" && op.ContainerID != "" {
			if err := removeSnapshotExportHelper(ctx, op.ContainerID, op.ID); err != nil {
				return wire.Result{}, err
			}
		}
		entries, err := os.ReadDir(c.artifact(""))
		if err != nil {
			return wire.Result{}, err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), snapshotExportTempPrefix(op.ID)) && strings.HasSuffix(entry.Name(), ".tar") {
				if err := os.Remove(c.artifact(entry.Name())); err != nil && !os.IsNotExist(err) {
					return wire.Result{}, err
				}
			}
		}
		if op.Kind == "target" && op.Archive != nil {
			if err := c.removeImportTag(ctx, op); err != nil {
				return wire.Result{}, err
			}
		}
		if op.Artifact != "" {
			c.mu.Lock()
			pinned := c.pins[op.Artifact] > 0
			c.mu.Unlock()
			if pinned {
				return wire.Result{}, errSnapshotTransferRetry
			}
			for _, name := range []string{op.Artifact, op.Artifact + ".partial"} {
				if err := os.Remove(c.artifact(name)); err != nil && !os.IsNotExist(err) {
					return wire.Result{}, err
				}
			}
		}
	}
	if err := c.update(current.ID, func(o *snapshotOperation) error { o.Phase = "deleted"; return nil }); err != nil {
		return wire.Result{}, err
	}
	return wire.Result{State: "deleted"}, nil
}
func (c *snapshotController) checkPurgeIdentity(ctx context.Context, id, scope string, deletedGeneration int64) error {
	var container dockerSnapshotContainer
	if err := dockerSnapshotJSON(ctx, &container, "inspect", "--format", "{{json .}}", id); err != nil {
		return err
	}
	labels := container.Config.Labels
	if labels[terminalExecSessionLabelKey] != terminalSessionIDHash(scope) || labels[terminalExecSchemaLabelKey] != terminalExecSchemaVersion || labels["onlyboxes.managed"] != "true" {
		return errSnapshotJournalConflict
	}
	if worker := labels["onlyboxes.worker_id"]; worker != "" && worker != c.workerID {
		return errSnapshotJournalConflict
	}
	generation := int64(1)
	if value := labels["onlyboxes.generation"]; value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return errSnapshotJournalConflict
		}
		generation = parsed
	}
	if generation < 1 || generation >= deletedGeneration {
		return errSnapshotJournalConflict
	}
	return nil
}

type snapshotImportedImage struct {
	ID       string `json:"Id"`
	RepoTags []string
	Config   struct{ Labels map[string]string }
}

func (c *snapshotController) removeImportTag(ctx context.Context, op snapshotOperation) error {
	tag := c.imageTag(op)
	var listed bytesBuffer
	if err := dockerSnapshotCLI(ctx, nil, &listed, "image", "ls", "--no-trunc", "--filter", "reference="+tag, "--format", "{{.ID}}"); err != nil {
		return err
	}
	id := strings.TrimSpace(listed.String())
	if id != "" {
		var image snapshotImportedImage
		if err := dockerSnapshotJSON(ctx, &image, "image", "inspect", "--format", "{{json .}}", id); err != nil {
			return err
		}
		if image.ID != id || image.Config.Labels["onlyboxes.snapshot.sha256"] != op.Archive.SHA256 || (id != op.ImageID && image.Config.Labels["onlyboxes.snapshot.import_tag"] != tag) {
			return errSnapshotJournalConflict
		}
		// Remove only the operation's tag. Another container or tag prevents forced
		// reclamation; it must never be removed merely because its archive matches.
		if err := dockerSnapshotCLI(ctx, nil, io.Discard, "image", "rm", tag); err != nil {
			return err
		}
	}
	// Repeated, ambiguously acknowledged imports can leave earlier images
	// dangling after the same tag moves. Immutable operation labels identify them.
	var dangling bytesBuffer
	if err := dockerSnapshotCLI(ctx, nil, &dangling, "image", "ls", "--all", "--no-trunc", "--filter", "label=onlyboxes.snapshot.import_tag="+tag, "--format", "{{.ID}}"); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, imageID := range strings.Fields(dangling.String()) {
		if seen[imageID] {
			continue
		}
		seen[imageID] = true
		var image snapshotImportedImage
		if err := dockerSnapshotJSON(ctx, &image, "image", "inspect", "--format", "{{json .}}", imageID); err != nil {
			return err
		}
		if image.ID != imageID || image.Config.Labels["onlyboxes.snapshot.sha256"] != op.Archive.SHA256 || image.Config.Labels["onlyboxes.snapshot.import_tag"] != tag {
			return errSnapshotJournalConflict
		}
		if err := c.removeDanglingImport(ctx, tag, image); err != nil {
			return err
		}
	}
	return nil
}

// Delete by our own temporary reference, never by image ID: another actor can
// add a foreign tag between inspection and deletion. The engine then retains
// that reference atomically when it removes ours. The deterministic GC tag and
// immutable operation label also make a crash after tagging discoverable.
func (c *snapshotController) removeDanglingImport(ctx context.Context, operationTag string, image snapshotImportedImage) error {
	gcTag := strings.TrimSuffix(operationTag, ":rootfs") + ":gc-" + strings.TrimPrefix(image.ID, "sha256:")
	claimed, foreign := false, false
	for _, tag := range image.RepoTags {
		if tag == gcTag {
			claimed = true
		} else {
			foreign = true
		}
	}
	if foreign && !claimed {
		return nil
	}
	if !claimed {
		var listed bytesBuffer
		if err := dockerSnapshotCLI(ctx, nil, &listed, "image", "ls", "--no-trunc", "--filter", "reference="+gcTag, "--format", "{{.ID}}"); err != nil {
			return err
		}
		if id := strings.TrimSpace(listed.String()); id != "" && id != image.ID {
			return errSnapshotJournalConflict
		}
		if err := dockerSnapshotCLI(ctx, nil, io.Discard, "image", "tag", image.ID, gcTag); err != nil {
			return err
		}
	}
	return dockerSnapshotCLI(ctx, nil, io.Discard, "image", "rm", gcTag)
}
func (c *snapshotController) detachDeletedScope(scope string) {
	m := c.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if local := m.sessions[scope]; local != nil {
		m.stopSessionLeaseTimerLocked(local)
		if local.proxyCancel != nil {
			local.proxyCancel()
		}
		delete(m.sessions, scope)
		if local.capacityReserved {
			m.activeSessionReservations--
			local.capacityReserved = false
		}
	}
}

// A stopped, read-only sentinel reserves the native name after rootfs removal.
// Docker requests already accepted when a client disconnected can finish late;
// they must encounter a reserved name rather than recreate a runnable sandbox.
func (c *snapshotController) ensureDeletionNameFence(ctx context.Context, op *snapshotOperation) error {
	for attempts := 0; attempts < 3; attempts++ {
		id, err := snapshotContainerID(ctx, op.SessionID)
		if err != nil {
			return err
		}
		if id != "" {
			var native dockerSnapshotContainer
			if err := dockerSnapshotJSON(ctx, &native, "inspect", "--format", "{{json .}}", id); err != nil {
				return err
			}
			if deletionMarker(native, op.SessionID, op.ID, op.Generation) {
				if (op.TombstoneID != "" && id != op.TombstoneID) || !native.HostConfig.ReadonlyRootfs {
					return errSnapshotJournalConflict
				}
				// A marker is never a user instance, even if an administrator started it.
				if native.State.Running {
					if err := dockerSnapshotCLI(ctx, nil, io.Discard, "stop", "--signal", "SIGKILL", "--timeout", "0", id); err != nil {
						return err
					}
				}
				if op.TombstoneID == "" {
					if err := c.update(op.ID, func(o *snapshotOperation) error { o.TombstoneID = id; return nil }); err != nil {
						return err
					}
					op.TombstoneID = id
				}
				return nil
			}
			if op.TombstoneID != "" {
				return errSnapshotJournalConflict
			}
			if err := c.checkPurgeIdentity(ctx, id, op.SessionID, op.Generation); err != nil {
				return err
			}
			if err := c.runtime.Remove(ctx, id); err != nil {
				return err
			}
			continue
		}
		if op.TombstoneID != "" {
			return errSnapshotJournalConflict
		}
		// No start action is ever issued for this control resource. Its rootfs is
		// the administrator's immutable helper, with no sandbox archive or mounts.
		args := []string{"create", "--name", terminalSessionResourceName(op.SessionID), "--read-only", "--network", "none", "--memory", "16m", "--pids-limit", "1", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--label", "onlyboxes.managed=true", "--label", terminalExecSchemaLabelKey + "=" + terminalExecSchemaVersion, "--label", terminalExecSessionLabelKey + "=" + terminalSessionIDHash(op.SessionID), "--label", "onlyboxes.worker_id=" + c.workerID, "--label", "onlyboxes.persistence=true", "--label", "onlyboxes.persistence.deleted=true", "--label", "onlyboxes.persistence.operation_id=" + op.ID, "--label", "onlyboxes.generation=" + strconv.FormatInt(op.Generation, 10), "--entrypoint", "/bin/false", c.runtime.helperImage}
		// An ambiguous creation result is resolved by inspecting the same name.
		_ = dockerSnapshotCLI(ctx, nil, io.Discard, args...)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return errSnapshotTransferRetry
}

func deletionMarker(native dockerSnapshotContainer, scope, operation string, generation int64) bool {
	labels := native.Config.Labels
	return labels["onlyboxes.persistence.deleted"] == "true" && labels["onlyboxes.persistence.operation_id"] == operation && labels["onlyboxes.generation"] == strconv.FormatInt(generation, 10) && labels["onlyboxes.worker_id"] != "" && labels[terminalExecSessionLabelKey] == terminalSessionIDHash(scope) && labels[terminalExecSchemaLabelKey] == terminalExecSchemaVersion && labels["onlyboxes.managed"] == "true" && native.HostConfig.ReadonlyRootfs && len(native.Mounts) == 0
}
