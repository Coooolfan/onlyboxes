package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	terminalExecCapabilityName     = "terminalexec"
	terminalExecCapabilityDeclared = "terminalExec"
	terminalExecContainerPrefix    = "onlyboxes-terminalexec-"
	terminalExecCapabilityLabel    = "onlyboxes.capability=terminalExec"
	terminalExecIdleCommand        = "while true; do sleep 3600; done"
	terminalExecCleanupTimeout     = 3 * time.Second
	terminalExecJanitorInterval    = 5 * time.Second
	terminalExecNoSessionMessage   = "session not found"
	terminalExecBusyMessage        = "session is busy"
	terminalExecNotReadyMessage    = "terminal executor is unavailable"
)

const (
	terminalExecCodeSessionNotFound = "session_not_found"
	terminalExecCodeSessionBusy     = "session_busy"
	terminalExecCodeInvalidPayload  = "invalid_payload"
)

type terminalExecPayload struct {
	Command         string `json:"command"`
	SessionID       string `json:"session_id,omitempty"`
	CreateIfMissing bool   `json:"create_if_missing,omitempty"`
	LeaseTTLSec     *int   `json:"lease_ttl_sec,omitempty"`
}

type terminalExecRequest struct {
	Command         string
	SessionID       string
	CreateIfMissing bool
	LeaseTTLSec     *int
}

type terminalExecRunResult struct {
	SessionID          string `json:"session_id"`
	Created            bool   `json:"created"`
	Stdout             string `json:"stdout"`
	Stderr             string `json:"stderr"`
	ExitCode           int    `json:"exit_code"`
	StdoutTruncated    bool   `json:"stdout_truncated"`
	StderrTruncated    bool   `json:"stderr_truncated"`
	LeaseExpiresUnixMS int64  `json:"lease_expires_unix_ms"`
}

type terminalExecError struct {
	code    string
	message string
}

func (e *terminalExecError) Error() string {
	if e == nil {
		return "terminal execution failed"
	}
	return e.message
}

func (e *terminalExecError) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func newTerminalExecError(code string, message string) *terminalExecError {
	return &terminalExecError{
		code:    strings.TrimSpace(code),
		message: strings.TrimSpace(message),
	}
}

type terminalSession struct {
	sessionID      string
	containerName  string
	leaseExpiresAt time.Time
	busy           bool
}

type terminalSessionManagerConfig struct {
	LeaseMinSec      int
	LeaseMaxSec      int
	LeaseDefaultSec  int
	OutputLimitBytes int
	DockerImage      string
	MemoryLimit      string
	CPULimit         string
	PidsLimit        int
}

type terminalSessionManager struct {
	mu       sync.Mutex
	sessions map[string]*terminalSession

	leaseMinSec      int
	leaseMaxSec      int
	leaseDefaultSec  int
	outputLimitBytes int
	dockerImage      string
	memoryLimit      string
	cpuLimit         string
	pidsLimit        int

	stopCh    chan struct{}
	doneCh    chan struct{}
	closeOnce sync.Once
}

func newTerminalSessionManager(cfg terminalSessionManagerConfig) *terminalSessionManager {
	leaseMinSec := cfg.LeaseMinSec
	if leaseMinSec <= 0 {
		leaseMinSec = 60
	}

	leaseMaxSec := cfg.LeaseMaxSec
	if leaseMaxSec <= 0 {
		leaseMaxSec = 1800
	}
	if leaseMaxSec < leaseMinSec {
		leaseMaxSec = leaseMinSec
	}

	leaseDefaultSec := cfg.LeaseDefaultSec
	if leaseDefaultSec <= 0 {
		leaseDefaultSec = 60
	}
	if leaseDefaultSec < leaseMinSec {
		leaseDefaultSec = leaseMinSec
	}
	if leaseDefaultSec > leaseMaxSec {
		leaseDefaultSec = leaseMaxSec
	}

	outputLimitBytes := cfg.OutputLimitBytes
	if outputLimitBytes <= 0 {
		outputLimitBytes = 1024 * 1024
	}

	dockerImage := strings.TrimSpace(cfg.DockerImage)
	if dockerImage == "" {
		dockerImage = defaultTerminalExecDockerImage
	}

	memoryLimit := strings.TrimSpace(cfg.MemoryLimit)
	if memoryLimit == "" {
		memoryLimit = defaultTerminalExecMemoryLimit
	}
	cpuLimit := strings.TrimSpace(cfg.CPULimit)
	if cpuLimit == "" {
		cpuLimit = defaultTerminalExecCPULimit
	}
	pidsLimit := cfg.PidsLimit
	if pidsLimit <= 0 {
		pidsLimit = defaultTerminalExecPidsLimit
	}

	manager := &terminalSessionManager{
		sessions:         make(map[string]*terminalSession),
		leaseMinSec:      leaseMinSec,
		leaseMaxSec:      leaseMaxSec,
		leaseDefaultSec:  leaseDefaultSec,
		outputLimitBytes: outputLimitBytes,
		dockerImage:      dockerImage,
		memoryLimit:      memoryLimit,
		cpuLimit:         cpuLimit,
		pidsLimit:        pidsLimit,
		stopCh:           make(chan struct{}),
		doneCh:           make(chan struct{}),
	}
	go manager.janitorLoop()
	return manager
}

func (m *terminalSessionManager) Close() {
	if m == nil {
		return
	}

	m.closeOnce.Do(func() {
		close(m.stopCh)
		<-m.doneCh

		m.mu.Lock()
		sessions := make([]*terminalSession, 0, len(m.sessions))
		for _, session := range m.sessions {
			if session == nil {
				continue
			}
			sessions = append(sessions, session)
		}
		m.sessions = make(map[string]*terminalSession)
		m.mu.Unlock()

		for _, session := range sessions {
			m.forceRemoveContainer(session.containerName)
		}
	})
}

func (m *terminalSessionManager) Execute(ctx context.Context, req terminalExecRequest) (terminalExecRunResult, error) {
	if m == nil {
		return terminalExecRunResult{}, newTerminalExecError("execution_failed", terminalExecNotReadyMessage)
	}

	command := strings.TrimSpace(req.Command)
	if command == "" {
		return terminalExecRunResult{}, newTerminalExecError(terminalExecCodeInvalidPayload, "command is required")
	}

	leaseDuration, err := m.resolveLeaseDuration(req.LeaseTTLSec)
	if err != nil {
		return terminalExecRunResult{}, err
	}

	now := time.Now()
	leaseTarget := now.Add(leaseDuration)
	sessionID := strings.TrimSpace(req.SessionID)

	var (
		created bool
		session *terminalSession
	)

	if sessionID == "" {
		sessionID = uuid.NewString()
		containerName, allocErr := newTerminalExecContainerName()
		if allocErr != nil {
			return terminalExecRunResult{}, fmt.Errorf("allocate terminal container name: %w", allocErr)
		}
		session = &terminalSession{
			sessionID:      sessionID,
			containerName:  containerName,
			leaseExpiresAt: leaseTarget,
			busy:           true,
		}
		created = true

		m.mu.Lock()
		m.sessions[sessionID] = session
		m.mu.Unlock()
	} else {
		m.mu.Lock()
		existing, ok := m.sessions[sessionID]
		if !ok {
			if !req.CreateIfMissing {
				m.mu.Unlock()
				return terminalExecRunResult{}, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
			}

			containerName, allocErr := newTerminalExecContainerName()
			if allocErr != nil {
				m.mu.Unlock()
				return terminalExecRunResult{}, fmt.Errorf("allocate terminal container name: %w", allocErr)
			}
			existing = &terminalSession{
				sessionID:      sessionID,
				containerName:  containerName,
				leaseExpiresAt: leaseTarget,
				busy:           true,
			}
			m.sessions[sessionID] = existing
			created = true
		} else {
			if existing.busy {
				m.mu.Unlock()
				return terminalExecRunResult{}, newTerminalExecError(terminalExecCodeSessionBusy, terminalExecBusyMessage)
			}
			existing.busy = true
			if existing.leaseExpiresAt.Before(leaseTarget) {
				existing.leaseExpiresAt = leaseTarget
			}
		}
		session = existing
		m.mu.Unlock()
	}

	if created {
		if err := m.createAndStartContainer(ctx, session.containerName); err != nil {
			m.dropSession(session.sessionID)
			return terminalExecRunResult{}, err
		}
	}

	execResult := runDockerCommand(ctx, terminalExecDockerExecArgs(session.containerName, command)...)
	if execResult.Err != nil {
		if errors.Is(execResult.Err, context.DeadlineExceeded) || errors.Is(execResult.Err, context.Canceled) {
			m.destroySession(session.sessionID)
			return terminalExecRunResult{}, execResult.Err
		}
		m.markSessionIdle(session.sessionID)
		return terminalExecRunResult{}, fmt.Errorf("docker exec failed: %w", execResult.Err)
	}

	if isNoSuchContainerMessage(execResult.Stderr) {
		m.destroySession(session.sessionID)
		return terminalExecRunResult{}, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}

	stdout, stdoutTruncated := truncateByBytes(execResult.Stdout, m.outputLimitBytes)
	stderr, stderrTruncated := truncateByBytes(execResult.Stderr, m.outputLimitBytes)
	leaseExpiresAt, ok := m.markSessionIdle(session.sessionID)
	if !ok {
		return terminalExecRunResult{}, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}

	return terminalExecRunResult{
		SessionID:          session.sessionID,
		Created:            created,
		Stdout:             stdout,
		Stderr:             stderr,
		ExitCode:           execResult.ExitCode,
		StdoutTruncated:    stdoutTruncated,
		StderrTruncated:    stderrTruncated,
		LeaseExpiresUnixMS: leaseExpiresAt.UnixMilli(),
	}, nil
}

func (m *terminalSessionManager) resolveLeaseDuration(leaseTTLSec *int) (time.Duration, error) {
	leaseSec := m.leaseDefaultSec
	if leaseTTLSec != nil {
		leaseSec = *leaseTTLSec
	}

	if leaseSec < m.leaseMinSec || leaseSec > m.leaseMaxSec {
		return 0, newTerminalExecError(
			terminalExecCodeInvalidPayload,
			fmt.Sprintf("lease_ttl_sec must be between %d and %d", m.leaseMinSec, m.leaseMaxSec),
		)
	}
	return time.Duration(leaseSec) * time.Second, nil
}

func (m *terminalSessionManager) createAndStartContainer(ctx context.Context, containerName string) error {
	createResult := runDockerCommand(ctx, terminalExecDockerCreateArgs(
		containerName,
		m.dockerImage,
		m.memoryLimit,
		m.cpuLimit,
		m.pidsLimit,
	)...)
	if createResult.Err != nil {
		return fmt.Errorf("docker create failed: %w", createResult.Err)
	}
	if createResult.ExitCode != 0 {
		return fmt.Errorf("docker create failed: %s", dockerCommandFailureMessage("exit code", createResult.ExitCode, createResult.Stderr))
	}

	startResult := runDockerCommand(ctx, terminalExecDockerStartArgs(containerName)...)
	if startResult.Err != nil {
		m.forceRemoveContainer(containerName)
		return fmt.Errorf("docker start failed: %w", startResult.Err)
	}
	if startResult.ExitCode != 0 {
		m.forceRemoveContainer(containerName)
		return fmt.Errorf("docker start failed: %s", dockerCommandFailureMessage("exit code", startResult.ExitCode, startResult.Stderr))
	}
	return nil
}

func (m *terminalSessionManager) janitorLoop() {
	ticker := time.NewTicker(terminalExecJanitorInterval)
	defer ticker.Stop()
	defer close(m.doneCh)

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.cleanupExpiredSessions()
		}
	}
}

func (m *terminalSessionManager) cleanupExpiredSessions() {
	now := time.Now()
	expired := make([]*terminalSession, 0)

	m.mu.Lock()
	for sessionID, session := range m.sessions {
		if session == nil || session.busy {
			continue
		}
		if session.leaseExpiresAt.After(now) {
			continue
		}
		expired = append(expired, session)
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()

	for _, session := range expired {
		m.forceRemoveContainer(session.containerName)
	}
}

func (m *terminalSessionManager) markSessionIdle(sessionID string) (time.Time, bool) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return time.Time{}, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[sessionID]
	if !ok || session == nil {
		return time.Time{}, false
	}
	session.busy = false
	return session.leaseExpiresAt, true
}

func (m *terminalSessionManager) dropSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}

	m.mu.Lock()
	delete(m.sessions, sessionID)
	m.mu.Unlock()
}

func (m *terminalSessionManager) destroySession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}

	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()
	if !ok || session == nil {
		return
	}
	m.forceRemoveContainer(session.containerName)
}

func (m *terminalSessionManager) forceRemoveContainer(containerName string) {
	containerName = strings.TrimSpace(containerName)
	if containerName == "" {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), terminalExecCleanupTimeout)
	defer cancel()

	result := runDockerCommand(cleanupCtx, pythonExecDockerRemoveArgs(containerName)...)
	if result.Err != nil {
		log.Printf("terminalExec cleanup failed: container=%s err=%v", containerName, result.Err)
		return
	}
	if result.ExitCode != 0 && !isNoSuchContainerMessage(result.Stderr) {
		log.Printf(
			"terminalExec cleanup failed: container=%s %s",
			containerName,
			dockerCommandFailureMessage("exit code", result.ExitCode, result.Stderr),
		)
	}
}

func terminalExecDockerCreateArgs(containerName string, dockerImage string, memoryLimit string, cpuLimit string, pidsLimit int) []string {
	return []string{
		"create",
		"--name", containerName,
		"--label", pythonExecManagedLabel,
		"--label", terminalExecCapabilityLabel,
		"--label", pythonExecRuntimeLabel,
		"--memory", memoryLimit,
		"--cpus", cpuLimit,
		"--pids-limit", strconv.Itoa(pidsLimit),
		dockerImage,
		"sh",
		"-lc",
		terminalExecIdleCommand,
	}
}

func terminalExecDockerStartArgs(containerName string) []string {
	return []string{"start", containerName}
}

func terminalExecDockerExecArgs(containerName string, command string) []string {
	return []string{"exec", containerName, "sh", "-lc", command}
}

func newTerminalExecContainerName() (string, error) {
	suffix, err := randomHex(8)
	if err != nil {
		return "", err
	}
	return terminalExecContainerPrefix + suffix, nil
}

func truncateByBytes(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 {
		return value, false
	}
	if len(value) <= maxBytes {
		return value, false
	}
	return value[:maxBytes], true
}
