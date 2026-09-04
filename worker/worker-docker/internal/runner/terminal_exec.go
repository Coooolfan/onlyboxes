package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/internal/logging"
)

const (
	terminalExecCapabilityName     = "terminalexec"
	terminalExecCapabilityDeclared = "terminalExec"
	terminalExecContainerPrefix    = "onlyboxes-terminal-v1-"
	terminalExecCapabilityLabel    = "onlyboxes.capability=terminalExec"
	terminalExecSessionLabelKey    = "onlyboxes.session_id_hash"
	terminalExecSchemaLabelKey     = "onlyboxes.schema_version"
	terminalExecSchemaVersion      = "1"
	terminalExecIdleCommand        = "while true; do sleep 3600; done"
	terminalExecCleanupTimeout     = 3 * time.Second
	terminalExecInspectTimeout     = 2 * time.Second
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
	containerIP    string
	leaseExpiresAt time.Time
	leaseTimer     *time.Timer
	proxyCtx       context.Context
	proxyCancel    context.CancelFunc

	// inflight counts commands currently executing against this session and
	// controls deferred cleanup for non-lease failures.
	inflight int
	// destroying stops the session accepting new commands. Non-lease failures
	// wait for inflight to drain; lease expiry is an immediate hard boundary.
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
	DockerNetwork    string
	// SessionMaxInflight caps concurrent commands per session. Defaults to 1,
	// which preserves the original strictly serial behaviour.
	SessionMaxInflight int
	// MaxActiveSessions caps terminal sandbox reservations across all sessions.
	// Zero preserves the existing unlimited behaviour.
	MaxActiveSessions int
	// PreserveOnClose leaves terminal containers running for process restart
	// recovery. Tests and explicit teardown may leave this false.
	PreserveOnClose bool
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
	dockerNetwork             string
	sessionMaxInflight        int
	maxActiveSessions         int
	activeSessionReservations int
	preserveOnClose           bool

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
		dockerNetwork:      strings.TrimSpace(cfg.DockerNetwork),
		sessionMaxInflight: sessionMaxInflight,
		maxActiveSessions:  maxActiveSessions,
		preserveOnClose:    cfg.PreserveOnClose,
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
			m.stopSessionLeaseTimerLocked(session)
			sessions = append(sessions, session)
		}
		m.sessions = make(map[string]*terminalSession)
		m.mu.Unlock()

		m.cleanupWG.Wait()
		for _, session := range sessions {
			if m.preserveOnClose {
				m.releaseCapacityReservation(session)
				continue
			}
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

func (m *terminalSessionManager) createAndStartContainer(ctx context.Context, containerName string) (string, error) {
	createResult := runDockerCommand(ctx, terminalExecDockerCreateArgsWithNetwork(
		containerName,
		m.dockerImage,
		m.memoryLimit,
		m.cpuLimit,
		m.pidsLimit,
		m.dockerNetwork,
	)...)
	if createResult.Err != nil {
		return "", fmt.Errorf("docker create failed: %w", createResult.Err)
	}
	if createResult.ExitCode != 0 {
		return "", fmt.Errorf("docker create failed: %s", dockerCommandFailureMessage("exit code", createResult.ExitCode, createResult.Stderr))
	}

	startResult := runDockerCommand(ctx, terminalExecDockerStartArgs(containerName)...)
	if startResult.Err != nil {
		m.forceRemoveContainer(containerName)
		return "", fmt.Errorf("docker start failed: %w", startResult.Err)
	}
	if startResult.ExitCode != 0 {
		m.forceRemoveContainer(containerName)
		return "", fmt.Errorf("docker start failed: %s", dockerCommandFailureMessage("exit code", startResult.ExitCode, startResult.Stderr))
	}
	if m.dockerNetwork == "" {
		return "", nil
	}
	containerIP, err := inspectTerminalContainerIP(ctx, containerName, m.dockerNetwork)
	if err != nil {
		m.forceRemoveContainer(containerName)
		return "", err
	}
	return containerIP, nil
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
		if session == nil || session.destroying {
			continue
		}
		if session.leaseExpiresAt.After(now) {
			continue
		}
		session.destroying = true
		m.stopSessionLeaseTimerLocked(session)
		if session.proxyCancel != nil {
			session.proxyCancel()
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

func (m *terminalSessionManager) expireSessionLease(session *terminalSession) {
	if m == nil || session == nil {
		return
	}

	now := time.Now()
	m.mu.Lock()
	current, ok := m.sessions[session.sessionID]
	if m.closed || !ok || current != session || session.destroying {
		m.mu.Unlock()
		return
	}
	if session.leaseExpiresAt.After(now) {
		m.scheduleSessionLeaseTimerLocked(session)
		m.mu.Unlock()
		return
	}
	session.destroying = true
	session.leaseTimer = nil
	if session.proxyCancel != nil {
		session.proxyCancel()
	}
	delete(m.sessions, session.sessionID)
	m.cleanupWG.Add(1)
	m.mu.Unlock()

	m.cleanupTrackedSession(session)
}

func (m *terminalSessionManager) scheduleSessionLeaseTimerLocked(session *terminalSession) {
	if m == nil || session == nil || m.closed || session.destroying {
		return
	}
	if session.leaseTimer != nil {
		session.leaseTimer.Stop()
	}
	delay := time.Until(session.leaseExpiresAt)
	if delay < 0 {
		delay = 0
	}
	session.leaseTimer = time.AfterFunc(delay, func() {
		m.expireSessionLease(session)
	})
}

func (m *terminalSessionManager) stopSessionLeaseTimerLocked(session *terminalSession) {
	if session == nil || session.leaseTimer == nil {
		return
	}
	session.leaseTimer.Stop()
	session.leaseTimer = nil
}

type terminalProxyTarget struct {
	IP             string
	SessionContext context.Context
}

var errTerminalProxySessionNotFound = errors.New("proxy session not found")

func (m *terminalSessionManager) ResolveProxyTarget(ctx context.Context, sessionID string, now time.Time) (terminalProxyTarget, error) {
	if m == nil {
		return terminalProxyTarget{}, errTerminalProxySessionNotFound
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return terminalProxyTarget{}, errTerminalProxySessionNotFound
	}
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	if !ok || session == nil || session.destroying {
		m.mu.Unlock()
		return terminalProxyTarget{}, errTerminalProxySessionNotFound
	}
	ready := session.ready
	m.mu.Unlock()

	select {
	case <-ctx.Done():
		return terminalProxyTarget{}, ctx.Err()
	case <-ready:
	}

	if now.IsZero() {
		now = time.Now()
	}
	var expired *terminalSession
	m.mu.Lock()
	current, ok := m.sessions[sessionID]
	if !ok || current != session || session.destroying || session.initErr != nil {
		m.mu.Unlock()
		return terminalProxyTarget{}, errTerminalProxySessionNotFound
	}
	if !session.leaseExpiresAt.After(now) {
		session.destroying = true
		m.stopSessionLeaseTimerLocked(session)
		if session.proxyCancel != nil {
			session.proxyCancel()
		}
		delete(m.sessions, sessionID)
		m.cleanupWG.Add(1)
		expired = session
		m.mu.Unlock()
		go m.cleanupTrackedSession(expired)
		return terminalProxyTarget{}, errTerminalProxySessionNotFound
	}
	address, err := netip.ParseAddr(strings.TrimSpace(session.containerIP))
	if err != nil || address.IsUnspecified() {
		m.mu.Unlock()
		return terminalProxyTarget{}, errTerminalProxySessionNotFound
	}
	sessionContext := session.proxyCtx
	if sessionContext == nil {
		sessionContext = context.Background()
	}
	target := terminalProxyTarget{
		IP:             address.Unmap().String(),
		SessionContext: sessionContext,
	}
	m.mu.Unlock()
	return target, nil
}

func inspectTerminalContainerIP(ctx context.Context, containerName string, dockerNetwork string) (string, error) {
	inspectCtx, cancel := context.WithTimeout(ctx, terminalExecInspectTimeout)
	defer cancel()
	result := runDockerCommand(inspectCtx, terminalExecDockerInspectIPArgs(containerName, dockerNetwork)...)
	if result.Err != nil {
		return "", fmt.Errorf("docker inspect proxy target failed: %w", result.Err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("docker inspect proxy target failed: %s", dockerCommandFailureMessage("exit code", result.ExitCode, result.Stderr))
	}
	address, err := netip.ParseAddr(strings.TrimSpace(result.Stdout))
	if err != nil {
		return "", errors.New("docker inspect returned an invalid proxy target IP")
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() {
		return "", errors.New("docker inspect returned an invalid proxy target IP")
	}
	return address.String(), nil
}

func terminalExecDockerInspectIPArgs(containerName string, dockerNetwork string) []string {
	format := `{{with index .NetworkSettings.Networks "` + strings.TrimSpace(dockerNetwork) + `"}}{{.IPAddress}}{{end}}`
	return []string{"inspect", "--format", format, containerName}
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
		generatedID, err := randomTerminalSessionID()
		if err != nil {
			return nil, false, fmt.Errorf("generate terminal session ID: %w", err)
		}
		session, err := m.newSessionLocked(generatedID, leaseTarget)
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
			m.scheduleSessionLeaseTimerLocked(existing)
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

func randomTerminalSessionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func (m *terminalSessionManager) newSessionLocked(sessionID string, leaseTarget time.Time) (*terminalSession, error) {
	if m.maxActiveSessions > 0 && m.activeSessionReservations >= m.maxActiveSessions {
		return nil, newTerminalExecError(
			terminalExecCodeSessionCapacityExceeded,
			terminalExecCapacityMessage,
		)
	}

	containerName := terminalSessionResourceName(sessionID)

	proxyCtx, proxyCancel := context.WithCancel(context.Background())
	session := &terminalSession{
		sessionID:        sessionID,
		containerName:    containerName,
		leaseExpiresAt:   leaseTarget,
		inflight:         1,
		ready:            make(chan struct{}),
		capacityReserved: true,
		proxyCtx:         proxyCtx,
		proxyCancel:      proxyCancel,
	}
	m.sessions[sessionID] = session
	m.activeSessionReservations++
	m.scheduleSessionLeaseTimerLocked(session)
	m.createWG.Add(1)
	return session, nil
}

// awaitSessionReady gates command execution on container creation. The creating
// caller performs the work and publishes the outcome; everyone else blocks until
// the container exists, so no command runs against a half-built session.
func (m *terminalSessionManager) awaitSessionReady(ctx context.Context, session *terminalSession, created bool) error {
	if created {
		defer m.createWG.Done()
		containerIP, err := m.createAndStartContainer(ctx, session.containerName)
		session.containerIP = containerIP
		session.initErr = err
		close(session.ready)
		if err != nil {
			m.releaseAndDestroySession(session.sessionID)
			return err
		}

		if !m.sessionIsActive(session) {
			m.cleanupInactiveCreatedSession(session)
			return newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
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
	if !m.sessionIsActive(session) {
		return newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}
	return nil
}

func (m *terminalSessionManager) cleanupInactiveCreatedSession(session *terminalSession) {
	if m == nil || session == nil {
		return
	}
	m.mu.Lock()
	current, owned := m.sessions[session.sessionID]
	owned = owned && current == session
	if owned {
		delete(m.sessions, session.sessionID)
		session.destroying = true
		m.stopSessionLeaseTimerLocked(session)
		if session.proxyCancel != nil {
			session.proxyCancel()
		}
	}
	m.mu.Unlock()

	if owned {
		m.cleanupSession(session)
		return
	}
	// An expiry cleanup may have run before docker create completed.
	m.forceRemoveContainer(session.containerName)
}

func (m *terminalSessionManager) sessionIsActive(session *terminalSession) bool {
	if m == nil || session == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.sessions[session.sessionID]
	return ok && current == session && !session.destroying && !m.closed
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
	m.stopSessionLeaseTimerLocked(session)
	if session.proxyCancel != nil {
		session.proxyCancel()
	}
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
	if session.proxyCancel != nil {
		session.proxyCancel()
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
	return terminalExecDockerCreateArgsWithNetwork(containerName, dockerImage, memoryLimit, cpuLimit, pidsLimit, "")
}

func terminalExecDockerCreateArgsWithNetwork(containerName string, dockerImage string, memoryLimit string, cpuLimit string, pidsLimit int, dockerNetwork string) []string {
	sessionHash := strings.TrimPrefix(strings.TrimSpace(containerName), terminalExecContainerPrefix)
	args := []string{
		"create",
		"--name", containerName,
		"--label", pythonExecManagedLabel,
		"--label", terminalExecCapabilityLabel,
		"--label", pythonExecRuntimeLabel,
		"--label", terminalExecSessionLabelKey + "=" + sessionHash,
		"--label", terminalExecSchemaLabelKey + "=" + terminalExecSchemaVersion,
		"--memory", memoryLimit,
		"--cpus", cpuLimit,
		"--pids-limit", strconv.Itoa(pidsLimit),
	}
	if network := strings.TrimSpace(dockerNetwork); network != "" {
		args = append(args, "--network", network)
	}
	return append(args,
		dockerImage,
		"sh",
		"-lc",
		terminalExecIdleCommand,
	)
}

func terminalExecDockerStartArgs(containerName string) []string {
	return []string{"start", containerName}
}

func terminalExecDockerExecArgs(containerName string, command string) []string {
	return []string{"exec", containerName, "sh", "-lc", command}
}

func terminalSessionIDHash(sessionID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sessionID)))
	return hex.EncodeToString(sum[:])
}

func terminalSessionResourceName(sessionID string) string {
	return terminalExecContainerPrefix + terminalSessionIDHash(sessionID)
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
