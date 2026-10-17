package grpcserver

import (
	"context"
	"errors"
	"sort"
	"sync"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
)

// RestorePlacement is an admission request, not permission to start execution.
// StagingBytes must budget both the archive and runtime import workspace. A
// cache hit may avoid downloading but never removes this space requirement.
type RestorePlacement struct {
	Spec              sandboxsnapshot.RestoreSpec
	StagingBytes      int64
	PreferredWorkerID string
	ExcludedWorkerIDs map[string]struct{}
}

// PersistenceReservation remains charged until the coordinator confirms local
// resources were released or ownership was handed over. A timed-out control must
// not automatically Release: its backend outcome may still be unknown.
type PersistenceReservation struct {
	mu       sync.Mutex
	service  *RegistryService
	session  *activeSession
	bytes    int64
	released bool
}

func (r *PersistenceReservation) WorkerID() string { return r.session.nodeID }

// Dispatch pins delivery to the exact connection that admitted the reservation.
// Reconnection requires reconciliation and a fresh reservation; it cannot turn
// a previously admitted restore into a new blind backend invocation.
func (r *PersistenceReservation) Dispatch(ctx context.Context, control *registryv1.SandboxPersistenceControl) (*registryv1.SandboxPersistenceReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released || r.service.getSession(r.session.nodeID) != r.session {
		return nil, &PersistenceControlError{Err: ErrNoCapabilityWorker}
	}
	return r.service.dispatchPersistenceControl(ctx, r.session, control)
}

func (r *PersistenceReservation) Release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released {
		return
	}
	r.session.persistence.mu.Lock()
	r.session.persistence.reservations--
	r.session.persistence.reservedBytes -= r.bytes
	r.session.persistence.mu.Unlock()
	r.released = true
}

func persistenceFormatSupported(formats []string, format string) bool {
	for _, candidate := range formats {
		if candidate == format {
			return true
		}
	}
	return false
}

func (p *persistenceSession) compatible(spec sandboxsnapshot.RestoreSpec) bool {
	c := p.capabilities
	// Initially require an exact backend version. Broader compatibility needs an
	// explicit tested matrix; a matching worker build or runtime name is not enough.
	return c.GetEnabled() && c.GetRuntime() == spec.Runtime && c.GetRuntimeVersion() == spec.RuntimeVersion && c.GetGuestPlatform() == spec.Platform && persistenceFormatSupported(c.GetReadFormats(), spec.Format)
}

// ReserveSnapshotRestore selects only compatible online sandbox workers. Local
// reservations close the gap before a worker acknowledges PREPARE_RESTORE and
// before its next heartbeat. Worker-local admission remains the final authority.
func (s *RegistryService) ReserveSnapshotRestore(request RestorePlacement) (*PersistenceReservation, error) {
	if err := request.Spec.Validate(); err != nil {
		return nil, err
	}
	if request.StagingBytes <= 0 {
		return nil, errors.New("restore staging space must be positive")
	}
	s.sessionsMu.RLock()
	candidates := make([]*activeSession, 0, len(s.sessions))
	for _, session := range s.sessions {
		candidates = append(candidates, session)
	}
	s.sessionsMu.RUnlock()
	// Original-node preference is optional, never a pin. Stable ordering makes
	// fallback predictable; each candidate still needs atomic capacity admission.
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i].nodeID, candidates[j].nodeID
		if (a == request.PreferredWorkerID) != (b == request.PreferredWorkerID) {
			return a == request.PreferredWorkerID
		}
		return a < b
	})
	hasBusyCandidate := false
	for _, session := range candidates {
		if isNodeExcluded(session.nodeID, request.ExcludedWorkerIDs) || !s.persistenceSessionOnline(session) || !session.hasCapability(taskCapabilityTerminalExec) {
			continue
		}
		p := &session.persistence
		p.mu.Lock()
		if p.closed || !p.compatible(request.Spec) {
			p.mu.Unlock()
			continue
		}
		capacity := session.terminalSessionCapacitySnapshot()
		if !capacity.known || (capacity.maxActiveSessions > 0 && int64(capacity.activeSessionCount)+int64(p.reservations) >= int64(capacity.maxActiveSessions)) || p.reservedBytes > p.availableBytes || request.StagingBytes > p.availableBytes-p.reservedBytes {
			p.mu.Unlock()
			continue
		}
		// Conservative accounting intentionally tolerates overlap between worker
		// reports and unacknowledged Console reservations. Neither may overbook.
		if int64(p.reportedInflight)+int64(p.reservations) >= int64(p.capabilities.GetMaxInflight()) {
			hasBusyCandidate = true
			p.mu.Unlock()
			continue
		}
		p.reservations++
		p.reservedBytes += request.StagingBytes
		p.mu.Unlock()
		return &PersistenceReservation{service: s, session: session, bytes: request.StagingBytes}, nil
	}
	if hasBusyCandidate {
		return nil, ErrNoWorkerCapacity
	}
	return nil, ErrNoCapabilityWorker
}
