package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	terminalSessionNotFoundCode         = "session_not_found"
	terminalSessionCapacityExceededCode = "session_capacity_exceeded"
)

type CommandExecutionError struct {
	Code    string
	Message string
}

func (e *CommandExecutionError) Error() string {
	if e == nil {
		return "command execution failed"
	}
	trimmedCode := strings.TrimSpace(e.Code)
	trimmedMessage := strings.TrimSpace(e.Message)
	if trimmedCode == "" && trimmedMessage == "" {
		return "command execution failed"
	}
	if trimmedCode == "" {
		return trimmedMessage
	}
	if trimmedMessage == "" {
		return trimmedCode
	}
	return fmt.Sprintf("%s: %s", trimmedCode, trimmedMessage)
}

func (s *RegistryService) DispatchEcho(ctx context.Context, message string, timeout time.Duration) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", status.Error(codes.InvalidArgument, "message is required")
	}
	if timeout <= 0 {
		timeout = defaultEchoTimeout
	}

	outcome, err := s.dispatchCommand(ctx, echoCapabilityName, buildEchoPayload(message), timeout, "", nil)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoCapabilityWorker):
			return "", ErrNoEchoWorker
		case errors.Is(err, ErrNoWorkerCapacity):
			return "", ErrNoWorkerCapacity
		case errors.Is(err, context.DeadlineExceeded):
			return "", ErrEchoTimeout
		default:
			return "", err
		}
	}
	if outcome.err != nil {
		return "", outcome.err
	}

	if message, ok := parseEchoPayload(outcome.payloadJSON); ok {
		return message, nil
	}
	if strings.TrimSpace(outcome.message) != "" {
		return outcome.message, nil
	}
	return "", &CommandExecutionError{
		Code:    "empty_result",
		Message: "worker returned empty echo result",
	}
}

func (s *RegistryService) dispatchCommand(
	ctx context.Context,
	capability string,
	payloadJSON []byte,
	timeout time.Duration,
	ownerID string,
	onDispatched func(commandID string),
) (commandOutcome, error) {
	capability = normalizeCapability(capability)
	if capability == "" {
		return commandOutcome{}, status.Error(codes.InvalidArgument, "capability is required")
	}
	if len(payloadJSON) == 0 {
		payloadJSON = []byte("{}")
	}

	commandCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		commandCtx, cancel = context.WithTimeout(ctx, timeout)
	} else if timeout < 0 {
		commandCtx, cancel = context.WithTimeout(ctx, defaultCommandDispatchTimeout)
	}
	defer cancel()

	terminalSessionID := terminalSessionIDFromPayload(capability, payloadJSON)
	session, terminalRouteReservationID, err := s.pickSessionForDispatch(capability, ownerID, terminalSessionID)
	if err != nil {
		return commandOutcome{}, err
	}
	rollbackTerminalRouteReservation := func() {
		if terminalRouteReservationID != 0 && terminalSessionID != "" {
			s.clearTerminalSessionRouteReservation(terminalSessionID, session.nodeID, terminalRouteReservationID)
		}
	}
	confirmTerminalRoute := func() {
		if terminalSessionID != "" {
			s.confirmTerminalSessionRoute(
				terminalSessionID,
				session.nodeID,
				terminalRouteReservationID,
				s.nowFn(),
			)
		}
	}

	commandID, err := s.newCommandIDFn()
	if err != nil {
		session.releaseCapability(capability)
		rollbackTerminalRouteReservation()
		return commandOutcome{}, status.Error(codes.Internal, "failed to create command_id")
	}

	resultCh, err := session.registerPending(commandID, capability)
	if err != nil {
		session.releaseCapability(capability)
		rollbackTerminalRouteReservation()
		return commandOutcome{}, err
	}
	// Always release pending state, even when enqueue succeeds and the caller
	// context is canceled before a worker result arrives.
	defer session.unregisterPending(commandID)

	dispatch := &registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_CommandDispatch{
			CommandDispatch: &registryv1.CommandDispatch{
				CommandId:   commandID,
				Capability:  capability,
				PayloadJson: payloadJSON,
			},
		},
	}
	if deadline, ok := commandCtx.Deadline(); ok {
		dispatch.GetCommandDispatch().DeadlineUnixMs = deadline.UnixMilli()
	}

	if err := session.enqueueCommand(commandCtx, dispatch); err != nil {
		rollbackTerminalRouteReservation()
		if errors.Is(err, context.DeadlineExceeded) {
			return commandOutcome{}, context.DeadlineExceeded
		}
		if errors.Is(err, context.Canceled) {
			return commandOutcome{}, context.Canceled
		}
		if mapped := status.FromContextError(err); mapped.Code() != codes.Unknown {
			return commandOutcome{}, mapped.Err()
		}
		if status.Code(err) != codes.Unknown {
			return commandOutcome{}, err
		}
		return commandOutcome{}, status.Error(codes.Unavailable, "worker session unavailable")
	}
	if onDispatched != nil {
		onDispatched(commandID)
	}

	select {
	case <-commandCtx.Done():
		if terminalRouteReservationID != 0 && terminalSessionID != "" {
			// The dispatch reached the worker stream, so cancellation does not
			// prove that session creation failed.
			confirmTerminalRoute()
		}
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			return commandOutcome{}, context.DeadlineExceeded
		}
		return commandOutcome{}, context.Canceled
	case outcome, ok := <-resultCh:
		if !ok {
			rollbackTerminalRouteReservation()
			return commandOutcome{}, status.Error(codes.Unavailable, "worker session closed before command result")
		}
		if outcome.err == nil && terminalSessionID != "" {
			confirmTerminalRoute()
		}
		if outcome.err != nil && terminalSessionID != "" {
			if isSessionNotFoundCommandError(outcome.err) {
				if terminalRouteReservationID != 0 {
					rollbackTerminalRouteReservation()
				} else {
					s.clearTerminalSessionRoute(terminalSessionID, session.nodeID)
				}
			} else if isSessionCapacityCommandError(outcome.err) {
				rollbackTerminalRouteReservation()
			} else if terminalRouteReservationID != 0 {
				// Other execution errors do not prove that session creation failed.
				confirmTerminalRoute()
			}
		}
		return outcome, nil
	}
}

func (s *RegistryService) pickSessionForDispatch(capability string, ownerID string, terminalSessionID string) (*activeSession, uint64, error) {
	normalizedTerminalSessionID := strings.TrimSpace(terminalSessionID)
	if normalizedTerminalSessionID == "" {
		session, err := s.pickSessionForCapability(capability, ownerID)
		return session, 0, err
	}
	now := s.nowFn()
	s.maybePruneTerminalSessionRoutes(now)

	nodeID, reservationID, ok := s.claimTerminalSessionRoute(normalizedTerminalSessionID, now)
	if !ok {
		return s.tryReserveAndPickTerminalSession(capability, ownerID, normalizedTerminalSessionID, now)
	}

	session, err := s.pickSessionForNodeAndCapability(nodeID, capability)
	if err == nil {
		return session, reservationID, nil
	}
	if errors.Is(err, ErrNoCapabilityWorker) {
		s.clearTerminalSessionRoute(normalizedTerminalSessionID, nodeID)
		return s.tryReserveAndPickTerminalSession(capability, ownerID, normalizedTerminalSessionID, now)
	}
	s.clearTerminalSessionRouteReservation(normalizedTerminalSessionID, nodeID, reservationID)
	return nil, 0, err
}

func (s *RegistryService) tryReserveAndPickTerminalSession(
	capability string,
	ownerID string,
	normalizedTerminalSessionID string,
	now time.Time,
) (*activeSession, uint64, error) {
	session, err := s.pickSessionForCapability(capability, ownerID)
	if err != nil {
		return nil, 0, err
	}

	resolvedNodeID, reservationID := s.reserveTerminalSessionRoute(normalizedTerminalSessionID, session.nodeID, now)
	if resolvedNodeID == session.nodeID {
		return session, reservationID, nil
	}

	// Another request reserved this session first; route to that node for consistency.
	session.releaseCapability(capability)

	session, err = s.pickSessionForNodeAndCapability(resolvedNodeID, capability)
	if err == nil {
		return session, reservationID, nil
	}
	if !errors.Is(err, ErrNoCapabilityWorker) {
		s.clearTerminalSessionRouteReservation(normalizedTerminalSessionID, resolvedNodeID, reservationID)
		return nil, 0, err
	}

	// The reserved route became stale before we could acquire it; clear and retry once.
	s.clearTerminalSessionRoute(normalizedTerminalSessionID, resolvedNodeID)

	session, err = s.pickSessionForCapability(capability, ownerID)
	if err != nil {
		return nil, 0, err
	}

	resolvedNodeID, reservationID = s.reserveTerminalSessionRoute(normalizedTerminalSessionID, session.nodeID, now)
	if resolvedNodeID == session.nodeID {
		return session, reservationID, nil
	}

	session.releaseCapability(capability)
	session, err = s.pickSessionForNodeAndCapability(resolvedNodeID, capability)
	if err != nil {
		if errors.Is(err, ErrNoCapabilityWorker) {
			s.clearTerminalSessionRoute(normalizedTerminalSessionID, resolvedNodeID)
		} else {
			s.clearTerminalSessionRouteReservation(normalizedTerminalSessionID, resolvedNodeID, reservationID)
		}
		return nil, 0, err
	}
	return session, reservationID, nil
}

func (s *RegistryService) pickSessionForNodeAndCapability(nodeID string, capability string) (*activeSession, error) {
	normalizedNodeID := strings.TrimSpace(nodeID)
	if normalizedNodeID == "" {
		return nil, ErrNoCapabilityWorker
	}

	session := s.getSession(normalizedNodeID)
	if session == nil || !session.hasCapability(capability) {
		return nil, ErrNoCapabilityWorker
	}

	if !session.tryAcquireCapability(capability) {
		return nil, ErrNoWorkerCapacity
	}
	return session, nil
}

func (s *RegistryService) pickSessionForCapability(capability string, ownerID string) (*activeSession, error) {
	nodeIDs := s.listOnlineNodeIDsForCapability(capability, ownerID)
	if len(nodeIDs) == 0 {
		return nil, ErrNoCapabilityWorker
	}

	start := int(atomic.AddUint64(&s.roundRobin, 1) - 1)
	type candidate struct {
		session  *activeSession
		inflight int
	}
	minInflight := int(^uint(0) >> 1)
	preferred := make([]candidate, 0, len(nodeIDs))
	fallback := make([]candidate, 0, len(nodeIDs))
	hasSession := false

	for i := 0; i < len(nodeIDs); i++ {
		index := (start + i) % len(nodeIDs)
		session := s.getSession(nodeIDs[index])
		if session == nil || !session.hasCapability(capability) {
			continue
		}
		hasSession = true
		inflight, maxInflight, ok := session.inflightSnapshot(capability)
		if !ok || inflight >= maxInflight {
			continue
		}
		cand := candidate{session: session, inflight: inflight}
		if inflight < minInflight {
			minInflight = inflight
			preferred = preferred[:0]
			preferred = append(preferred, cand)
		} else if inflight == minInflight {
			preferred = append(preferred, cand)
		} else {
			fallback = append(fallback, cand)
		}
	}

	if len(preferred) == 0 {
		if hasSession {
			return nil, ErrNoWorkerCapacity
		}
		return nil, ErrNoCapabilityWorker
	}

	for i := 0; i < len(preferred); i++ {
		session := preferred[i].session
		if session.tryAcquireCapability(capability) {
			return session, nil
		}
	}
	for _, cand := range fallback {
		if cand.session.tryAcquireCapability(capability) {
			return cand.session, nil
		}
	}
	return nil, ErrNoWorkerCapacity
}

func (s *RegistryService) listOnlineNodeIDsForCapability(capability string, ownerID string) []string {
	now := s.nowFn()
	offlineTTL := time.Duration(s.offlineTTLSec) * time.Second
	normalizedCapability := normalizeCapability(capability)
	if normalizedCapability == computerUseCapabilityName || normalizedCapability == readImageCapabilityName {
		normalizedOwnerID := normalizeTaskOwnerID(ownerID)
		if normalizedOwnerID == "" {
			return []string{}
		}
		return s.store.ListOnlineNodeIDsByOwnerTypeAndCapability(
			normalizedOwnerID,
			registry.WorkerTypeSys,
			normalizedCapability,
			now,
			offlineTTL,
		)
	}
	return s.store.ListOnlineNodeIDsByCapability(normalizedCapability, now, offlineTTL)
}

func normalizeCapability(capability string) string {
	return strings.TrimSpace(strings.ToLower(capability))
}

func isSessionNotFoundCommandError(err error) bool {
	return isCommandErrorCode(err, terminalSessionNotFoundCode)
}

func isSessionCapacityCommandError(err error) bool {
	return isCommandErrorCode(err, terminalSessionCapacityExceededCode)
}

func isCommandErrorCode(err error, code string) bool {
	var commandErr *CommandExecutionError
	if !errors.As(err, &commandErr) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(commandErr.Code), code)
}

func terminalSessionIDFromPayload(capability string, payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	switch capability {
	case taskCapabilityTerminalExec:
		var decoded terminalExecScopedPayload
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return ""
		}
		return strings.TrimSpace(decoded.SessionID)
	case taskCapabilityTerminalResource:
		var decoded terminalResourceScopedPayload
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return ""
		}
		return strings.TrimSpace(decoded.SessionID)
	default:
		return ""
	}
}

func parseEchoPayload(payload []byte) (string, bool) {
	if len(payload) == 0 {
		return "", false
	}
	var decoded struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", false
	}
	if strings.TrimSpace(decoded.Message) == "" {
		return "", false
	}
	return decoded.Message, true
}

// CapabilityInflightEntry holds the inflight snapshot for a single capability.
type CapabilityInflightEntry struct {
	Name        string
	Inflight    int
	MaxInflight int
}

// WorkerInflightSnapshot holds the inflight snapshot for a single worker.
type WorkerInflightSnapshot struct {
	NodeID             string
	ActiveSessionCount int
	Capabilities       []CapabilityInflightEntry
}

// InflightStats returns inflight data for all active sessions.
func (s *RegistryService) InflightStats() []WorkerInflightSnapshot {
	sessions := func() map[string]*activeSession {
		s.sessionsMu.RLock()
		defer s.sessionsMu.RUnlock()
		sessions := make(map[string]*activeSession, len(s.sessions))
		for k, v := range s.sessions {
			sessions[k] = v
		}
		return sessions
	}()

	out := make([]WorkerInflightSnapshot, 0, len(sessions))
	for _, session := range sessions {
		caps := session.allCapabilitiesSnapshot()
		entries := make([]CapabilityInflightEntry, len(caps))
		for i, c := range caps {
			entries[i] = CapabilityInflightEntry{
				Name:        c.name,
				Inflight:    c.inflight,
				MaxInflight: c.maxInflight,
			}
		}
		out = append(out, WorkerInflightSnapshot{
			NodeID:             session.nodeID,
			ActiveSessionCount: int(session.activeSessionCount()),
			Capabilities:       entries,
		})
	}
	return out
}

func buildEchoPayload(message string) []byte {
	encoded, err := json.Marshal(struct {
		Message string `json:"message"`
	}{
		Message: message,
	})
	if err != nil {
		return []byte(`{"message":"` + message + `"}`)
	}
	return encoded
}
