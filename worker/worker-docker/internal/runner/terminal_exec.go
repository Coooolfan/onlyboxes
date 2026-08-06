package runner

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/logging"
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
	terminalExecCapacityMessage    = "terminal session capacity exceeded"
	terminalExecNotReadyMessage    = "terminal executor is unavailable"

	// defaultTerminalSessionMaxInflight keeps one command per session, matching
	// the behaviour before per-session concurrency became configurable.
	defaultTerminalSessionMaxInflight = 1
)

const (
	terminalExecCodeSessionNotFound         = "session_not_found"
	terminalExecCodeSessionBusy             = "session_busy"
	terminalExecCodeSessionCapacityExceeded = "session_capacity_exceeded"
	terminalExecCodeInvalidPayload          = "invalid_payload"
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

	// inflight counts commands currently executing against this session.
	// A session is idle, and therefore reclaimable, only at zero.
	inflight int
	// destroying stops the session accepting new commands. The container is
	// removed by whichever caller drops inflight to zero.
	destroying bool
	// ready is closed once container creation finished; initErr carries the
	// outcome. Callers that did not create the session must wait on it before
	// touching the container.
	ready            chan struct{}
	initErr          error
	capacityReserved bool
}

type terminalSessionManagerConfig struct {
	LeaseMinSec      int
	LeaseMaxSec      int
	LeaseDefaultSec  int
	OutputLimitBytes int
	ExportMaxBytes   int
	DockerImage      string
	MemoryLimit      string
	CPULimit         string
	PidsLimit        int
	// SessionMaxInflight caps concurrent commands per session. Defaults to 1,
	// which preserves the original strictly serial behaviour.
	SessionMaxInflight int
	// MaxActiveSessions caps terminal sandbox reservations across all sessions.
	// Zero preserves the existing unlimited behaviour.
	MaxActiveSessions int
}

type terminalSessionManager struct {
	mu       sync.Mutex
	sessions map[string]*terminalSession

	leaseMinSec               int
	leaseMaxSec               int
	leaseDefaultSec           int
	outputLimitBytes          int
	exportMaxBytes            int
	dockerImage               string
	memoryLimit               string
	cpuLimit                  string
	pidsLimit                 int
	sessionMaxInflight        int
	maxActiveSessions         int
	activeSessionReservations int

	stopCh    chan struct{}
	doneCh    chan struct{}
	createWG  sync.WaitGroup
	cleanupWG sync.WaitGroup
	closed    bool
	closeOnce sync.Once
}

func (m *terminalSessionManager) ActiveSessionCount() int32 {
	if m == nil {
		return 0
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return int32(m.activeSessionReservations)
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

	sessionMaxInflight := cfg.SessionMaxInflight
	if sessionMaxInflight <= 0 {
		sessionMaxInflight = defaultTerminalSessionMaxInflight
	}
	maxActiveSessions := cfg.MaxActiveSessions
	if maxActiveSessions < 0 {
		maxActiveSessions = 0
	}

	manager := &terminalSessionManager{
		sessions:           make(map[string]*terminalSession),
		leaseMinSec:        leaseMinSec,
		leaseMaxSec:        leaseMaxSec,
		leaseDefaultSec:    leaseDefaultSec,
		outputLimitBytes:   outputLimitBytes,
		exportMaxBytes:     cfg.ExportMaxBytes,
		dockerImage:        dockerImage,
		memoryLimit:        memoryLimit,
		cpuLimit:           cpuLimit,
		pidsLimit:          pidsLimit,
		sessionMaxInflight: sessionMaxInflight,
		maxActiveSessions:  maxActiveSessions,
		stopCh:             make(chan struct{}),
		doneCh:             make(chan struct{}),
	}
	go manager.janitorLoop()
	return manager
}

func (m *terminalSessionManager) Close() {
	if m == nil {
		return
	}

	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		close(m.stopCh)
		<-m.doneCh
		m.createWG.Wait()

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

		m.cleanupWG.Wait()
		for _, session := range sessions {
			m.cleanupSession(session)
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

	leaseTarget := time.Now().Add(leaseDuration)

	session, created, err := m.claimSession(strings.TrimSpace(req.SessionID), leaseTarget, req.CreateIfMissing)
	if err != nil {
		return terminalExecRunResult{}, err
	}

	if err := m.awaitSessionReady(ctx, session, created); err != nil {
		return terminalExecRunResult{}, err
	}

	execResult := runDockerCommand(ctx, terminalExecDockerExecArgs(session.containerName, command)...)
	if execResult.Err != nil {
		if errors.Is(execResult.Err, context.DeadlineExceeded) || errors.Is(execResult.Err, context.Canceled) {
			m.releaseAndDestroySession(session.sessionID)
			return terminalExecRunResult{}, execResult.Err
		}
		m.releaseSession(session.sessionID)
		return terminalExecRunResult{}, fmt.Errorf("docker exec failed: %w", execResult.Err)
	}

	if isNoSuchContainerMessage(execResult.Stderr) {
		m.releaseAndDestroySession(session.sessionID)
		return terminalExecRunResult{}, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}

	stdout, stdoutTruncated := truncateByBytes(execResult.Stdout, m.outputLimitBytes)
	stderr, stderrTruncated := truncateByBytes(execResult.Stderr, m.outputLimitBytes)
	leaseExpiresAt, ok := m.releaseSession(session.sessionID)
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
		if session == nil || session.destroying || session.inflight > 0 {
			continue
		}
		if session.leaseExpiresAt.After(now) {
			continue
		}
		expired = append(expired, session)
		m.cleanupWG.Add(1)
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()

	for _, session := range expired {
		m.cleanupTrackedSession(session)
	}
}

// claimSession reserves one inflight slot, creating the session when needed.
// An empty sessionID always allocates a new session.
func (m *terminalSessionManager) claimSession(
	sessionID string,
	leaseTarget time.Time,
	createIfMissing bool,
) (*terminalSession, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, false, newTerminalExecError("execution_failed", terminalExecNotReadyMessage)
	}

	if sessionID == "" {
		session, err := m.newSessionLocked(uuid.NewString(), leaseTarget)
		return session, true, err
	}

	existing, ok := m.sessions[sessionID]
	if ok && existing != nil && !existing.destroying {
		if existing.inflight >= m.sessionMaxInflight {
			return nil, false, newTerminalExecError(terminalExecCodeSessionBusy, terminalExecBusyMessage)
		}
		existing.inflight++
		if existing.leaseExpiresAt.Before(leaseTarget) {
			existing.leaseExpiresAt = leaseTarget
		}
		return existing, false, nil
	}

	// Sessions pending destruction still own their id, so they are reported as
	// missing rather than being silently replaced.
	if !createIfMissing || (ok && existing != nil && existing.destroying) {
		return nil, false, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}

	session, err := m.newSessionLocked(sessionID, leaseTarget)
	return session, true, err
}

func (m *terminalSessionManager) newSessionLocked(sessionID string, leaseTarget time.Time) (*terminalSession, error) {
	if m.maxActiveSessions > 0 && m.activeSessionReservations >= m.maxActiveSessions {
		return nil, newTerminalExecError(
			terminalExecCodeSessionCapacityExceeded,
			terminalExecCapacityMessage,
		)
	}

	containerName, err := newTerminalExecContainerName()
	if err != nil {
		return nil, fmt.Errorf("allocate terminal container name: %w", err)
	}

	session := &terminalSession{
		sessionID:        sessionID,
		containerName:    containerName,
		leaseExpiresAt:   leaseTarget,
		inflight:         1,
		ready:            make(chan struct{}),
		capacityReserved: true,
	}
	m.sessions[sessionID] = session
	m.activeSessionReservations++
	m.createWG.Add(1)
	return session, nil
}

// awaitSessionReady gates command execution on container creation. The creating
// caller performs the work and publishes the outcome; everyone else blocks until
// the container exists, so no command runs against a half-built session.
func (m *terminalSessionManager) awaitSessionReady(ctx context.Context, session *terminalSession, created bool) error {
	if created {
		defer m.createWG.Done()
		err := m.createAndStartContainer(ctx, session.containerName)
		session.initErr = err
		close(session.ready)
		if err != nil {
			m.releaseAndDestroySession(session.sessionID)
			return err
		}
		return nil
	}

	select {
	case <-session.ready:
	case <-ctx.Done():
		m.releaseSession(session.sessionID)
		return ctx.Err()
	}

	// Safe without the lock: initErr is written before ready is closed.
	if session.initErr != nil {
		m.releaseSession(session.sessionID)
		return session.initErr
	}
	return nil
}

// releaseSession gives back one inflight slot and reports the current lease.
// It removes the container when it retires the last slot of a dying session.
// A command that already produced a result still reports success here, even if
// the session is being torn down: the work completed and its output is valid.
func (m *terminalSessionManager) releaseSession(sessionID string) (time.Time, bool) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return time.Time{}, false
	}

	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	if !ok || session == nil {
		m.mu.Unlock()
		return time.Time{}, false
	}
	if session.inflight > 0 {
		session.inflight--
	}
	leaseExpiresAt := session.leaseExpiresAt
	var retired *terminalSession
	if session.destroying && session.inflight == 0 {
		delete(m.sessions, sessionID)
		m.cleanupWG.Add(1)
		retired = session
	}
	m.mu.Unlock()

	if retired != nil {
		m.cleanupTrackedSession(retired)
	}
	return leaseExpiresAt, true
}

// releaseAndDestroySession retires the caller's slot and marks the session for
// destruction. The container survives until the last concurrent command drains,
// so one command's timeout cannot kill its siblings.
func (m *terminalSessionManager) releaseAndDestroySession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}

	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	if !ok || session == nil {
		m.mu.Unlock()
		return
	}
	session.destroying = true
	if session.inflight > 0 {
		session.inflight--
	}
	var retired *terminalSession
	if session.inflight == 0 {
		delete(m.sessions, sessionID)
		m.cleanupWG.Add(1)
		retired = session
	}
	m.mu.Unlock()

	if retired != nil {
		m.cleanupTrackedSession(retired)
	}
}

func (m *terminalSessionManager) cleanupTrackedSession(session *terminalSession) {
	defer m.cleanupWG.Done()
	m.cleanupSession(session)
}

func (m *terminalSessionManager) cleanupSession(session *terminalSession) {
	if session == nil {
		return
	}
	m.forceRemoveContainer(session.containerName)
	m.releaseCapacityReservation(session)
}

func (m *terminalSessionManager) releaseCapacityReservation(session *terminalSession) {
	if session == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if !session.capacityReserved {
		return
	}
	session.capacityReserved = false
	if m.activeSessionReservations > 0 {
		m.activeSessionReservations--
	}
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
		logging.Warnf("terminalExec cleanup failed: container=%s err=%v", containerName, result.Err)
		return
	}
	if result.ExitCode != 0 && !isNoSuchContainerMessage(result.Stderr) {
		logging.Warnf(
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
