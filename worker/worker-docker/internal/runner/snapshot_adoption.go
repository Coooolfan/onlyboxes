package runner

import (
	"context"
	"time"

	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

// Initial inspection preserves the existing CID and its current lease. It can
// attach a first guard record, but cannot revive revoked/expired ownership.
func (c *snapshotController) probeInitialSession(ctx context.Context, scope string) (wire.Result, error) {
	instance, err := c.runtime.Describe(ctx, terminalSessionResourceName(scope), scope)
	if err != nil {
		return wire.Result{}, err
	}
	if instance.State != "running" || instance.Paused {
		return wire.Result{}, errSnapshotJournalConflict
	}
	m := c.manager
	m.mu.Lock()
	local := m.sessions[scope]
	if m.closed || local == nil || local.destroying || !local.leaseExpiresAt.After(time.Now()) || (local.persistenceContainerID != "" && local.persistenceContainerID != instance.ID) {
		m.mu.Unlock()
		return wire.Result{}, errSnapshotJournalConflict
	}
	lease := local.leaseExpiresAt.UnixMilli()
	if err := c.journal.Probe(scope, instance.ID, lease); err != nil {
		m.mu.Unlock()
		return wire.Result{}, err
	}
	if m.persistenceBootstrap == nil {
		m.persistenceBootstrap = map[string]string{}
	}
	if m.persistenceBootstrapReady == nil {
		m.persistenceBootstrapReady = map[string]bool{}
	}
	m.persistenceBootstrap[scope] = instance.ID
	m.persistenceBootstrapReady[scope] = false
	local.persistenceContainerID = instance.ID
	m.mu.Unlock()
	op := snapshotOperation{SessionID: scope, ContainerID: instance.ID, Generation: 1, LeaseExpiresUnixMS: lease}
	if c.config.LeaseGuard != nil {
		if err := c.config.LeaseGuard.Adopt(ctx, op); err != nil {
			return wire.Result{}, err
		}
	}
	// Neither an old ready channel nor a successful lease grant proves that
	// this exact instance still runs after the asynchronous control call.
	confirmed, err := c.runtime.Describe(ctx, instance.ID, scope)
	if err != nil {
		return wire.Result{}, err
	}
	if confirmed.ID != instance.ID || confirmed.State != "running" || confirmed.Paused {
		return wire.Result{}, errSnapshotJournalConflict
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.sessions[scope] != local || local.destroying || local.leaseExpiresAt.UnixMilli() != lease || lease <= time.Now().UnixMilli() || m.persistenceBootstrap[scope] != instance.ID {
		return wire.Result{}, errSnapshotJournalConflict
	}
	m.persistenceBootstrapReady[scope] = true
	return wire.Result{State: "running", Spec: &instance.Spec, InstanceID: instance.ID, LeaseExpiresUnixMS: lease}, nil
}
