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

type activeSession struct {
	nodeID    string
	sessionID string

	capabilitiesMu sync.Mutex
	capabilities   map[string]*sessionCapability

	controlOutbound chan *registryv1.ConnectResponse
	commandOutbound chan *registryv1.ConnectResponse
	done            chan struct{}

	pendingMu sync.Mutex
	pending   map[string]*pendingCommand

	closeOnce sync.Once
	closedErr error
}

func newActiveSession(nodeID string, sessionID string, hello *registryv1.ConnectHello) *activeSession {
	return &activeSession{
		nodeID:          nodeID,
		sessionID:       sessionID,
		capabilities:    capabilitiesFromHello(hello),
		controlOutbound: make(chan *registryv1.ConnectResponse, controlOutboundBufferSize),
		commandOutbound: make(chan *registryv1.ConnectResponse, commandOutboundBufferSize),
		done:            make(chan struct{}),
		pending:         make(map[string]*pendingCommand),
	}
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

	s.pendingMu.Lock()
	pending, ok := s.pending[commandID]
	if ok {
		delete(s.pending, commandID)
	}
	s.pendingMu.Unlock()
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

	s.pendingMu.Lock()
	pending, ok := s.pending[commandID]
	if ok {
		delete(s.pending, commandID)
	}
	s.pendingMu.Unlock()
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

		s.pendingMu.Lock()
		pending := s.pending
		s.pending = make(map[string]*pendingCommand)
		s.pendingMu.Unlock()

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
