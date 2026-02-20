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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

	outcome, err := s.dispatchCommand(ctx, echoCapabilityName, buildEchoPayload(message), timeout, nil)
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

	session, err := s.pickSessionForCapability(capability)
	if err != nil {
		return commandOutcome{}, err
	}

	commandID, err := s.newCommandIDFn()
	if err != nil {
		session.releaseCapability(capability)
		return commandOutcome{}, status.Error(codes.Internal, "failed to create command_id")
	}

	resultCh, err := session.registerPending(commandID, capability)
	if err != nil {
		session.releaseCapability(capability)
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
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			return commandOutcome{}, context.DeadlineExceeded
		}
		return commandOutcome{}, context.Canceled
	case outcome, ok := <-resultCh:
		if !ok {
			return commandOutcome{}, status.Error(codes.Unavailable, "worker session closed before command result")
		}
		return outcome, nil
	}
}

func (s *RegistryService) pickSessionForCapability(capability string) (*activeSession, error) {
	now := s.nowFn()
	nodeIDs := s.store.ListOnlineNodeIDsByCapability(capability, now, time.Duration(s.offlineTTLSec)*time.Second)
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

func normalizeCapability(capability string) string {
	return strings.TrimSpace(strings.ToLower(capability))
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
