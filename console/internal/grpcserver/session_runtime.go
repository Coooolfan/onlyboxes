package grpcserver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

type commandOutcome struct {
	payloadJSON []byte
	message     string
	err         error
	completedAt time.Time
}

type pendingCommand struct {
	resultCh   chan commandOutcome
	capability string
	closeOnce  sync.Once
}

type sessionCapability struct {
	maxInflight int
	inflight    int
}

type terminalSessionCapacitySnapshot struct {
	known              bool
	maxActiveSessions  int
	activeSessionCount int
	observedAt         time.Time
}

type activeSession struct {
	nodeID       string
	sessionID    string
	executorKind string
	connectedAt  time.Time

	capabilitiesMu sync.Mutex
	capabilities   map[string]*sessionCapability

	terminalCapacityMu sync.RWMutex
	terminalCapacity   terminalSessionCapacitySnapshot

	recoveryMu         sync.RWMutex
	recoveryRequired   bool
	recoveryComplete   bool
	recoveryCandidates map[string]int64
	recoveryResults    map[string]registryv1.TerminalSessionRecoveryResult_Status

	controlOutbound chan *registryv1.ConnectResponse
	commandOutbound chan *registryv1.ConnectResponse
	done            chan struct{}

	pendingMu sync.Mutex
	pending   map[string]*pendingCommand

	closeOnce sync.Once
	closedErr error
}

func newActiveSession(nodeID string, sessionID string, hello *registryv1.ConnectHello) *activeSession {
	session := newActiveSessionAt(nodeID, sessionID, hello, time.Now())
	// This convenience constructor is used by already-established in-process
	// sessions in tests. Real Connect sessions use newActiveSessionAt and must
	// complete the recovery handshake before becoming ready.
	session.markRecoveryComplete()
	return session
}

func newActiveSessionAt(nodeID string, sessionID string, hello *registryv1.ConnectHello, observedAt time.Time) *activeSession {
	session := &activeSession{
		nodeID:             nodeID,
		sessionID:          sessionID,
		executorKind:       strings.TrimSpace(hello.GetExecutorKind()),
		connectedAt:        observedAt,
		capabilities:       capabilitiesFromHello(hello),
		controlOutbound:    make(chan *registryv1.ConnectResponse, controlOutboundBufferSize),
		commandOutbound:    make(chan *registryv1.ConnectResponse, commandOutboundBufferSize),
		done:               make(chan struct{}),
		pending:            make(map[string]*pendingCommand),
		recoveryCandidates: make(map[string]int64),
		recoveryResults:    make(map[string]registryv1.TerminalSessionRecoveryResult_Status),
	}
	_, session.recoveryRequired = session.capabilities[taskCapabilityTerminalExec]
	session.recoveryComplete = !session.recoveryRequired
	if capacity := hello.GetTerminalSessionCapacity(); capacity != nil {
		session.terminalCapacity = terminalSessionCapacitySnapshot{
			known:              true,
			maxActiveSessions:  int(capacity.GetMaxActiveSessions()),
			activeSessionCount: int(capacity.GetActiveSessionCount()),
			observedAt:         observedAt,
		}
	}
	return session
}

func (s *activeSession) setRecoveryCandidates(candidates []*registryv1.TerminalSessionRecoveryCandidate) {
	if s == nil {
		return
	}
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	s.recoveryCandidates = make(map[string]int64, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		sessionID := strings.TrimSpace(candidate.GetSessionId())
		if sessionID == "" {
			continue
		}
		s.recoveryCandidates[sessionID] = candidate.GetLeaseExpiresUnixMs()
	}
}

func (s *activeSession) recoveryCandidateSnapshot() map[string]int64 {
	if s == nil {
		return nil
	}
	s.recoveryMu.RLock()
	defer s.recoveryMu.RUnlock()
	out := make(map[string]int64, len(s.recoveryCandidates))
	for sessionID, lease := range s.recoveryCandidates {
		out[sessionID] = lease
	}
	return out
}

func (s *activeSession) markRecoveryComplete() {
	if s == nil {
		return
	}
	s.recoveryMu.Lock()
	s.recoveryComplete = true
	s.recoveryMu.Unlock()
}

func (s *activeSession) setRecoveryResults(results map[string]registryv1.TerminalSessionRecoveryResult_Status) {
	if s == nil {
		return
	}
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	s.recoveryResults = make(map[string]registryv1.TerminalSessionRecoveryResult_Status, len(results))
	for sessionID, status := range results {
		s.recoveryResults[sessionID] = status
	}
}

func (s *activeSession) matchesRecoveryResults(report *registryv1.TerminalSessionRecoveryReport) bool {
	if s == nil || report == nil {
		return false
	}
	s.recoveryMu.RLock()
	defer s.recoveryMu.RUnlock()
	if len(report.GetResults()) != len(s.recoveryResults) {
		return false
	}
	seen := make(map[string]struct{}, len(report.GetResults()))
	for _, result := range report.GetResults() {
		if result == nil {
			return false
		}
		sessionID := strings.TrimSpace(result.GetSessionId())
		status, ok := s.recoveryResults[sessionID]
		if !ok || status != result.GetStatus() {
			return false
		}
		if _, duplicate := seen[sessionID]; duplicate {
			return false
		}
		seen[sessionID] = struct{}{}
	}
	return true
}

func (s *activeSession) isReady() bool {
	if s == nil {
		return false
	}
	s.recoveryMu.RLock()
	defer s.recoveryMu.RUnlock()
	return !s.recoveryRequired || s.recoveryComplete
}

func (s *activeSession) hasCapability(capability string) bool {
	normalized := normalizeCapability(capability)
	if normalized == "" {
		return false
	}
	s.capabilitiesMu.Lock()
	defer s.capabilitiesMu.Unlock()
	_, ok := s.capabilities[normalized]
	return ok
}

func (s *activeSession) inflightSnapshot(capability string) (int, int, bool) {
	normalized := normalizeCapability(capability)
	if normalized == "" {
		return 0, 0, false
	}
	s.capabilitiesMu.Lock()
	defer s.capabilitiesMu.Unlock()
	state, ok := s.capabilities[normalized]
	if !ok || state == nil {
		return 0, 0, false
	}
	max := state.maxInflight
	if max <= 0 {
		max = defaultCapabilityMaxInflight
		state.maxInflight = max
	}
	return state.inflight, max, true
}

type capabilitySnapshot struct {
	name        string
	inflight    int
	maxInflight int
}

func (s *activeSession) allCapabilitiesSnapshot() []capabilitySnapshot {
	s.capabilitiesMu.Lock()
	defer s.capabilitiesMu.Unlock()
	out := make([]capabilitySnapshot, 0, len(s.capabilities))
	for name, state := range s.capabilities {
		if state == nil {
			continue
		}
		max := state.maxInflight
		if max <= 0 {
			max = defaultCapabilityMaxInflight
		}
		out = append(out, capabilitySnapshot{
			name:        name,
			inflight:    state.inflight,
			maxInflight: max,
		})
	}
	return out
}

func (s *activeSession) setActiveSessionCount(count int32, observedAt time.Time) {
	if s == nil {
		return
	}
	s.terminalCapacityMu.Lock()
	defer s.terminalCapacityMu.Unlock()
	s.terminalCapacity.activeSessionCount = int(count)
	s.terminalCapacity.observedAt = observedAt
}

func (s *activeSession) terminalSessionCapacitySnapshot() terminalSessionCapacitySnapshot {
	if s == nil {
		return terminalSessionCapacitySnapshot{}
	}
	s.terminalCapacityMu.RLock()
	defer s.terminalCapacityMu.RUnlock()
	return s.terminalCapacity
}

func (s *activeSession) tryAcquireCapability(capability string) bool {
	normalized := normalizeCapability(capability)
	if normalized == "" {
		return false
	}
	s.capabilitiesMu.Lock()
	defer s.capabilitiesMu.Unlock()
	state, ok := s.capabilities[normalized]
	if !ok || state == nil {
		return false
	}
	if state.maxInflight <= 0 {
		state.maxInflight = defaultCapabilityMaxInflight
	}
	if state.inflight >= state.maxInflight {
		return false
	}
	state.inflight++
	return true
}

func (s *activeSession) releaseCapability(capability string) {
	normalized := normalizeCapability(capability)
	if normalized == "" {
		return
	}
	s.capabilitiesMu.Lock()
	defer s.capabilitiesMu.Unlock()
	state, ok := s.capabilities[normalized]
	if !ok || state == nil {
		return
	}
	if state.inflight > 0 {
		state.inflight--
	}
}

func (s *activeSession) enqueueControl(ctx context.Context, response *registryv1.ConnectResponse) error {
	return s.enqueue(ctx, s.controlOutbound, response)
}

func (s *activeSession) enqueueCommand(ctx context.Context, response *registryv1.ConnectResponse) error {
	return s.enqueue(ctx, s.commandOutbound, response)
}

func (s *activeSession) enqueue(ctx context.Context, outbound chan<- *registryv1.ConnectResponse, response *registryv1.ConnectResponse) error {
	select {
	case <-s.done:
		return s.sessionError()
	default:
	}

	select {
	case <-s.done:
		return s.sessionError()
	case <-ctx.Done():
		return ctx.Err()
	case outbound <- response:
		return nil
	}
}

func (s *activeSession) registerPending(commandID string, capability string) (<-chan commandOutcome, error) {
	commandID = strings.TrimSpace(commandID)
	if commandID == "" {
		return nil, errors.New("command_id is required")
	}

	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	select {
	case <-s.done:
		return nil, s.sessionError()
	default:
	}

	resultCh := make(chan commandOutcome, 1)
	s.pending[commandID] = &pendingCommand{
		resultCh:   resultCh,
		capability: normalizeCapability(capability),
	}
	return resultCh, nil
}

func (s *activeSession) unregisterPending(commandID string) {
	commandID = strings.TrimSpace(commandID)
	if commandID == "" {
		return
	}

	pending, ok := func() (*pendingCommand, bool) {
		s.pendingMu.Lock()
		defer s.pendingMu.Unlock()
		pending, ok := s.pending[commandID]
		if ok {
			delete(s.pending, commandID)
		}
		return pending, ok
	}()
	if !ok || pending == nil {
		return
	}

	s.releaseCapability(pending.capability)
	pending.closeResult(nil)
}

func (s *activeSession) resolvePending(result *registryv1.CommandResult) {
	if result == nil {
		return
	}
	commandID := strings.TrimSpace(result.GetCommandId())
	if commandID == "" {
		return
	}

	pending, ok := func() (*pendingCommand, bool) {
		s.pendingMu.Lock()
		defer s.pendingMu.Unlock()
		pending, ok := s.pending[commandID]
		if ok {
			delete(s.pending, commandID)
		}
		return pending, ok
	}()
	if !ok || pending == nil {
		return
	}

	s.releaseCapability(pending.capability)

	outcome := commandOutcome{}
	if commandErr := result.GetError(); commandErr != nil {
		outcome.err = &CommandExecutionError{
			Code:    commandErr.GetCode(),
			Message: commandErr.GetMessage(),
		}
	} else if payload := result.GetPayloadJson(); len(payload) > 0 {
		outcome.payloadJSON = append([]byte(nil), payload...)
		if message, ok := parseEchoPayload(payload); ok {
			outcome.message = message
		}
	} else {
		outcome.err = &CommandExecutionError{
			Code:    "empty_result",
			Message: "worker returned empty command result",
		}
	}
	if result.GetCompletedUnixMs() > 0 {
		outcome.completedAt = time.UnixMilli(result.GetCompletedUnixMs())
	} else {
		outcome.completedAt = time.Now()
	}

	pending.closeResult(&outcome)
}

func (s *activeSession) close(err error) {
	s.closeOnce.Do(func() {
		if err == nil {
			err = errors.New(defaultCloseMessage)
		}
		s.closedErr = err
		close(s.done)

		pending := func() map[string]*pendingCommand {
			s.pendingMu.Lock()
			defer s.pendingMu.Unlock()
			pending := s.pending
			s.pending = make(map[string]*pendingCommand)
			return pending
		}()

		for _, pendingEntry := range pending {
			if pendingEntry == nil {
				continue
			}
			s.releaseCapability(pendingEntry.capability)
			outcome := commandOutcome{err: err}
			pendingEntry.closeResult(&outcome)
		}
	})
}

func (p *pendingCommand) closeResult(outcome *commandOutcome) {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		if outcome != nil {
			select {
			case p.resultCh <- *outcome:
			default:
			}
		}
		close(p.resultCh)
	})
}

func (s *activeSession) sessionError() error {
	if s.closedErr != nil {
		return s.closedErr
	}
	return errors.New(defaultCloseMessage)
}

func capabilitiesFromHello(hello *registryv1.ConnectHello) map[string]*sessionCapability {
	capabilitySet := make(map[string]*sessionCapability)
	if hello == nil {
		return capabilitySet
	}

	for _, capability := range hello.GetCapabilities() {
		if capability == nil {
			continue
		}
		name := normalizeCapability(capability.GetName())
		if name == "" {
			continue
		}
		maxInflight := int(capability.GetMaxInflight())
		if maxInflight <= 0 {
			maxInflight = defaultCapabilityMaxInflight
		}
		capabilitySet[name] = &sessionCapability{maxInflight: maxInflight}
	}

	return capabilitySet
}
