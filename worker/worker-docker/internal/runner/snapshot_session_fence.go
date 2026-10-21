package runner

import (
	"context"
	"errors"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

type terminalPersistenceFence struct {
	operationID string
	generation  int64
	containerID string
	active      bool
	held        bool
	capacity    bool
}

// Called before ordinary recovery; the default worker cannot safely interpret
// lifecycle ownership. The lifecycle controller installs a journal-aware hook.
var recoverPersistenceSessionsFn = func(_ context.Context, ordinary []*registryv1.TerminalSessionRecoveryCandidate, held []*registryv1.SandboxPersistenceRecoveryCandidate) error {
	if len(held) > 0 {
		return errSnapshotJournalUnavailable
	}
	for _, candidate := range ordinary {
		if candidate.GetGeneration() > 0 {
			return errSnapshotJournalUnavailable
		}
	}
	return nil
}

func (m *terminalSessionManager) persistenceConflictLocked(sessionID string, generation int64) error {
	fence, exists := m.persistenceFences[sessionID]
	bootstrap := m.persistenceBootstrap[sessionID] != ""
	if m.persistenceFailed || ((exists || bootstrap) && m.persistenceDisconnected) || (!exists && bootstrap && m.persistenceGuardRequired && !m.persistenceBootstrapReady[sessionID]) || (exists && (!fence.active || fence.generation != generation)) || (!exists && generation != 0 && !(generation == 1 && m.persistenceBootstrap[sessionID] != "" && m.sessions[sessionID] != nil && m.sessions[sessionID].persistenceContainerID == m.persistenceBootstrap[sessionID])) {
		return newTerminalExecError("session_state_conflict", "sandbox lifecycle or generation does not allow execution")
	}
	return nil
}

func (m *terminalSessionManager) persistenceHeldLocked(sessionID string) bool {
	return m.persistenceFences[sessionID].held || m.persistenceFailed
}

// InstallSnapshotJournal is a startup integration primitive. It must complete
// before Hello/recovery; no user command can open a journal-owned inactive box.
func (m *terminalSessionManager) InstallSnapshotJournal(journal *snapshotJournal) error {
	state, err := journal.Snapshot()
	if err != nil {
		m.mu.Lock()
		m.persistenceFailed = true
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errSnapshotJournalUnavailable
	}
	for session, id := range state.Current {
		if local := m.sessions[session]; local != nil && local.persistenceContainerID != "" && state.Operations[id].ContainerID != "" && local.persistenceContainerID != state.Operations[id].ContainerID {
			m.persistenceFailed = true
			return errSnapshotJournalConflict
		}
	}
	priorBootstrap := m.persistenceBootstrap
	priorReady := m.persistenceBootstrapReady
	m.persistenceBootstrap = make(map[string]string, len(state.Probes))
	m.persistenceBootstrapReady = make(map[string]bool, len(state.Probes))
	for scope, id := range state.Probes {
		m.persistenceBootstrap[scope] = id
		m.persistenceBootstrapReady[scope] = !m.persistenceGuardRequired || (priorBootstrap[scope] == id && priorReady[scope])
	}
	m.persistenceFences = make(map[string]terminalPersistenceFence, len(state.Current))
	for session, id := range state.Current {
		op := state.Operations[id]
		if local := m.sessions[session]; local != nil && op.active() {
			local.leaseExpiresAt = time.UnixMilli(op.LeaseExpiresUnixMS)
		}
		m.setPersistenceFenceLocked(session, terminalPersistenceFence{operationID: op.ID, generation: op.Generation, containerID: op.ContainerID, active: op.active(), held: op.held(), capacity: !op.terminal()})
	}
	return nil
}

func (m *terminalSessionManager) setPersistenceFenceLocked(sessionID string, fence terminalPersistenceFence) {
	if m.persistenceFences == nil {
		m.persistenceFences = make(map[string]terminalPersistenceFence)
	}
	m.persistenceFences[sessionID] = fence
	if session := m.sessions[sessionID]; session != nil {
		if fence.containerID != "" {
			session.persistenceContainerID = fence.containerID
		}
		if !fence.active && session.proxyCancel != nil {
			session.proxyCancel()
		}
		if fence.held {
			m.stopSessionLeaseTimerLocked(session)
		} else if fence.active {
			if session.proxyCtx == nil || session.proxyCtx.Err() != nil {
				session.proxyCtx, session.proxyCancel = context.WithCancel(context.Background())
			}
			session.persistenceFault = false
			m.scheduleSessionLeaseTimerLocked(session)
		}
	}
}

// ProtectPersistenceRecovery consumes Console's authoritative ownership hints
// before ordinary orphan cleanup. Existing explicit activation/rollback wins
// over a same-operation hint captured just before Console finished that op.
func (m *terminalSessionManager) ProtectPersistenceRecovery(hints []*registryv1.SandboxPersistenceRecoveryCandidate) error {
	holds := make(map[string]bool, len(hints))
	for _, hint := range hints {
		if hint == nil || !validPersistenceScope(hint.GetScopedSessionId()) || !snapshotOperationID.MatchString(hint.GetOperationId()) || hint.GetGeneration() <= 0 || (hint.GetRole() != registryv1.SandboxPersistenceRecoveryCandidate_SOURCE && hint.GetRole() != registryv1.SandboxPersistenceRecoveryCandidate_TARGET) {
			return errSnapshotJournalConflict
		}
		if holds[hint.GetScopedSessionId()] {
			return errSnapshotJournalConflict
		}
		holds[hint.GetScopedSessionId()] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errSnapshotJournalUnavailable
	}
	for _, hint := range hints {
		if fence, known := m.persistenceFences[hint.GetScopedSessionId()]; known && fence.generation > hint.GetGeneration() {
			return errSnapshotJournalConflict
		}
	}
	m.persistenceRecoveryHolds = holds
	for _, hint := range hints {
		session := hint.GetScopedSessionId()
		fence, known := m.persistenceFences[session]
		if known && fence.generation > hint.GetGeneration() {
			return errSnapshotJournalConflict
		}
		if known && fence.operationID == hint.GetOperationId() && fence.generation == hint.GetGeneration() && fence.active {
			continue
		}
		m.setPersistenceFenceLocked(session, terminalPersistenceFence{operationID: hint.GetOperationId(), generation: hint.GetGeneration(), containerID: fence.containerID, held: true, capacity: true})
	}
	return nil
}

// HoldSource closes admission atomically against lease/destruction checks. The
// caller persists the journal claim before any backend operation. A failed
// journal write must leave the gate closed and the source protected.
func (m *terminalSessionManager) HoldSource(sessionID, operationID string, generation int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.persistenceFailed || !validPersistenceScope(sessionID) || !snapshotOperationID.MatchString(operationID) || generation <= 0 {
		return errSnapshotJournalConflict
	}
	prior, known := m.persistenceFences[sessionID]
	if known && ((prior.generation != generation) || (!prior.active && prior.operationID != operationID)) {
		return errSnapshotJournalConflict
	}
	session := m.sessions[sessionID]
	if session == nil || session.destroying || (!session.leaseExpiresAt.After(time.Now()) && !(known && prior.held && prior.operationID == operationID)) {
		return errSnapshotJournalConflict
	}
	m.setPersistenceFenceLocked(sessionID, terminalPersistenceFence{operationID: operationID, generation: generation, containerID: session.persistenceContainerID, held: true, capacity: true})
	return nil
}

func (m *terminalSessionManager) DrainPersistence(ctx context.Context, sessionID, operationID string, generation int64) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		fence, known := m.persistenceFences[sessionID]
		session := m.sessions[sessionID]
		valid := known && fence.operationID == operationID && fence.generation == generation && fence.held && session != nil && !m.closed && !m.persistenceFailed
		drained := valid && session.inflight == 0
		fault := valid && session.persistenceFault
		m.mu.Unlock()
		if !valid {
			return errSnapshotJournalConflict
		}
		if fault {
			return errors.New("accepted command failed while draining snapshot source")
		}
		if drained {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *terminalSessionManager) activeSessionCountLocked() int {
	count := m.activeSessionReservations
	for id, fence := range m.persistenceFences {
		if fence.capacity && (m.sessions[id] == nil || !m.sessions[id].capacityReserved) {
			count++
		}
	}
	return count
}
