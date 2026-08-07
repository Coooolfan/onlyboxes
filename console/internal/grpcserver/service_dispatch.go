package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
	terminalSessionUnavailableCode      = "session_unavailable"
)

type dispatchOptions struct {
	ownerID               string
	taskID                string
	terminalSessionIntent terminalSessionIntent
	onDispatched          func(commandID string) error
}

type sessionPickOptions struct {
	terminalSessionIntent terminalSessionIntent
	excludedNodeIDs       map[string]struct{}
	taskID                string
}

type dispatchAttemptResult struct {
	outcome           commandOutcome
	capacityRetryable bool
}

var errTerminalSessionRetryRouteConflict = errors.New("terminal session route points to an attempted worker")

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

	outcome, err := s.dispatchCommand(ctx, echoCapabilityName, buildEchoPayload(message), timeout, dispatchOptions{})
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
	options dispatchOptions,
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
	maxAttempts := 1
	if capability == taskCapabilityTerminalExec && terminalSessionID != "" {
		maxAttempts = len(s.listOnlineNodeIDsForCapability(capability, options.ownerID))
		if maxAttempts < 1 {
			maxAttempts = 1
		}
	}

	attemptedNodeIDs := make(map[string]struct{}, maxAttempts)
	pickOptions := sessionPickOptions{
		terminalSessionIntent: options.terminalSessionIntent,
		excludedNodeIDs:       attemptedNodeIDs,
		taskID:                options.taskID,
	}
	var lastCapacityOutcome *commandOutcome

	for attemptIndex := 1; attemptIndex <= maxAttempts; attemptIndex++ {
		if err := commandCtx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return commandOutcome{}, context.DeadlineExceeded
			}
			return commandOutcome{}, context.Canceled
		}

		session, terminalRouteReservationID, err := s.pickSessionForDispatch(
			capability,
			options.ownerID,
			terminalSessionID,
			pickOptions,
		)
		if err != nil {
			if commandErr := commandCtx.Err(); commandErr != nil {
				if errors.Is(commandErr, context.DeadlineExceeded) {
					return commandOutcome{}, context.DeadlineExceeded
				}
				return commandOutcome{}, context.Canceled
			}
			if lastCapacityOutcome != nil {
				slog.Info(
					"terminal capacity retry exhausted",
					"task_id", options.taskID,
					"attempted_node_count", len(attemptedNodeIDs),
				)
				return *lastCapacityOutcome, nil
			}
			return commandOutcome{}, err
		}
		attemptedNodeIDs[session.nodeID] = struct{}{}

		attemptResult, err := s.dispatchCommandAttempt(
			commandCtx,
			capability,
			payloadJSON,
			terminalSessionID,
			session,
			terminalRouteReservationID,
			options.onDispatched,
		)
		if err != nil {
			return commandOutcome{}, err
		}
		if !attemptResult.capacityRetryable {
			return attemptResult.outcome, nil
		}

		capacityOutcome := attemptResult.outcome
		lastCapacityOutcome = &capacityOutcome
		if attemptIndex >= maxAttempts {
			slog.Info(
				"terminal capacity retry exhausted",
				"task_id", options.taskID,
				"attempted_node_count", len(attemptedNodeIDs),
			)
			return capacityOutcome, nil
		}
		slog.Info(
			"terminal capacity retry",
			"task_id", options.taskID,
			"previous_node_id", session.nodeID,
			"next_attempt", attemptIndex+1,
		)
	}

	if lastCapacityOutcome != nil {
		return *lastCapacityOutcome, nil
	}
	return commandOutcome{}, ErrNoCapabilityWorker
}

func (s *RegistryService) dispatchCommandAttempt(
	commandCtx context.Context,
	capability string,
	payloadJSON []byte,
	terminalSessionID string,
	session *activeSession,
	terminalRouteReservationID uint64,
	onDispatched func(commandID string) error,
) (dispatchAttemptResult, error) {
	rollbackTerminalRouteReservation := func() routeReservationReleaseResult {
		if terminalRouteReservationID == 0 || terminalSessionID == "" {
			return routeReservationNotOwned
		}
		return s.clearTerminalSessionRouteReservation(
			terminalSessionID,
			session.nodeID,
			terminalRouteReservationID,
		)
	}
	confirmTerminalRoute := func(resultPayload []byte) {
		if terminalSessionID != "" {
			s.confirmTerminalSessionRoute(
				terminalSessionID,
				session.nodeID,
				terminalRouteReservationID,
				s.nowFn(),
			)
			if leaseExpiresUnixMs := terminalSessionLeaseExpiresUnixMs(resultPayload); leaseExpiresUnixMs > 0 {
				s.updateTerminalSessionRouteLease(terminalSessionID, session.nodeID, leaseExpiresUnixMs, s.nowFn())
			}
		}
	}

	commandID, err := s.newCommandIDFn()
	if err != nil {
		session.releaseCapability(capability)
		rollbackTerminalRouteReservation()
		return dispatchAttemptResult{}, status.Error(codes.Internal, "failed to create command_id")
	}

	resultCh, err := session.registerPending(commandID, capability)
	if err != nil {
		session.releaseCapability(capability)
		rollbackTerminalRouteReservation()
		return dispatchAttemptResult{}, err
	}
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
			return dispatchAttemptResult{}, context.DeadlineExceeded
		}
		if errors.Is(err, context.Canceled) {
			return dispatchAttemptResult{}, context.Canceled
		}
		if mapped := status.FromContextError(err); mapped.Code() != codes.Unknown {
			return dispatchAttemptResult{}, mapped.Err()
		}
		if status.Code(err) != codes.Unknown {
			return dispatchAttemptResult{}, err
		}
		return dispatchAttemptResult{}, status.Error(codes.Unavailable, "worker session unavailable")
	}
	if onDispatched != nil {
		if err := onDispatched(commandID); err != nil {
			if terminalRouteReservationID != 0 {
				confirmTerminalRoute(nil)
			}
			return dispatchAttemptResult{}, err
		}
	}

	select {
	case <-commandCtx.Done():
		if terminalRouteReservationID != 0 && terminalSessionID != "" {
			// The dispatch reached the worker stream, so cancellation does not
			// prove that session creation failed.
			confirmTerminalRoute(nil)
		}
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			return dispatchAttemptResult{}, context.DeadlineExceeded
		}
		return dispatchAttemptResult{}, context.Canceled
	case outcome, ok := <-resultCh:
		if !ok {
			rollbackTerminalRouteReservation()
			return dispatchAttemptResult{}, status.Error(codes.Unavailable, "worker session closed before command result")
		}
		if outcome.err == nil && terminalSessionID != "" {
			confirmTerminalRoute(outcome.payloadJSON)
			return dispatchAttemptResult{outcome: outcome}, nil
		}
		if outcome.err == nil || terminalSessionID == "" {
			return dispatchAttemptResult{outcome: outcome}, nil
		}

		switch {
		case isSessionNotFoundCommandError(outcome.err):
			if terminalRouteReservationID != 0 {
				rollbackTerminalRouteReservation()
			} else {
				s.clearTerminalSessionRoute(terminalSessionID, session.nodeID)
			}
		case isSessionCapacityCommandError(outcome.err):
			releaseResult := rollbackTerminalRouteReservation()
			return dispatchAttemptResult{
				outcome: outcome,
				capacityRetryable: capability == taskCapabilityTerminalExec &&
					terminalRouteReservationID != 0 &&
					releaseResult == routeReservationRemoved,
			}, nil
		case terminalRouteReservationID != 0:
			// Other execution errors do not prove that session creation failed.
			confirmTerminalRoute(nil)
		}
		return dispatchAttemptResult{outcome: outcome}, nil
	}
}

func (s *RegistryService) pickSessionForDispatch(
	capability string,
	ownerID string,
	terminalSessionID string,
	options sessionPickOptions,
) (*activeSession, uint64, error) {
	normalizedTerminalSessionID := strings.TrimSpace(terminalSessionID)
	if normalizedTerminalSessionID == "" {
		session, err := s.pickSessionForCapability(capability, ownerID, options)
		return session, 0, err
	}
	now := s.nowFn()
	s.maybePruneTerminalSessionRoutes(now)
	if route, exists := s.terminalSessionRouteSnapshot(normalizedTerminalSessionID, now); exists && route.ReservationID == 0 {
		boundSession := s.getSession(route.NodeID)
		if route.RecoveryState != terminalSessionRecoveryReady || boundSession == nil || !boundSession.isReady() {
			return nil, 0, terminalSessionUnavailableError()
		}
	}

	nodeID, reservationID, ok := s.claimTerminalSessionRoute(normalizedTerminalSessionID, now)
	if !ok {
		return s.tryReserveAndPickTerminalSession(capability, ownerID, normalizedTerminalSessionID, now, options)
	}
	if isNodeExcluded(nodeID, options.excludedNodeIDs) {
		if reservationID != 0 {
			s.clearTerminalSessionRouteReservation(normalizedTerminalSessionID, nodeID, reservationID)
		}
		return nil, 0, errTerminalSessionRetryRouteConflict
	}

	session, err := s.pickSessionForNodeAndCapability(nodeID, capability)
	if err == nil {
		return session, reservationID, nil
	}
	if errors.Is(err, ErrNoCapabilityWorker) {
		if reservationID == 0 {
			return nil, 0, terminalSessionUnavailableError()
		}
		s.clearTerminalSessionRoute(normalizedTerminalSessionID, nodeID)
		return s.tryReserveAndPickTerminalSession(capability, ownerID, normalizedTerminalSessionID, now, options)
	}
	s.clearTerminalSessionRouteReservation(normalizedTerminalSessionID, nodeID, reservationID)
	return nil, 0, err
}

func (s *RegistryService) tryReserveAndPickTerminalSession(
	capability string,
	ownerID string,
	normalizedTerminalSessionID string,
	now time.Time,
	options sessionPickOptions,
) (*activeSession, uint64, error) {
	for reserveAttempt := 0; reserveAttempt < 2; reserveAttempt++ {
		session, err := s.pickSessionForCapability(capability, ownerID, options)
		if err != nil {
			return nil, 0, err
		}

		resolvedNodeID, reservationID := s.reserveTerminalSessionRoute(normalizedTerminalSessionID, session.nodeID, now)
		if resolvedNodeID == session.nodeID {
			return session, reservationID, nil
		}

		// Another request reserved this session first; follow that node for consistency.
		session.releaseCapability(capability)
		if isNodeExcluded(resolvedNodeID, options.excludedNodeIDs) {
			if reservationID != 0 {
				s.clearTerminalSessionRouteReservation(normalizedTerminalSessionID, resolvedNodeID, reservationID)
			}
			return nil, 0, errTerminalSessionRetryRouteConflict
		}

		session, err = s.pickSessionForNodeAndCapability(resolvedNodeID, capability)
		if err == nil {
			return session, reservationID, nil
		}
		if !errors.Is(err, ErrNoCapabilityWorker) {
			s.clearTerminalSessionRouteReservation(normalizedTerminalSessionID, resolvedNodeID, reservationID)
			return nil, 0, err
		}

		// The reserved route became stale before acquisition. Clear it and retry once.
		s.clearTerminalSessionRoute(normalizedTerminalSessionID, resolvedNodeID)
	}
	return nil, 0, ErrNoCapabilityWorker
}

func isNodeExcluded(nodeID string, excludedNodeIDs map[string]struct{}) bool {
	if len(excludedNodeIDs) == 0 {
		return false
	}
	_, excluded := excludedNodeIDs[strings.TrimSpace(nodeID)]
	return excluded
}

func (s *RegistryService) pickSessionForNodeAndCapability(nodeID string, capability string) (*activeSession, error) {
	normalizedNodeID := strings.TrimSpace(nodeID)
	if normalizedNodeID == "" {
		return nil, ErrNoCapabilityWorker
	}

	session := s.getSession(normalizedNodeID)
	if session == nil || !session.isReady() || !session.hasCapability(capability) {
		return nil, ErrNoCapabilityWorker
	}

	if !session.tryAcquireCapability(capability) {
		return nil, ErrNoWorkerCapacity
	}
	return session, nil
}

func (s *RegistryService) pickSessionForCapability(
	capability string,
	ownerID string,
	options sessionPickOptions,
) (*activeSession, error) {
	nodeIDs := s.listOnlineNodeIDsForCapability(capability, ownerID)
	if len(nodeIDs) == 0 {
		return nil, ErrNoCapabilityWorker
	}

	const (
		capacityGroupKnownAvailable = iota
		capacityGroupUnknown
		capacityGroupReportedFull
		capacityGroupCount
	)
	type candidate struct {
		session          *activeSession
		inflight         int
		terminalCapacity terminalSessionCapacitySnapshot
	}

	start := int(atomic.AddUint64(&s.roundRobin, 1) - 1)
	groups := make([][]candidate, capacityGroupCount)
	hasSession := false
	capacityAware := normalizeCapability(capability) == taskCapabilityTerminalExec &&
		options.terminalSessionIntent == terminalSessionIntentKnownNew

	for i := 0; i < len(nodeIDs); i++ {
		index := (start + i) % len(nodeIDs)
		nodeID := nodeIDs[index]
		if isNodeExcluded(nodeID, options.excludedNodeIDs) {
			continue
		}
		session := s.getSession(nodeID)
		if session == nil || !session.isReady() || !session.hasCapability(capability) {
			continue
		}
		hasSession = true
		inflight, maxInflight, ok := session.inflightSnapshot(capability)
		if !ok || inflight >= maxInflight {
			continue
		}

		terminalCapacity := session.terminalSessionCapacitySnapshot()
		group := capacityGroupKnownAvailable
		if capacityAware {
			switch {
			case !terminalCapacity.known:
				group = capacityGroupUnknown
			case terminalCapacity.maxActiveSessions > 0 &&
				terminalCapacity.activeSessionCount >= terminalCapacity.maxActiveSessions:
				group = capacityGroupReportedFull
			}
		}
		groups[group] = append(groups[group], candidate{
			session:          session,
			inflight:         inflight,
			terminalCapacity: terminalCapacity,
		})
	}

	for groupIndex, candidates := range groups {
		sort.SliceStable(candidates, func(i, j int) bool {
			return candidates[i].inflight < candidates[j].inflight
		})
		for _, candidate := range candidates {
			if !candidate.session.tryAcquireCapability(capability) {
				continue
			}
			if capacityAware && groupIndex < capacityGroupReportedFull {
				for _, skipped := range groups[capacityGroupReportedFull] {
					slog.Debug(
						"terminal capacity candidate skipped",
						"task_id", options.taskID,
						"node_id", skipped.session.nodeID,
						"active_session_count", skipped.terminalCapacity.activeSessionCount,
						"max_active_sessions", skipped.terminalCapacity.maxActiveSessions,
					)
				}
			}
			return candidate.session, nil
		}
	}

	if hasSession {
		return nil, ErrNoWorkerCapacity
	}
	return nil, ErrNoCapabilityWorker
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

func terminalSessionUnavailableError() error {
	return &CommandExecutionError{
		Code:    terminalSessionUnavailableCode,
		Message: "terminal session is temporarily unavailable",
	}
}

func terminalSessionLeaseExpiresUnixMs(payload []byte) int64 {
	if len(payload) == 0 {
		return 0
	}
	var decoded struct {
		LeaseExpiresUnixMS int64 `json:"lease_expires_unix_ms"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.LeaseExpiresUnixMS <= 0 {
		return 0
	}
	return decoded.LeaseExpiresUnixMS
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

// TerminalSessionCapacityInflightEntry describes whether a worker declared a
// terminal session limit and, when known, its configured maximum.
type TerminalSessionCapacityInflightEntry struct {
	Known             bool
	MaxActiveSessions int
}

// WorkerInflightSnapshot holds the inflight snapshot for a single worker.
type WorkerInflightSnapshot struct {
	NodeID                  string
	ActiveSessionCount      int
	TerminalSessionCapacity TerminalSessionCapacityInflightEntry
	Capabilities            []CapabilityInflightEntry
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
		capacity := session.terminalSessionCapacitySnapshot()
		out = append(out, WorkerInflightSnapshot{
			NodeID:             session.nodeID,
			ActiveSessionCount: capacity.activeSessionCount,
			TerminalSessionCapacity: TerminalSessionCapacityInflightEntry{
				Known:             capacity.known,
				MaxActiveSessions: capacity.maxActiveSessions,
			},
			Capabilities: entries,
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
