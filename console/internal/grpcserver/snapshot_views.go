package grpcserver

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
)

// ManagedSessionView adds stable lifecycle metadata to transient runtime routes.
// Its fields are safe for management APIs; private restore specs stay in storage.
type ManagedSessionView struct {
	TerminalSessionView
	LifecycleState          sandboxsnapshot.State
	OperationID             string
	Runtime                 string
	Platform                string
	RetentionExpiresUnixMS  int64
	SnapshotID              string
	SnapshotCreatedAtUnixMS int64
	SnapshotSizeBytes       int64
	SnapshotSHA256          string
}

func (s *RegistryService) managedPersistentView(v sandboxsnapshot.SessionView, now time.Time) ManagedSessionView {
	view := ManagedSessionView{TerminalSessionView: TerminalSessionView{AccountID: v.OwnerID, SessionID: v.SessionID, LastUsedUnixMs: v.LastUsedUnixMS, CreatedAtUnixMs: v.CreatedAtUnixMS}, LifecycleState: v.State, OperationID: v.OperationID, Runtime: v.Runtime, Platform: v.Platform, RetentionExpiresUnixMS: v.RetentionExpiresUnixMS, SnapshotID: v.SnapshotID, SnapshotCreatedAtUnixMS: v.SnapshotCreatedAtUnixMS, SnapshotSizeBytes: v.SnapshotSizeBytes, SnapshotSHA256: v.SnapshotSHA256}
	if v.State == sandboxsnapshot.Running {
		view.WorkerID = v.WorkerID
		view.LeaseExpiresUnixMs = v.LeaseExpiresUnixMS
		view.Status = TerminalSessionStatusUnavailable
		if live, ok := s.GetTerminalSession(v.OwnerID, v.SessionID, now); ok && live.WorkerID == v.WorkerID {
			view.Status = live.Status
		}
		if v.OperationID != "" {
			view.Status = TerminalSessionStatusReconciling
		}
	}
	return view
}

func (s *RegistryService) ListManagedSessions(ctx context.Context, owner string, now time.Time) ([]ManagedSessionView, error) {
	var stable []sandboxsnapshot.SessionView
	var err error
	if s.snapshotRepository != nil {
		stable, err = s.snapshotRepository.SessionViews(ctx, owner)
		if err != nil {
			return nil, err
		}
	}
	seen := make(map[string]bool, len(stable))
	views := make([]ManagedSessionView, 0, len(stable))
	for _, v := range stable {
		seen[v.ScopedSessionID] = true
		if v.State != sandboxsnapshot.Deleting {
			views = append(views, s.managedPersistentView(v, now))
		}
	}
	for _, v := range s.ListTerminalSessions(owner, now) {
		if !seen[sandboxsnapshot.ScopeSessionID(v.AccountID, v.SessionID)] {
			views = append(views, ManagedSessionView{TerminalSessionView: v, LifecycleState: sandboxsnapshot.Running})
		}
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].CreatedAtUnixMs != views[j].CreatedAtUnixMs {
			return views[i].CreatedAtUnixMs > views[j].CreatedAtUnixMs
		}
		if views[i].AccountID != views[j].AccountID {
			return views[i].AccountID < views[j].AccountID
		}
		return views[i].SessionID < views[j].SessionID
	})
	return views, nil
}

func (s *RegistryService) GetManagedSession(ctx context.Context, owner, id string, now time.Time) (ManagedSessionView, error) {
	if s.snapshotRepository != nil {
		v, err := s.snapshotRepository.SessionView(ctx, owner, id)
		if err == nil {
			if v.State == sandboxsnapshot.Deleting {
				return ManagedSessionView{}, sandboxsnapshot.ErrNotFound
			}
			return s.managedPersistentView(v, now), nil
		}
		if !errors.Is(err, sandboxsnapshot.ErrNotFound) {
			return ManagedSessionView{}, err
		}
	}
	if v, ok := s.GetTerminalSession(owner, id, now); ok {
		return ManagedSessionView{TerminalSessionView: v, LifecycleState: sandboxsnapshot.Running}, nil
	}
	return ManagedSessionView{}, sandboxsnapshot.ErrNotFound
}

func (s *RegistryService) SnapshotOperation(ctx context.Context, owner, id, operation string) (sandboxsnapshot.Operation, error) {
	if s.snapshotRepository == nil {
		return sandboxsnapshot.Operation{}, sandboxsnapshot.ErrNotFound
	}
	// Logical deletion hides operations as well as the stable session.
	session, err := s.snapshotRepository.Session(ctx, owner, id)
	if err != nil {
		return sandboxsnapshot.Operation{}, err
	}
	if session.State == sandboxsnapshot.Deleting {
		return sandboxsnapshot.Operation{}, sandboxsnapshot.ErrNotFound
	}
	return s.snapshotRepository.Operation(ctx, owner, id, operation)
}

func (s *RegistryService) DeleteManagedSession(ctx context.Context, owner, id string, now time.Time) (bool, error) {
	s.persistenceDispatchMu.Lock()
	defer s.persistenceDispatchMu.Unlock()
	if s.snapshotRepository != nil {
		_, err := s.snapshotRepository.Session(ctx, owner, id)
		if err == nil {
			if err := s.snapshotRepository.DeleteSession(ctx, owner, id); err != nil {
				if errors.Is(err, sandboxsnapshot.ErrNotFound) {
					return false, nil
				}
				return false, err
			}
			scope := sandboxsnapshot.ScopeSessionID(owner, id)
			s.terminalRoutesMu.Lock()
			if route, ok := s.terminalSessionToNode[scope]; ok {
				s.deleteTerminalSessionRouteLocked(scope, route)
			}
			s.terminalRoutesMu.Unlock()
			return true, nil
		}
		if !errors.Is(err, sandboxsnapshot.ErrNotFound) {
			return false, err
		}
	}
	return s.DeleteTerminalSession(owner, id, now)
}

func (s *RegistryService) ExpireSnapshotSession(ctx context.Context, owner, id string) error {
	s.persistenceDispatchMu.Lock()
	defer s.persistenceDispatchMu.Unlock()
	if err := s.snapshotRepository.DeleteExpiredSession(ctx, owner, id); err != nil {
		return err
	}
	scope := sandboxsnapshot.ScopeSessionID(owner, id)
	s.terminalRoutesMu.Lock()
	defer s.terminalRoutesMu.Unlock()
	if route, ok := s.terminalSessionToNode[scope]; ok {
		s.deleteTerminalSessionRouteLocked(scope, route)
	}
	return nil
}

func (s *RegistryService) SnapshotCleanupStats(ctx context.Context, owner string) (sandboxsnapshot.CleanupStats, error) {
	if s.snapshotRepository == nil {
		return sandboxsnapshot.CleanupStats{}, ErrPersistenceUnavailable
	}
	return s.snapshotRepository.CleanupStats(ctx, owner)
}
