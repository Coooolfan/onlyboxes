package runner

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/logging"
)

const (
	terminalRecoveryDockerMaxAttempts = 3
	terminalRecoveryDockerRetryDelay  = 100 * time.Millisecond
)

var recoverTerminalSessionsFn = func(
	_ context.Context,
	candidates []*registryv1.TerminalSessionRecoveryCandidate,
) []*registryv1.TerminalSessionRecoveryResult {
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
	results := make([]*registryv1.TerminalSessionRecoveryResult, 0, len(candidates))
	expectedNames := make(map[string]struct{}, len(candidates))
	now := time.Now()
	recoveredCount := 0
	missingCount := 0
	invalidCount := 0

	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		sessionID := strings.TrimSpace(candidate.GetSessionId())
		name := terminalSessionResourceName(sessionID)
		status := registryv1.TerminalSessionRecoveryResult_INVALID
		leaseExpiresAt := time.UnixMilli(candidate.GetLeaseExpiresUnixMs())
		m.mu.Lock()
		fence, known := m.persistenceFences[sessionID]
		protected := m.persistenceHeldLocked(sessionID) || (known && candidate.GetGeneration() != fence.generation) || (!known && candidate.GetGeneration() > 0)
		m.mu.Unlock()
		if protected {
			status = registryv1.TerminalSessionRecoveryResult_INVALID
		} else if sessionID != "" && candidate.GetLeaseExpiresUnixMs() > 0 && leaseExpiresAt.After(now) {
			status = m.recoverOne(ctx, sessionID, name, leaseExpiresAt)
		} else if sessionID != "" {
			resource := name
			if known {
				resource = fence.containerID
			}
			if resource != "" {
				m.forceRemoveContainer(resource)
			}
		}
		if known || candidate.GetGeneration() > 0 {
			expectedNames[name] = struct{}{}
		}
		switch status {
		case registryv1.TerminalSessionRecoveryResult_RECOVERED:
			expectedNames[name] = struct{}{}
			recoveredCount++
		case registryv1.TerminalSessionRecoveryResult_MISSING:
			missingCount++
		default:
			invalidCount++
		}
		results = append(results, &registryv1.TerminalSessionRecoveryResult{
			SessionId: sessionID,
			Status:    status,
		})
	}

	discovered, orphanCleaned := m.cleanupOrphanContainers(ctx, expectedNames)
	logging.Infof(
		"terminal recovery completed: executor_kind=docker discovered=%d candidate=%d recovered=%d missing=%d invalid=%d orphan_cleaned=%d duration_ms=%d recovery_failures=%d",
		discovered, len(results), recoveredCount, missingCount, invalidCount, orphanCleaned,
		time.Since(startedAt).Milliseconds(), invalidCount,
	)
	return results
}

func (m *terminalSessionManager) recoverOne(
	ctx context.Context,
	sessionID string,
	containerName string,
	leaseExpiresAt time.Time,
) registryv1.TerminalSessionRecoveryResult_Status {
	for attempt := 1; attempt <= terminalRecoveryDockerMaxAttempts; attempt++ {
		status, retryable := m.recoverOneAttempt(ctx, sessionID, containerName, leaseExpiresAt)
		if !retryable || attempt == terminalRecoveryDockerMaxAttempts {
			return status
		}
		logging.Warnf(
			"terminal recovery Docker check failed; retrying: session_id_hash=%s attempt=%d max_attempts=%d",
			terminalSessionIDHash(sessionID),
			attempt,
			terminalRecoveryDockerMaxAttempts,
		)
		if !waitTerminalRecoveryRetry(ctx, terminalRecoveryDockerRetryDelay) {
			return status
		}
	}
	return registryv1.TerminalSessionRecoveryResult_INVALID
}

func (m *terminalSessionManager) recoverOneAttempt(
	ctx context.Context,
	sessionID string,
	containerName string,
	leaseExpiresAt time.Time,
) (registryv1.TerminalSessionRecoveryResult_Status, bool) {
	m.mu.Lock()
	if m.closed || m.persistenceHeldLocked(sessionID) {
		m.mu.Unlock()
		return registryv1.TerminalSessionRecoveryResult_INVALID, false
	}
	fence, persistent := m.persistenceFences[sessionID]
	bootstrap := !persistent && m.persistenceBootstrap[sessionID] != ""
	if bootstrap {
		persistent = true
		fence = terminalPersistenceFence{containerID: m.persistenceBootstrap[sessionID], generation: 1, active: true}
	}
	if existing := m.sessions[sessionID]; existing != nil && !existing.destroying {
		if persistent && existing.persistenceContainerID != fence.containerID {
			m.mu.Unlock()
			return registryv1.TerminalSessionRecoveryResult_INVALID, false
		}
		if !bootstrap {
			existing.leaseExpiresAt = leaseExpiresAt
			m.scheduleSessionLeaseTimerLocked(existing)
			m.mu.Unlock()
			return registryv1.TerminalSessionRecoveryResult_RECOVERED, false
		}
	}
	m.mu.Unlock()

	inspect := runDockerCommand(ctx,
		"inspect",
		"--format", "{{json .Config.Labels}}\t{{.State.Status}}",
		containerName,
	)
	if inspect.Err != nil || inspect.ExitCode != 0 {
		if inspect.Err == nil && isNoSuchContainerMessage(inspect.Stderr) {
			return registryv1.TerminalSessionRecoveryResult_MISSING, false
		}
		return registryv1.TerminalSessionRecoveryResult_MISSING, true
	}
	parts := strings.SplitN(strings.TrimSpace(inspect.Stdout), "\t", 2)
	if len(parts) != 2 {
		return registryv1.TerminalSessionRecoveryResult_INVALID, false
	}
	labels := map[string]string{}
	if err := json.Unmarshal([]byte(parts[0]), &labels); err != nil ||
		labels[terminalExecSessionLabelKey] != terminalSessionIDHash(sessionID) ||
		labels[terminalExecSchemaLabelKey] != terminalExecSchemaVersion {
		if persistent {
			return registryv1.TerminalSessionRecoveryResult_INVALID, false
		}
		m.forceRemoveContainer(containerName)
		return registryv1.TerminalSessionRecoveryResult_INVALID, false
	}
	if persistent {
		labelGeneration := labels["onlyboxes.generation"]
		if labelGeneration != "" && labelGeneration != strconv.FormatInt(fence.generation, 10) || labelGeneration == "" && fence.generation != 1 {
			return registryv1.TerminalSessionRecoveryResult_INVALID, false
		}
		identity := runDockerCommand(ctx, "inspect", "--format", "{{.Id}}", containerName)
		if identity.Err != nil || identity.ExitCode != 0 {
			return registryv1.TerminalSessionRecoveryResult_INVALID, true
		}
		if fence.containerID == "" || strings.TrimSpace(identity.Stdout) != fence.containerID {
			return registryv1.TerminalSessionRecoveryResult_INVALID, false
		}
	}
	matching := runDockerCommand(ctx,
		"ps", "-a",
		"--filter", "label="+terminalExecSessionLabelKey+"="+terminalSessionIDHash(sessionID),
		"--filter", "label="+terminalExecSchemaLabelKey+"="+terminalExecSchemaVersion,
		"--format", "{{.Names}}",
	)
	if matching.Err != nil || matching.ExitCode != 0 {
		return registryv1.TerminalSessionRecoveryResult_INVALID, true
	}
	matchingNames := nonEmptyLines(matching.Stdout)
	if len(matchingNames) != 1 || matchingNames[0] != containerName {
		if persistent {
			return registryv1.TerminalSessionRecoveryResult_INVALID, false
		}
		for _, name := range matchingNames {
			m.forceRemoveContainer(name)
		}
		return registryv1.TerminalSessionRecoveryResult_INVALID, false
	}

	switch strings.TrimSpace(strings.ToLower(parts[1])) {
	case "running":
	case "created", "exited", "stopped":
		if persistent {
			return registryv1.TerminalSessionRecoveryResult_INVALID, false
		}
		start := runDockerCommand(ctx, terminalExecDockerStartArgs(containerName)...)
		if start.Err != nil || start.ExitCode != 0 {
			return registryv1.TerminalSessionRecoveryResult_INVALID, true
		}
	default:
		if persistent {
			return registryv1.TerminalSessionRecoveryResult_INVALID, false
		}
		m.forceRemoveContainer(containerName)
		return registryv1.TerminalSessionRecoveryResult_INVALID, false
	}
	containerIP := ""
	if m.dockerNetwork != "" {
		var err error
		resource := containerName
		if persistent {
			resource = fence.containerID
		}
		containerIP, err = inspectTerminalContainerIP(ctx, resource, m.dockerNetwork)
		if err != nil {
			return registryv1.TerminalSessionRecoveryResult_INVALID, true
		}
	}

	ready := make(chan struct{})
	close(ready)
	proxyCtx, proxyCancel := context.WithCancel(context.Background())
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		proxyCancel()
		return registryv1.TerminalSessionRecoveryResult_INVALID, false
	}
	if existing := m.sessions[sessionID]; existing != nil {
		proxyCancel()
		if existing.containerName != containerName || existing.destroying {
			return registryv1.TerminalSessionRecoveryResult_INVALID, false
		}
		existing.containerIP = containerIP
		if bootstrap && (existing.proxyCtx == nil || existing.proxyCtx.Err() != nil) {
			existing.proxyCtx, existing.proxyCancel = context.WithCancel(context.Background())
		}
		existing.leaseExpiresAt = leaseExpiresAt
		m.scheduleSessionLeaseTimerLocked(existing)
		return registryv1.TerminalSessionRecoveryResult_RECOVERED, false
	}
	session := &terminalSession{
		sessionID:              sessionID,
		containerName:          containerName,
		containerIP:            containerIP,
		leaseExpiresAt:         leaseExpiresAt,
		proxyCtx:               proxyCtx,
		proxyCancel:            proxyCancel,
		ready:                  ready,
		capacityReserved:       true,
		persistenceContainerID: fence.containerID,
	}
	m.sessions[sessionID] = session
	m.activeSessionReservations++
	m.scheduleSessionLeaseTimerLocked(session)
	return registryv1.TerminalSessionRecoveryResult_RECOVERED, false
}

func waitTerminalRecoveryRetry(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx == nil || ctx.Err() == nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nonEmptyLines(value string) []string {
	lines := make([]string, 0)
	for _, line := range strings.Split(value, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

func (m *terminalSessionManager) cleanupOrphanContainers(ctx context.Context, expectedNames map[string]struct{}) (int, int) {
	m.mu.Lock()
	failed := m.persistenceFailed
	m.mu.Unlock()
	if failed {
		return 0, 0
	}
	args := []string{"ps", "-a", "--filter", "label=" + terminalExecSessionLabelKey, "--filter", "label=" + terminalExecSchemaLabelKey + "=" + terminalExecSchemaVersion, "--format", "{{.Names}}"}
	if m.workerID != "" {
		args = append(args, "--filter", "label=onlyboxes.worker_id="+m.workerID)
	}
	listed := runDockerCommand(ctx, args...)
	if listed.Err != nil || listed.ExitCode != 0 {
		logging.Warnf("terminal recovery orphan discovery failed")
		return 0, 0
	}
	discovered := 0
	cleaned := 0
	for _, line := range strings.Split(listed.Stdout, "\n") {
		name := strings.TrimSpace(line)
		if !strings.HasPrefix(name, terminalExecContainerPrefix) {
			continue
		}
		discovered++
		m.mu.Lock()
		protected := false
		for session := range m.persistenceFences {
			if terminalSessionResourceName(session) == name {
				protected = true
				break
			}
		}
		for session := range m.persistenceBootstrap {
			if terminalSessionResourceName(session) == name {
				protected = true
				break
			}
		}
		for session := range m.persistenceRecoveryHolds {
			if terminalSessionResourceName(session) == name {
				protected = true
				break
			}
		}
		m.mu.Unlock()
		if protected {
			continue
		}
		if _, expected := expectedNames[name]; expected {
			continue
		}
		m.forceRemoveContainer(name)
		cleaned++
	}
	return discovered, cleaned
}
