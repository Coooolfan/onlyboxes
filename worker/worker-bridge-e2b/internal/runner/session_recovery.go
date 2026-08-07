package runner

import (
	"context"
	"errors"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b"
	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/logging"
)

var recoverTerminalSessionsFn = func(
	_ context.Context,
	candidates []*registryv1.TerminalSessionRecoveryCandidate,
) []*registryv1.TerminalSessionRecoveryResult {
	return missingTerminalRecoveryResults(candidates)
}

func missingTerminalRecoveryResults(candidates []*registryv1.TerminalSessionRecoveryCandidate) []*registryv1.TerminalSessionRecoveryResult {
	results := make([]*registryv1.TerminalSessionRecoveryResult, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		results = append(results, &registryv1.TerminalSessionRecoveryResult{
			SessionId: candidate.GetSessionId(),
			Status:    registryv1.TerminalSessionRecoveryResult_MISSING,
		})
	}
	return results
}

func (m *terminalSessionManager) Recover(
	ctx context.Context,
	candidates []*registryv1.TerminalSessionRecoveryCandidate,
) []*registryv1.TerminalSessionRecoveryResult {
	startedAt := time.Now()
	recoveryBackend, ok := m.backend.(e2bRecoveryBackend)
	if !ok {
		return missingTerminalRecoveryResults(candidates)
	}
	results := make([]*registryv1.TerminalSessionRecoveryResult, 0, len(candidates))
	discovered := 0
	now := time.Now()
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		sessionID := strings.TrimSpace(candidate.GetSessionId())
		leaseExpiresAt := time.UnixMilli(candidate.GetLeaseExpiresUnixMs())
		status := registryv1.TerminalSessionRecoveryResult_INVALID
		if sessionID != "" && candidate.GetLeaseExpiresUnixMs() > 0 && leaseExpiresAt.After(now) {
			var found int
			status, found = m.recoverOne(ctx, recoveryBackend, sessionID, leaseExpiresAt)
			discovered += found
		}
		results = append(results, &registryv1.TerminalSessionRecoveryResult{SessionId: sessionID, Status: status})
	}
	counts := map[registryv1.TerminalSessionRecoveryResult_Status]int{}
	for _, result := range results {
		counts[result.GetStatus()]++
	}
	logging.Infof(
		"terminal recovery completed: executor_kind=e2b discovered=%d candidate=%d recovered=%d missing=%d invalid=%d orphan_cleaned=0 duration_ms=%d recovery_failures=%d",
		discovered,
		len(results),
		counts[registryv1.TerminalSessionRecoveryResult_RECOVERED],
		counts[registryv1.TerminalSessionRecoveryResult_MISSING],
		counts[registryv1.TerminalSessionRecoveryResult_INVALID],
		time.Since(startedAt).Milliseconds(),
		counts[registryv1.TerminalSessionRecoveryResult_INVALID],
	)
	return results
}

func (m *terminalSessionManager) recoverOne(
	ctx context.Context,
	backend e2bRecoveryBackend,
	sessionID string,
	leaseExpiresAt time.Time,
) (registryv1.TerminalSessionRecoveryResult_Status, int) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return registryv1.TerminalSessionRecoveryResult_INVALID, 0
	}
	if existing := m.sessions[sessionID]; existing != nil && !existing.destroying {
		existing.desiredLeaseExpiresAt = leaseExpiresAt
		existing.confirmedLeaseExpiresAt = leaseExpiresAt
		existing.remoteTimeoutExpiresAt = leaseExpiresAt
		m.mu.Unlock()
		return registryv1.TerminalSessionRecoveryResult_RECOVERED, 1
	}
	m.mu.Unlock()

	infos, err := backend.List(ctx, terminalSessionMetadata(sessionID))
	if err != nil {
		return registryv1.TerminalSessionRecoveryResult_INVALID, 0
	}
	if len(infos) == 0 {
		return registryv1.TerminalSessionRecoveryResult_MISSING, 0
	}
	matches := make([]e2b.SandboxInfo, 0, len(infos))
	for _, info := range infos {
		if info.Metadata[terminalSessionMetadataKey] == terminalSessionIDHash(sessionID) &&
			info.Metadata[terminalSessionSchemaKey] == terminalSessionSchemaVersion &&
			info.Metadata[terminalSessionWorkerMetadataKey] == terminalSessionWorkerMetadata {
			matches = append(matches, info)
		}
	}
	if len(matches) == 0 {
		return registryv1.TerminalSessionRecoveryResult_INVALID, len(infos)
	}
	if len(matches) != 1 {
		for _, info := range matches {
			if strings.TrimSpace(info.ID) != "" {
				_ = m.backend.Kill(ctx, info.ID)
			}
		}
		return registryv1.TerminalSessionRecoveryResult_INVALID, len(matches)
	}
	sandbox, err := backend.Connect(ctx, matches[0].ID, secondsUntil(leaseExpiresAt))
	if errors.Is(err, e2b.ErrSandboxNotFound) {
		return registryv1.TerminalSessionRecoveryResult_MISSING, 1
	}
	if err != nil || sandbox == nil || strings.TrimSpace(sandbox.ID) == "" {
		return registryv1.TerminalSessionRecoveryResult_INVALID, 1
	}

	ready := make(chan struct{})
	close(ready)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return registryv1.TerminalSessionRecoveryResult_INVALID, 1
	}
	if existing := m.sessions[sessionID]; existing != nil {
		if existing.sandbox == nil || existing.sandbox.ID != sandbox.ID || existing.destroying {
			return registryv1.TerminalSessionRecoveryResult_INVALID, 1
		}
		existing.desiredLeaseExpiresAt = leaseExpiresAt
		existing.confirmedLeaseExpiresAt = leaseExpiresAt
		existing.remoteTimeoutExpiresAt = leaseExpiresAt
		return registryv1.TerminalSessionRecoveryResult_RECOVERED, 1
	}
	m.sessions[sessionID] = &terminalSession{
		sessionID:               sessionID,
		sandbox:                 sandbox,
		desiredLeaseExpiresAt:   leaseExpiresAt,
		confirmedLeaseExpiresAt: leaseExpiresAt,
		remoteTimeoutExpiresAt:  leaseExpiresAt,
		capacityReserved:        true,
		ready:                   ready,
	}
	m.activeSessionReservations++
	return registryv1.TerminalSessionRecoveryResult_RECOVERED, 1
}
