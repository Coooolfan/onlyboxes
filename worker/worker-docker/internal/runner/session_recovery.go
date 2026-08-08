package runner

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/logging"
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
		if sessionID != "" && candidate.GetLeaseExpiresUnixMs() > 0 && leaseExpiresAt.After(now) {
			status = m.recoverOne(ctx, sessionID, name, leaseExpiresAt)
		} else if sessionID != "" {
			m.forceRemoveContainer(name)
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
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return registryv1.TerminalSessionRecoveryResult_INVALID
	}
	if existing := m.sessions[sessionID]; existing != nil && !existing.destroying {
		existing.leaseExpiresAt = leaseExpiresAt
		m.mu.Unlock()
		return registryv1.TerminalSessionRecoveryResult_RECOVERED
	}
	m.mu.Unlock()

	inspect := runDockerCommand(ctx,
		"inspect",
		"--format", "{{json .Config.Labels}}\t{{.State.Status}}",
		containerName,
	)
	if inspect.Err != nil || inspect.ExitCode != 0 {
		if inspect.Err == nil && isNoSuchContainerMessage(inspect.Stderr) {
			return registryv1.TerminalSessionRecoveryResult_MISSING
		}
		return registryv1.TerminalSessionRecoveryResult_MISSING
	}
	parts := strings.SplitN(strings.TrimSpace(inspect.Stdout), "\t", 2)
	if len(parts) != 2 {
		return registryv1.TerminalSessionRecoveryResult_INVALID
	}
	labels := map[string]string{}
	if err := json.Unmarshal([]byte(parts[0]), &labels); err != nil ||
		labels[terminalExecSessionLabelKey] != terminalSessionIDHash(sessionID) ||
		labels[terminalExecSchemaLabelKey] != terminalExecSchemaVersion {
		m.forceRemoveContainer(containerName)
		return registryv1.TerminalSessionRecoveryResult_INVALID
	}
	matching := runDockerCommand(ctx,
		"ps", "-a",
		"--filter", "label="+terminalExecSessionLabelKey+"="+terminalSessionIDHash(sessionID),
		"--filter", "label="+terminalExecSchemaLabelKey+"="+terminalExecSchemaVersion,
		"--format", "{{.Names}}",
	)
	if matching.Err != nil || matching.ExitCode != 0 {
		return registryv1.TerminalSessionRecoveryResult_INVALID
	}
	matchingNames := nonEmptyLines(matching.Stdout)
	if len(matchingNames) != 1 || matchingNames[0] != containerName {
		for _, name := range matchingNames {
			m.forceRemoveContainer(name)
		}
		return registryv1.TerminalSessionRecoveryResult_INVALID
	}

	switch strings.TrimSpace(strings.ToLower(parts[1])) {
	case "running":
	case "created", "exited", "stopped":
		start := runDockerCommand(ctx, terminalExecDockerStartArgs(containerName)...)
		if start.Err != nil || start.ExitCode != 0 {
			return registryv1.TerminalSessionRecoveryResult_INVALID
		}
	default:
		m.forceRemoveContainer(containerName)
		return registryv1.TerminalSessionRecoveryResult_INVALID
	}

	ready := make(chan struct{})
	close(ready)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return registryv1.TerminalSessionRecoveryResult_INVALID
	}
	if existing := m.sessions[sessionID]; existing != nil {
		if existing.containerName != containerName || existing.destroying {
			return registryv1.TerminalSessionRecoveryResult_INVALID
		}
		existing.leaseExpiresAt = leaseExpiresAt
		return registryv1.TerminalSessionRecoveryResult_RECOVERED
	}
	m.sessions[sessionID] = &terminalSession{
		sessionID:        sessionID,
		containerName:    containerName,
		leaseExpiresAt:   leaseExpiresAt,
		ready:            ready,
		capacityReserved: true,
	}
	m.activeSessionReservations++
	return registryv1.TerminalSessionRecoveryResult_RECOVERED
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
	listed := runDockerCommand(ctx,
		"ps", "-a",
		"--filter", "label="+terminalExecSessionLabelKey,
		"--filter", "label="+terminalExecSchemaLabelKey+"="+terminalExecSchemaVersion,
		"--format", "{{.Names}}",
	)
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
		if _, expected := expectedNames[name]; expected {
			continue
		}
		m.forceRemoveContainer(name)
		cleaned++
	}
	return discovered, cleaned
}
