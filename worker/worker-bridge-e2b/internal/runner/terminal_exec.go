package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/internal/logging"
	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b"
)

const (
	terminalExecCapabilityName        = "terminalexec"
	terminalExecCapabilityDeclared    = "terminalExec"
	terminalExecJanitorInterval       = 5 * time.Second
	terminalExecCleanupTimeout        = 10 * time.Second
	terminalExecNoSessionMessage      = "session not found"
	terminalExecBusyMessage           = "session is busy"
	terminalExecCapacityMessage       = "terminal session capacity exceeded"
	terminalExecNotReadyMessage       = "terminal executor is unavailable"
	defaultTerminalLeaseMinSec        = 60
	defaultTerminalLeaseMaxSec        = 1800
	defaultTerminalLeaseSec           = 300
	defaultTerminalOutputLimitBytes   = 1024 * 1024
	defaultTerminalSessionMaxInflight = 128
	terminalSessionWorkerMetadataKey  = "onlyboxes.worker"
	terminalSessionWorkerMetadata     = "worker-bridge-e2b"
	terminalSessionMetadataKey        = "onlyboxes.session_id_hash"
	terminalSessionSchemaKey          = "onlyboxes.schema_version"
	terminalSessionSchemaVersion      = "1"
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

type terminalProxyRunResult struct {
	URL          string `json:"url"`
	TrafficToken string `json:"traffic_token,omitempty"`
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
	capacityReserved        bool
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
	MaxActiveSessions  int
	PreserveOnClose    bool
	// JanitorInterval is test-only tuning in practice; zero selects the
	// production interval.
	JanitorInterval time.Duration
}

type terminalSessionManager struct {
	mu        sync.Mutex
	sessions  map[string]*terminalSession
	closed    bool
	createWG  sync.WaitGroup
	cleanupWG sync.WaitGroup

	backend                   e2bBackend
	template                  string
	leaseMinSec               int
	leaseMaxSec               int
	leaseDefaultSec           int
	outputLimitBytes          int
	exportMaxBytes            int
	exportMode                string
	sessionMaxInflight        int
	maxActiveSessions         int
	activeSessionReservations int
	janitorInterval           time.Duration
	preserveOnClose           bool

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
	maxActiveSessions := cfg.MaxActiveSessions
	if maxActiveSessions < 0 {
		maxActiveSessions = 0
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
		maxActiveSessions:  maxActiveSessions,
		janitorInterval:    janitorInterval,
		preserveOnClose:    cfg.PreserveOnClose,
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
	return int32(m.activeSessionReservations)
}

func (m *terminalSessionManager) ResolveProxy(_ context.Context, sessionID string, port int, now time.Time) (terminalProxyRunResult, error) {
	if m == nil || port < 1 || port > 65535 {
		return terminalProxyRunResult{}, newTerminalExecError(terminalExecCodeInvalidPayload, "port must be between 1 and 65535")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return terminalProxyRunResult{}, newTerminalExecError(terminalExecCodeInvalidPayload, "session_id is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	if !ok || session == nil || session.destroying || session.sandbox == nil || !session.confirmedLeaseExpiresAt.After(now) {
		m.mu.Unlock()
		return terminalProxyRunResult{}, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}
	sandbox := *session.sandbox
	m.mu.Unlock()
	proxyURL, trafficToken, err := sandbox.ProxyURL(port)
	if err != nil {
		return terminalProxyRunResult{}, err
	}
	return terminalProxyRunResult{URL: proxyURL, TrafficToken: trafficToken}, nil
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

type retiredTerminalSession struct {
	sandbox          *e2b.Sandbox
	capacityReserved bool
}

func (m *terminalSessionManager) claimSession(sessionID string, leaseTarget time.Time, createIfMissing bool) (*terminalSession, bool, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, false, newTerminalExecError("execution_failed", terminalExecNotReadyMessage)
	}
	if sessionID == "" {
		session, err := m.newSessionLocked(rand.Text(), leaseTarget)
		m.mu.Unlock()
		return session, true, err
	}
	if existing, ok := m.sessions[sessionID]; ok && existing != nil && !existing.destroying {
		if existing.inflight == 0 && !existing.confirmedLeaseExpiresAt.After(time.Now()) {
			oldCapacityReserved := existing.capacityReserved
			if !createIfMissing {
				delete(m.sessions, sessionID)
				m.cleanupWG.Add(1)
				retired := retiredTerminalSession{
					sandbox:          existing.sandbox,
					capacityReserved: oldCapacityReserved,
				}
				m.mu.Unlock()
				m.cleanupTrackedRetiredSession(retired)
				return nil, false, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
			}
			if !oldCapacityReserved && !m.capacityAvailableLocked() {
				m.mu.Unlock()
				return nil, false, newTerminalExecError(
					terminalExecCodeSessionCapacityExceeded,
					terminalExecCapacityMessage,
				)
			}
			delete(m.sessions, sessionID)
			m.cleanupWG.Add(1)
			retired := retiredTerminalSession{
				sandbox:          existing.sandbox,
				capacityReserved: false,
			}
			session := m.newSessionWithReservationLocked(sessionID, leaseTarget, true, !oldCapacityReserved)
			m.mu.Unlock()
			// The replacement inherits the old slot, so the old sandbox must be
			// cleaned before the new remote sandbox is created.
			m.cleanupTrackedRetiredSession(retired)
			return session, true, nil
		}
		if existing.inflight >= m.sessionMaxInflight {
			m.mu.Unlock()
			return nil, false, newTerminalExecError(terminalExecCodeSessionBusy, terminalExecBusyMessage)
		}
		existing.inflight++
		if existing.desiredLeaseExpiresAt.Before(leaseTarget) {
			existing.desiredLeaseExpiresAt = leaseTarget
		}
		m.mu.Unlock()
		return existing, false, nil
	}
	existing, exists := m.sessions[sessionID]
	if !createIfMissing || (exists && existing != nil && existing.destroying) {
		m.mu.Unlock()
		return nil, false, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}
	session, err := m.newSessionLocked(sessionID, leaseTarget)
	m.mu.Unlock()
	return session, true, err
}

func (m *terminalSessionManager) capacityAvailableLocked() bool {
	return m.maxActiveSessions <= 0 || m.activeSessionReservations < m.maxActiveSessions
}

func (m *terminalSessionManager) newSessionLocked(sessionID string, leaseTarget time.Time) (*terminalSession, error) {
	if !m.capacityAvailableLocked() {
		return nil, newTerminalExecError(
			terminalExecCodeSessionCapacityExceeded,
			terminalExecCapacityMessage,
		)
	}
	return m.newSessionWithReservationLocked(sessionID, leaseTarget, true, true), nil
}

func (m *terminalSessionManager) newSessionWithReservationLocked(
	sessionID string,
	leaseTarget time.Time,
	capacityReserved bool,
	incrementReservation bool,
) *terminalSession {
	m.createWG.Add(1)
	session := &terminalSession{
		sessionID:             sessionID,
		desiredLeaseExpiresAt: leaseTarget,
		inflight:              1,
		capacityReserved:      capacityReserved,
		ready:                 make(chan struct{}),
	}
	m.sessions[sessionID] = session
	if capacityReserved && incrementReservation {
		m.activeSessionReservations++
	}
	return session
}

func (m *terminalSessionManager) awaitSessionReady(ctx context.Context, session *terminalSession, created bool) error {
	if created {
		defer m.createWG.Done()
		m.mu.Lock()
		leaseExpiresAt := session.desiredLeaseExpiresAt
		m.mu.Unlock()
		timeout := secondsUntil(leaseExpiresAt)
		var sandbox *e2b.Sandbox
		var err error
		if recoveryBackend, ok := m.backend.(e2bRecoveryBackend); ok {
			sandbox, err = recoveryBackend.CreateWithMetadata(ctx, m.template, timeout, terminalSessionMetadata(session.sessionID))
		} else {
			sandbox, err = m.backend.Create(ctx, m.template, timeout)
		}
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

func (m *terminalSessionManager) cleanupTrackedRetiredSession(session retiredTerminalSession) {
	defer m.cleanupWG.Done()
	m.cleanupRetiredSession(session)
}

func (m *terminalSessionManager) cleanupRetiredSession(session retiredTerminalSession) {
	m.killSandbox(session.sandbox)
	if session.capacityReserved {
		m.mu.Lock()
		if m.activeSessionReservations > 0 {
			m.activeSessionReservations--
		}
		m.mu.Unlock()
	}
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
	var retired *retiredTerminalSession
	if session.destroying && session.inflight == 0 {
		delete(m.sessions, sessionID)
		m.cleanupWG.Add(1)
		retired = &retiredTerminalSession{
			sandbox:          session.sandbox,
			capacityReserved: session.capacityReserved,
		}
	}
	m.mu.Unlock()
	if retired != nil {
		m.cleanupTrackedRetiredSession(*retired)
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
	var retired *retiredTerminalSession
	if session.inflight == 0 {
		delete(m.sessions, sessionID)
		m.cleanupWG.Add(1)
		retired = &retiredTerminalSession{
			sandbox:          session.sandbox,
			capacityReserved: session.capacityReserved,
		}
	}
	m.mu.Unlock()
	if retired != nil {
		m.cleanupTrackedRetiredSession(*retired)
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
	var expired []retiredTerminalSession
	m.mu.Lock()
	for id, session := range m.sessions {
		if session == nil || session.destroying || session.inflight > 0 || session.confirmedLeaseExpiresAt.After(now) {
			continue
		}
		delete(m.sessions, id)
		m.cleanupWG.Add(1)
		expired = append(expired, retiredTerminalSession{
			sandbox:          session.sandbox,
			capacityReserved: session.capacityReserved,
		})
	}
	m.mu.Unlock()
	for _, session := range expired {
		m.cleanupTrackedRetiredSession(session)
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
		var sessions []retiredTerminalSession
		for _, session := range m.sessions {
			if session != nil {
				sessions = append(sessions, retiredTerminalSession{
					sandbox:          session.sandbox,
					capacityReserved: session.capacityReserved,
				})
			}
		}
		m.sessions = map[string]*terminalSession{}
		m.mu.Unlock()
		m.cleanupWG.Wait()
		for _, session := range sessions {
			if m.preserveOnClose {
				m.mu.Lock()
				if session.capacityReserved && m.activeSessionReservations > 0 {
					m.activeSessionReservations--
				}
				m.mu.Unlock()
				continue
			}
			m.cleanupRetiredSession(session)
		}
	})
}

func terminalSessionIDHash(sessionID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sessionID)))
	return hex.EncodeToString(sum[:])
}

func terminalSessionMetadata(sessionID string) map[string]string {
	return map[string]string{
		terminalSessionWorkerMetadataKey: terminalSessionWorkerMetadata,
		terminalSessionMetadataKey:       terminalSessionIDHash(sessionID),
		terminalSessionSchemaKey:         terminalSessionSchemaVersion,
	}
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
