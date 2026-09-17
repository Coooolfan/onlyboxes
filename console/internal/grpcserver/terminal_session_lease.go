package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const terminalSessionLeaseRenewTimeout = 10 * time.Second

var (
	ErrTerminalSessionNotFound    = errors.New("terminal session not found")
	ErrTerminalSessionUnavailable = errors.New("terminal session is unavailable")
	ErrTerminalLeaseInvalid       = errors.New("terminal session lease is invalid")
)

// RenewTerminalSessionLease asks the bound worker to extend an active session
// and persists the absolute lease confirmed by that worker.
func (s *RegistryService) RenewTerminalSessionLease(
	ctx context.Context,
	ownerID string,
	sessionID string,
	leaseTTLSec int,
) (TerminalSessionView, error) {
	if s == nil {
		return TerminalSessionView{}, ErrTerminalSessionUnavailable
	}
	normalizedOwnerID := normalizeTaskOwnerID(ownerID)
	normalizedSessionID := strings.TrimSpace(sessionID)
	if normalizedOwnerID == "" || normalizedSessionID == "" {
		return TerminalSessionView{}, ErrTerminalSessionNotFound
	}
	if leaseTTLSec <= 0 {
		return TerminalSessionView{}, ErrTerminalLeaseInvalid
	}

	view, ok := s.GetTerminalSession(normalizedOwnerID, normalizedSessionID, s.nowFn())
	if !ok {
		return TerminalSessionView{}, ErrTerminalSessionNotFound
	}
	if view.Status != TerminalSessionStatusReady {
		return TerminalSessionView{}, ErrTerminalSessionUnavailable
	}

	scopedSessionID := scopeTerminalSessionID(normalizedOwnerID, normalizedSessionID)
	payload, err := json.Marshal(terminalLeaseRenewPayload{
		SessionID:   scopedSessionID,
		LeaseTTLSec: leaseTTLSec,
	})
	if err != nil {
		return TerminalSessionView{}, fmt.Errorf("encode terminal lease renewal: %w", err)
	}
	outcome, err := s.dispatchCommand(ctx, taskCapabilityTerminalLeaseRenew, payload, terminalSessionLeaseRenewTimeout, dispatchOptions{
		ownerID:      normalizedOwnerID,
		targetNodeID: view.WorkerID,
	})
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return TerminalSessionView{}, context.DeadlineExceeded
		case errors.Is(err, context.Canceled):
			return TerminalSessionView{}, context.Canceled
		case errors.Is(err, ErrTargetWorkerOffline),
			errors.Is(err, ErrTargetWorkerCapabilityUnavailable),
			errors.Is(err, ErrNoCapabilityWorker),
			errors.Is(err, ErrNoWorkerCapacity):
			return TerminalSessionView{}, ErrTerminalSessionUnavailable
		default:
			return TerminalSessionView{}, err
		}
	}
	if outcome.err != nil {
		switch {
		case isSessionNotFoundCommandError(outcome.err):
			return TerminalSessionView{}, ErrTerminalSessionNotFound
		case isCommandErrorCode(outcome.err, taskOwnerScopeInvalidPayloadCode):
			return TerminalSessionView{}, fmt.Errorf("%w: %v", ErrTerminalLeaseInvalid, outcome.err)
		default:
			return TerminalSessionView{}, outcome.err
		}
	}

	result := struct {
		SessionID          string `json:"session_id"`
		LeaseExpiresUnixMS int64  `json:"lease_expires_unix_ms"`
	}{}
	if err := json.Unmarshal(outcome.payloadJSON, &result); err != nil ||
		strings.TrimSpace(result.SessionID) != scopedSessionID ||
		result.LeaseExpiresUnixMS <= 0 {
		return TerminalSessionView{}, errors.New("worker returned invalid terminal lease renewal result")
	}

	updated, ok := s.GetTerminalSession(normalizedOwnerID, normalizedSessionID, s.nowFn())
	if !ok {
		return TerminalSessionView{}, errors.New("renewed terminal session route is unavailable")
	}
	return updated, nil
}
