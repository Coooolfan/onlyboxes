package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b"
	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/logging"
)

const (
	terminalExecCapabilityName        = "terminalexec"
	terminalExecCapabilityDeclared    = "terminalExec"
	terminalExecJanitorInterval       = 5 * time.Second
	terminalExecCleanupTimeout        = 10 * time.Second
	terminalExecNoSessionMessage      = "session not found"
	terminalExecBusyMessage           = "session is busy"
	terminalExecNotReadyMessage       = "terminal executor is unavailable"
	defaultTerminalLeaseMinSec        = 60
	defaultTerminalLeaseMaxSec        = 1800
	defaultTerminalLeaseSec           = 300
	defaultTerminalOutputLimitBytes   = 1024 * 1024
	defaultTerminalSessionMaxInflight = 128
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

func newTerminalExecError(code, message string) *terminalExecError {
	return &terminalExecError{code: strings.TrimSpace(code), message: strings.TrimSpace(message)}
}

type terminalSession struct {
	sessionID               string
	sandbox                 *e2b.Sandbox
	desiredLeaseExpiresAt   time.Time
	confirmedLeaseExpiresAt time.Time
	remoteTimeoutExpiresAt  time.Time
	leaseSyncMu             sync.Mutex
	inflight                int
	destroying              bool
	ready                   chan struct{}
	initErr                 error
}

type terminalSessionManagerConfig struct {
	Backend            e2bBackend
	Template           string
	LeaseMinSec        int
	LeaseMaxSec        int
	LeaseDefaultSec    int
	OutputLimitBytes   int
	ExportMaxBytes     int
	ExportMode         string
	SessionMaxInflight int
	// JanitorInterval is test-only tuning in practice; zero selects the
	// production interval.
	JanitorInterval time.Duration
}

type terminalSessionManager struct {
	mu       sync.Mutex
	sessions map[string]*terminalSession
	closed   bool
	createWG sync.WaitGroup

	backend            e2bBackend
	template           string
	leaseMinSec        int
	leaseMaxSec        int
	leaseDefaultSec    int
	outputLimitBytes   int
	exportMaxBytes     int
	exportMode         string
	sessionMaxInflight int
	janitorInterval    time.Duration

	stopCh    chan struct{}
	doneCh    chan struct{}
	closeOnce sync.Once
}

func newTerminalSessionManager(cfg terminalSessionManagerConfig) *terminalSessionManager {
	leaseMin := cfg.LeaseMinSec
	if leaseMin <= 0 {
		leaseMin = defaultTerminalLeaseMinSec
	}
	leaseMax := cfg.LeaseMaxSec
	if leaseMax <= 0 {
		leaseMax = defaultTerminalLeaseMaxSec
	}
	if leaseMax < leaseMin {
		leaseMax = leaseMin
	}
	leaseDefault := cfg.LeaseDefaultSec
	if leaseDefault <= 0 {
		leaseDefault = defaultTerminalLeaseSec
	}
	leaseDefault = clampTerminalValue(leaseDefault, leaseMin, leaseMax)
	outputLimit := cfg.OutputLimitBytes
	if outputLimit <= 0 {
		outputLimit = defaultTerminalOutputLimitBytes
	}
	maxInflight := cfg.SessionMaxInflight
	if maxInflight <= 0 {
		maxInflight = defaultTerminalSessionMaxInflight
	}
	janitorInterval := cfg.JanitorInterval
	if janitorInterval <= 0 {
		janitorInterval = terminalExecJanitorInterval
	}
	manager := &terminalSessionManager{
		sessions:           map[string]*terminalSession{},
		backend:            cfg.Backend,
		template:           strings.TrimSpace(cfg.Template),
		leaseMinSec:        leaseMin,
		leaseMaxSec:        leaseMax,
		leaseDefaultSec:    leaseDefault,
		outputLimitBytes:   outputLimit,
		exportMaxBytes:     cfg.ExportMaxBytes,
		exportMode:         normalizeTerminalExportMode(cfg.ExportMode),
		sessionMaxInflight: maxInflight,
		janitorInterval:    janitorInterval,
		stopCh:             make(chan struct{}),
		doneCh:             make(chan struct{}),
	}
	go manager.janitorLoop()
	return manager
}

func clampTerminalValue(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func (m *terminalSessionManager) ActiveSessionCount() int32 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var count int32
	for _, session := range m.sessions {
		if session != nil && !session.destroying {
			count++
		}
	}
	return count
}

func (m *terminalSessionManager) Execute(ctx context.Context, req terminalExecRequest) (terminalExecRunResult, error) {
	if m == nil || m.backend == nil {
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
	session, created, err := m.claimSession(strings.TrimSpace(req.SessionID), time.Now().Add(leaseDuration), req.CreateIfMissing)
	if err != nil {
		return terminalExecRunResult{}, err
	}
	if err := m.awaitSessionReady(ctx, session, created); err != nil {
		return terminalExecRunResult{}, err
	}
	if err := m.syncSandboxTimeout(ctx, session); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			m.releaseAndDestroySession(session.sessionID)
			return terminalExecRunResult{}, err
		}
		if errors.Is(err, e2b.ErrSandboxNotFound) {
			m.releaseAndDestroySession(session.sessionID)
			return terminalExecRunResult{}, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
		}
		m.releaseSession(session.sessionID)
		return terminalExecRunResult{}, fmt.Errorf("extend E2B sandbox timeout: %w", err)
	}
	result, err := m.backend.Run(ctx, session.sandbox, command, m.outputLimitBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			m.releaseAndDestroySession(session.sessionID)
			return terminalExecRunResult{}, err
		}
		if errors.Is(err, e2b.ErrSandboxNotFound) {
			m.releaseAndDestroySession(session.sessionID)
			return terminalExecRunResult{}, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
		}
		m.releaseSession(session.sessionID)
		return terminalExecRunResult{}, err
	}
	leaseExpires, ok := m.releaseSession(session.sessionID)
	if !ok {
		return terminalExecRunResult{}, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}
	return terminalExecRunResult{
		SessionID:          session.sessionID,
		Created:            created,
		Stdout:             result.Stdout,
		Stderr:             result.Stderr,
		ExitCode:           result.ExitCode,
		StdoutTruncated:    result.StdoutTruncated,
		StderrTruncated:    result.StderrTruncated,
		LeaseExpiresUnixMS: leaseExpires.UnixMilli(),
	}, nil
}

func (m *terminalSessionManager) resolveLeaseDuration(value *int) (time.Duration, error) {
	seconds := m.leaseDefaultSec
	if value != nil {
		seconds = *value
	}
	if seconds < m.leaseMinSec || seconds > m.leaseMaxSec {
		return 0, newTerminalExecError(
			terminalExecCodeInvalidPayload,
			fmt.Sprintf("lease_ttl_sec must be between %d and %d", m.leaseMinSec, m.leaseMaxSec),
		)
	}
	return time.Duration(seconds) * time.Second, nil
}

func (m *terminalSessionManager) claimSession(sessionID string, leaseTarget time.Time, createIfMissing bool) (*terminalSession, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, false, newTerminalExecError("execution_failed", terminalExecNotReadyMessage)
	}
	if sessionID == "" {
		return m.newSessionLocked(uuid.NewString(), leaseTarget), true, nil
	}
	if existing, ok := m.sessions[sessionID]; ok && existing != nil && !existing.destroying {
		if existing.inflight >= m.sessionMaxInflight {
			return nil, false, newTerminalExecError(terminalExecCodeSessionBusy, terminalExecBusyMessage)
		}
		existing.inflight++
		if existing.desiredLeaseExpiresAt.Before(leaseTarget) {
			existing.desiredLeaseExpiresAt = leaseTarget
		}
		return existing, false, nil
	}
	existing, exists := m.sessions[sessionID]
	if !createIfMissing || (exists && existing != nil && existing.destroying) {
		return nil, false, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}
	return m.newSessionLocked(sessionID, leaseTarget), true, nil
}

func (m *terminalSessionManager) newSessionLocked(sessionID string, leaseTarget time.Time) *terminalSession {
	m.createWG.Add(1)
	session := &terminalSession{
		sessionID:             sessionID,
		desiredLeaseExpiresAt: leaseTarget,
		inflight:              1,
		ready:                 make(chan struct{}),
	}
	m.sessions[sessionID] = session
	return session
}

func (m *terminalSessionManager) awaitSessionReady(ctx context.Context, session *terminalSession, created bool) error {
	if created {
		defer m.createWG.Done()
		m.mu.Lock()
		leaseExpiresAt := session.desiredLeaseExpiresAt
		m.mu.Unlock()
		timeout := secondsUntil(leaseExpiresAt)
		sandbox, err := m.backend.Create(ctx, m.template, timeout)
		m.mu.Lock()
		session.sandbox = sandbox
		session.initErr = err
		if err == nil {
			session.confirmedLeaseExpiresAt = leaseExpiresAt
			session.remoteTimeoutExpiresAt = leaseExpiresAt
		}
		m.mu.Unlock()
		close(session.ready)
		if err != nil {
			m.releaseAndDestroySession(session.sessionID)
			return fmt.Errorf("create E2B terminal sandbox: %w", err)
		}
		return nil
	}
	select {
	case <-session.ready:
	case <-ctx.Done():
		m.releaseSession(session.sessionID)
		return ctx.Err()
	}
	if session.initErr != nil {
		m.releaseSession(session.sessionID)
		return session.initErr
	}
	return nil
}

func (m *terminalSessionManager) syncSandboxTimeout(ctx context.Context, session *terminalSession) error {
	session.leaseSyncMu.Lock()
	defer session.leaseSyncMu.Unlock()

	m.mu.Lock()
	leaseExpires := session.desiredLeaseExpiresAt
	targetExpires := leaseExpires
	if deadline, ok := ctx.Deadline(); ok && deadline.After(targetExpires) {
		targetExpires = deadline
	}
	remoteExpires := session.remoteTimeoutExpiresAt
	sandbox := session.sandbox
	if !remoteExpires.Before(targetExpires) {
		if session.confirmedLeaseExpiresAt.Before(leaseExpires) {
			session.confirmedLeaseExpiresAt = leaseExpires
		}
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	if sandbox == nil {
		return errors.New("E2B sandbox is unavailable")
	}
	if err := m.backend.SetTimeout(ctx, sandbox.ID, secondsUntil(targetExpires)); err != nil {
		return err
	}
	m.mu.Lock()
	if session.remoteTimeoutExpiresAt.Before(targetExpires) {
		session.remoteTimeoutExpiresAt = targetExpires
	}
	if session.confirmedLeaseExpiresAt.Before(leaseExpires) {
		session.confirmedLeaseExpiresAt = leaseExpires
	}
	m.mu.Unlock()
	return nil
}

func secondsUntil(target time.Time) int {
	seconds := int(time.Until(target).Seconds()) + 1
	if seconds < 1 {
		return 1
	}
	return seconds
}

func (m *terminalSessionManager) releaseSession(sessionID string) (time.Time, bool) {
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	if !ok || session == nil {
		m.mu.Unlock()
		return time.Time{}, false
	}
	if session.inflight > 0 {
		session.inflight--
	}
	expires := session.confirmedLeaseExpiresAt
	var sandbox *e2b.Sandbox
	if session.destroying && session.inflight == 0 {
		delete(m.sessions, sessionID)
		sandbox = session.sandbox
	}
	m.mu.Unlock()
	if sandbox != nil {
		m.killSandbox(sandbox)
	}
	return expires, true
}

func (m *terminalSessionManager) releaseAndDestroySession(sessionID string) {
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
	var sandbox *e2b.Sandbox
	if session.inflight == 0 {
		delete(m.sessions, sessionID)
		sandbox = session.sandbox
	}
	m.mu.Unlock()
	if sandbox != nil {
		m.killSandbox(sandbox)
	}
}

func (m *terminalSessionManager) janitorLoop() {
	ticker := time.NewTicker(m.janitorInterval)
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
	var expired []*e2b.Sandbox
	m.mu.Lock()
	for id, session := range m.sessions {
		if session == nil || session.destroying || session.inflight > 0 || session.confirmedLeaseExpiresAt.After(now) {
			continue
		}
		delete(m.sessions, id)
		if session.sandbox != nil {
			expired = append(expired, session.sandbox)
		}
	}
	m.mu.Unlock()
	for _, sandbox := range expired {
		m.killSandbox(sandbox)
	}
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
		var sandboxes []*e2b.Sandbox
		for _, session := range m.sessions {
			if session != nil && session.sandbox != nil {
				sandboxes = append(sandboxes, session.sandbox)
			}
		}
		m.sessions = map[string]*terminalSession{}
		m.mu.Unlock()
		for _, sandbox := range sandboxes {
			m.killSandbox(sandbox)
		}
	})
}

func (m *terminalSessionManager) killSandbox(sandbox *e2b.Sandbox) {
	if sandbox == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), terminalExecCleanupTimeout)
	defer cancel()
	if err := m.backend.Kill(ctx, sandbox.ID); err != nil {
		logging.Warnf("terminalExec cleanup failed: sandbox_id=%s err=%v", sandbox.ID, err)
	}
}
