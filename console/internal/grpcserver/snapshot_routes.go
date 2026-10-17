package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
)

const sessionStateConflictCode = "session_state_conflict"

func sessionStateConflictError() error {
	return &CommandExecutionError{Code: sessionStateConflictCode, Message: "session lifecycle does not allow execution"}
}

// checkPersistentExecution is called under persistenceDispatchMu when admitting
// commands. Durable lifecycle ownership is authoritative even if route caches
// have not yet caught up with a completed database transaction.
func (s *RegistryService) checkPersistentExecution(ctx context.Context, scopedID, expectedWorker string) (int64, error) {
	if s.snapshotRepository == nil || scopedID == "" {
		return 0, nil
	}
	session, err := s.snapshotRepository.SessionByScope(ctx, scopedID)
	if errors.Is(err, sandboxsnapshot.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read persistent session gate: %w", err)
	}
	if session.State != sandboxsnapshot.Running || session.OperationID != "" || (expectedWorker != "" && session.WorkerID != expectedWorker) {
		return session.Generation, sessionStateConflictError()
	}
	route, err := s.snapshotRepository.ExecutionRoute(ctx, scopedID)
	if errors.Is(err, sandboxsnapshot.ErrNotFound) {
		return session.Generation, terminalSessionUnavailableError()
	}
	if err != nil {
		return session.Generation, err
	}
	if route.WorkerID != session.WorkerID || route.LeaseExpiresUnixMS <= s.nowFn().UnixMilli() {
		return session.Generation, terminalSessionUnavailableError()
	}
	return session.Generation, nil
}

func bindPersistentCommand(payload []byte, generation int64, capability string) ([]byte, error) {
	if generation == 0 {
		return payload, nil
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(payload, &value); err != nil || value == nil {
		return nil, errors.New("persistent command payload must be an object")
	}
	value["generation"] = json.RawMessage(strconv.FormatInt(generation, 10))
	if capability == taskCapabilityTerminalExec {
		// A persistent sandbox can never silently turn into an empty instance if
		// the restored runtime or route disappears after admission.
		value["create_if_missing"] = json.RawMessage("false")
	}
	return json.Marshal(value)
}

func (s *RegistryService) BeginSnapshotOperation(ctx context.Context, request sandboxsnapshot.BeginRequest) (sandboxsnapshot.Operation, error) {
	s.persistenceDispatchMu.Lock()
	defer s.persistenceDispatchMu.Unlock()
	if s.snapshotRepository == nil {
		return sandboxsnapshot.Operation{}, errors.New("snapshot repository unavailable")
	}
	return s.snapshotRepository.Begin(ctx, request)
}

func (s *RegistryService) AdoptSnapshotSession(ctx context.Context, owner, id, worker string, spec sandboxsnapshot.RestoreSpec) error {
	s.persistenceDispatchMu.Lock()
	defer s.persistenceDispatchMu.Unlock()
	if s.snapshotRepository == nil {
		return errors.New("snapshot repository unavailable")
	}
	return s.snapshotRepository.AdoptRunning(ctx, owner, id, worker, spec)
}

// RefreshSnapshotRoute copies a committed route into the transient index. The
// persistent gate remains closed until the activation acknowledgement clears
// operation ownership in Repository.Finish.
func (s *RegistryService) RefreshSnapshotRoute(ctx context.Context, owner, id string) error {
	s.persistenceDispatchMu.Lock()
	defer s.persistenceDispatchMu.Unlock()
	session, err := s.snapshotRepository.Session(ctx, owner, id)
	if err != nil {
		return err
	}
	route, err := s.snapshotRepository.ExecutionRoute(ctx, session.ScopedSessionID)
	if err != nil && !errors.Is(err, sandboxsnapshot.ErrNotFound) {
		return err
	}
	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()
	if old, ok := s.terminalSessionToNode[session.ScopedSessionID]; ok {
		s.deleteTerminalSessionRouteLocked(session.ScopedSessionID, old)
	}
	if errors.Is(err, sandboxsnapshot.ErrNotFound) || session.State != sandboxsnapshot.Running || route.LeaseExpiresUnixMS <= s.nowFn().UnixMilli() {
		return nil
	}
	s.terminalSessionToNode[session.ScopedSessionID] = terminalSessionRoute{NodeID: route.WorkerID, LeaseExpiresUnixMs: route.LeaseExpiresUnixMS, LastUsedUnixMs: route.LastUsedUnixMS, CreatedAtUnixMs: route.CreatedAtUnixMS, RecoveryState: terminalSessionRecoveryReady}
	if s.terminalNodeToSessionIDIndex[route.WorkerID] == nil {
		s.terminalNodeToSessionIDIndex[route.WorkerID] = make(map[string]struct{})
	}
	s.terminalNodeToSessionIDIndex[route.WorkerID][session.ScopedSessionID] = struct{}{}
	return nil
}

// Existing preview URLs belong to stable sessions, so route cache invalidation
// during suspend, lease pruning or worker recovery must not revoke those URLs.
func (s *RegistryService) preserveSnapshotPreviews(scopedID string) bool {
	if s.snapshotRepository == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), terminalRouteStoreTimeout)
	defer cancel()
	session, err := s.snapshotRepository.SessionByScope(ctx, scopedID)
	// A database error cannot prove that a stable session was deleted. Its
	// authorization gate will remain unavailable until the database is readable.
	return (err == nil && session.State != sandboxsnapshot.Deleting) || (err != nil && !errors.Is(err, sandboxsnapshot.ErrNotFound))
}

func (s *RegistryService) enqueueFencedCommand(ctx context.Context, capability, scopedID string, session *activeSession, payload []byte, enqueue func(context.Context, []byte) error) (int64, error) {
	if scopedID == "" {
		return 0, enqueue(ctx, payload)
	}
	s.persistenceDispatchMu.RLock()
	generation, err := s.checkPersistentExecution(ctx, scopedID, session.nodeID)
	if err != nil {
		s.persistenceDispatchMu.RUnlock()
		return generation, err
	}
	if generation == 0 {
		s.persistenceDispatchMu.RUnlock()
		return 0, enqueue(ctx, payload)
	}
	defer s.persistenceDispatchMu.RUnlock()
	payload, err = bindPersistentCommand(payload, generation, capability)
	if err != nil {
		return generation, err
	}
	// Bound time spent holding lifecycle admission while a worker stream is
	// congested. Suspend can acquire ownership after already-admitted deliveries.
	admissionCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	err = enqueue(admissionCtx, payload)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return generation, ErrNoWorkerCapacity
	}
	return generation, err
}
