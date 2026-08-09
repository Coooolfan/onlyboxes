package grpcserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"strings"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *RegistryService) Connect(stream grpc.BidiStreamingServer[registryv1.ConnectRequest, registryv1.ConnectResponse]) (retErr error) {
	if err := stream.Context().Err(); err != nil {
		return status.FromContextError(err).Err()
	}

	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return status.Error(codes.InvalidArgument, "first frame must be hello")
		}
		return mapStreamError(err)
	}

	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first frame must be hello")
	}
	if err := validateHello(hello); err != nil {
		return err
	}

	secret, ok := s.getCredential(hello.GetNodeId())
	if !ok {
		return status.Error(codes.Unauthenticated, "unknown worker_id")
	}

	workerSecret := strings.TrimSpace(hello.GetWorkerSecret())
	if workerSecret == "" {
		return status.Error(codes.Unauthenticated, "worker_secret is required")
	}
	hasher := func() *persistence.Hasher {
		s.credentialsMu.RLock()
		defer s.credentialsMu.RUnlock()
		return s.hasher
	}()
	if hasher != nil {
		if !hasher.Equal(secret, workerSecret) {
			return status.Error(codes.Unauthenticated, "invalid worker credential")
		}
	} else if subtle.ConstantTimeCompare([]byte(secret), []byte(workerSecret)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid worker credential")
	}
	resolvedHello, err := s.resolveHelloByWorkerType(hello)
	if err != nil {
		return err
	}
	hello = resolvedHello

	now := s.nowFn()
	sessionID, err := s.newSessionIDFn()
	if err != nil {
		return status.Error(codes.Internal, "failed to create session_id")
	}

	session := newActiveSessionAt(hello.GetNodeId(), sessionID, hello, now)
	if err := s.configureSessionProxy(session, hello, workerSecret); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid proxy endpoint: %v", err)
	}
	recoveryCandidates, err := s.beginTerminalSessionRecoveryWithError(session.nodeID, now)
	if err != nil {
		return status.Errorf(codes.Internal, "prepare terminal session recovery: %v", err)
	}
	session.setRecoveryCandidates(recoveryCandidates)
	logTerminalSessionCapacityInvariant(session.nodeID, session.terminalSessionCapacitySnapshot())
	replaced := s.swapSession(session)
	if replaced != nil {
		replaced.close(status.Error(codes.FailedPrecondition, "session replaced by a newer connection"))
	}
	defer func() {
		s.removeSession(session)
		session.close(retErr)
	}()

	if err := s.store.Upsert(hello, sessionID, now); err != nil {
		return status.Error(codes.Internal, "failed to persist worker registration")
	}

	writerErrCh := make(chan error, 1)
	go func() {
		writerErrCh <- writerLoop(stream, session)
	}()

	if err := session.enqueueControl(stream.Context(), newConnectAck(sessionID, s.heartbeatIntervalSec, recoveryCandidates...)); err != nil {
		return status.Error(codes.Internal, "failed to send connect ack")
	}

	for {
		select {
		case err := <-writerErrCh:
			if err == nil {
				return nil
			}
			return mapStreamError(err)
		default:
		}

		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return mapStreamError(err)
		}

		switch {
		case req.GetHeartbeat() != nil:
			heartbeatCtx, cancel := context.WithTimeout(stream.Context(), heartbeatAckEnqueueTimeout)
			err := s.handleHeartbeat(heartbeatCtx, session, req.GetHeartbeat())
			cancel()
			if err != nil {
				return err
			}
		case req.GetCommandResult() != nil:
			if err := handleCommandResult(session, req.GetCommandResult()); err != nil {
				return err
			}
		case req.GetTerminalSessionRecoveryReport() != nil:
			if !session.recoveryRequired {
				return status.Error(codes.InvalidArgument, "terminal session recovery is not supported by this worker")
			}
			if session.isReady() {
				if !session.matchesRecoveryResults(req.GetTerminalSessionRecoveryReport()) {
					return status.Error(codes.FailedPrecondition, "terminal session recovery report changed after completion")
				}
				if err := session.enqueueControl(stream.Context(), newTerminalSessionRecoveryAck()); err != nil {
					return status.Error(codes.Internal, "failed to resend terminal session recovery ack")
				}
				continue
			}
			if err := s.applyTerminalSessionRecoveryReport(session, req.GetTerminalSessionRecoveryReport(), s.nowFn()); err != nil {
				return status.Error(codes.InvalidArgument, err.Error())
			}
			if err := session.enqueueControl(stream.Context(), newTerminalSessionRecoveryAck()); err != nil {
				return status.Error(codes.Internal, "failed to send terminal session recovery ack")
			}
			session.markRecoveryComplete()
		default:
			return status.Error(codes.InvalidArgument, "unsupported frame type")
		}
	}
}

func (s *RegistryService) resolveHelloByWorkerType(hello *registryv1.ConnectHello) (*registryv1.ConnectHello, error) {
	if hello == nil {
		return nil, status.Error(codes.InvalidArgument, "hello frame is required")
	}
	if s == nil || s.store == nil {
		return hello, nil
	}

	workerType := s.store.WorkerTypeByNodeID(hello.GetNodeId())
	if workerType != registry.WorkerTypeSys {
		return hello, nil
	}

	hasComputerUse := false
	hasReadImage := false
	computerUseMaxInflight := 0
	readImageMaxInflight := 0
	for _, capability := range hello.GetCapabilities() {
		if capability == nil {
			continue
		}
		switch normalizeCapability(capability.GetName()) {
		case computerUseCapabilityName:
			hasComputerUse = true
			computerUseMaxInflight = int(capability.GetMaxInflight())
		case readImageCapabilityName:
			hasReadImage = true
			readImageMaxInflight = int(capability.GetMaxInflight())
		default:
			return nil, status.Error(codes.PermissionDenied, "worker-sys supports only computerUse and readImage capabilities")
		}
	}
	if !hasComputerUse || !hasReadImage {
		return nil, status.Error(codes.PermissionDenied, "worker-sys requires computerUse and readImage capabilities")
	}

	labels := cloneLabels(hello.GetLabels())
	return &registryv1.ConnectHello{
		NodeId:       hello.GetNodeId(),
		NodeName:     hello.GetNodeName(),
		ExecutorKind: hello.GetExecutorKind(),
		Labels:       labels,
		Version:      hello.GetVersion(),
		WorkerSecret: hello.GetWorkerSecret(),
		Capabilities: []*registryv1.CapabilityDeclaration{
			{
				Name:        computerUseCapabilityDeclared,
				MaxInflight: int32(sysCapabilityMaxInflight(computerUseMaxInflight)),
			},
			{
				Name:        readImageCapabilityDeclared,
				MaxInflight: int32(sysCapabilityMaxInflight(readImageMaxInflight)),
			},
		},
	}, nil
}

// sysCapabilityMaxInflight keeps the worker's declared concurrency. worker-sys
// pins the capability set, but the per-capability limit is the worker's own
// configuration and must not be overridden here.
func sysCapabilityMaxInflight(declared int) int {
	if declared <= 0 {
		return 1
	}
	return declared
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return map[string]string{}
	}
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func (s *RegistryService) handleHeartbeat(ctx context.Context, session *activeSession, heartbeat *registryv1.HeartbeatFrame) error {
	if heartbeat == nil {
		return status.Error(codes.InvalidArgument, "heartbeat frame is required")
	}
	if strings.TrimSpace(heartbeat.GetSessionId()) == "" {
		return status.Error(codes.InvalidArgument, "session_id is required")
	}
	if heartbeat.GetNodeId() != session.nodeID {
		return status.Error(codes.InvalidArgument, "node_id mismatch")
	}
	if heartbeat.GetActiveSessionCount() < 0 {
		return status.Error(codes.InvalidArgument, "active_session_count must be non-negative")
	}

	now := s.nowFn()
	if err := s.store.TouchWithSession(heartbeat.GetNodeId(), heartbeat.GetSessionId(), now); err != nil {
		if errors.Is(err, registry.ErrNodeNotFound) {
			return status.Error(codes.NotFound, "node not found")
		}
		if errors.Is(err, registry.ErrSessionMismatch) {
			return status.Error(codes.FailedPrecondition, "session is outdated")
		}
		return status.Error(codes.Internal, "failed to update heartbeat")
	}
	session.setActiveSessionCount(heartbeat.GetActiveSessionCount(), now)
	logTerminalSessionCapacityInvariant(session.nodeID, session.terminalSessionCapacitySnapshot())

	if err := session.enqueueControl(ctx, newHeartbeatAck(s.heartbeatIntervalSec)); err != nil {
		if status.Code(err) != codes.Unknown {
			return err
		}
		if mapped := status.FromContextError(err); mapped.Code() != codes.Unknown {
			return mapped.Err()
		}
		return status.Error(codes.Internal, "failed to send heartbeat ack")
	}
	return nil
}

func handleCommandResult(session *activeSession, result *registryv1.CommandResult) error {
	if result == nil {
		return status.Error(codes.InvalidArgument, "command_result frame is required")
	}
	if strings.TrimSpace(result.GetCommandId()) == "" {
		return status.Error(codes.InvalidArgument, "command_id is required")
	}

	session.resolvePending(result)
	return nil
}

func validateHello(hello *registryv1.ConnectHello) error {
	if hello == nil {
		return status.Error(codes.InvalidArgument, "hello frame is required")
	}
	if err := validateNodeID(hello.GetNodeId()); err != nil {
		return err
	}
	if capacity := hello.GetTerminalSessionCapacity(); capacity != nil {
		if capacity.GetMaxActiveSessions() < 0 {
			return status.Error(codes.InvalidArgument, "terminal_session_capacity.max_active_sessions must be non-negative")
		}
		if capacity.GetActiveSessionCount() < 0 {
			return status.Error(codes.InvalidArgument, "terminal_session_capacity.active_session_count must be non-negative")
		}
	}
	return nil
}

func logTerminalSessionCapacityInvariant(nodeID string, snapshot terminalSessionCapacitySnapshot) {
	if !snapshot.known || snapshot.maxActiveSessions <= 0 || snapshot.activeSessionCount <= snapshot.maxActiveSessions {
		return
	}
	slog.Warn(
		"worker terminal session capacity invariant violated",
		"node_id", nodeID,
		"active_session_count", snapshot.activeSessionCount,
		"max_active_sessions", snapshot.maxActiveSessions,
	)
}

func validateNodeID(nodeID string) error {
	if strings.TrimSpace(nodeID) == "" {
		return status.Error(codes.InvalidArgument, "node_id is required")
	}
	if len(nodeID) > maxNodeIDLength {
		return status.Error(codes.InvalidArgument, "node_id is too long")
	}
	return nil
}

func mapStreamError(err error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	if mapped := status.FromContextError(err); mapped.Code() != codes.Unknown {
		return mapped.Err()
	}
	return err
}

func (s *RegistryService) getSession(nodeID string) *activeSession {
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()
	return s.sessions[nodeID]
}

func (s *RegistryService) swapSession(session *activeSession) *activeSession {
	if session == nil {
		return nil
	}
	replaced := func() *activeSession {
		s.sessionsMu.Lock()
		defer s.sessionsMu.Unlock()
		replaced := s.sessions[session.nodeID]
		s.sessions[session.nodeID] = session
		return replaced
	}()
	return replaced
}

func (s *RegistryService) removeSession(session *activeSession) {
	if session == nil {
		return
	}
	shouldClearStoreSession := false

	func() {
		s.sessionsMu.Lock()
		defer s.sessionsMu.Unlock()
		current, ok := s.sessions[session.nodeID]
		if !ok {
			return
		}
		if current.sessionID != session.sessionID {
			return
		}
		delete(s.sessions, session.nodeID)
		shouldClearStoreSession = true
	}()
	if !shouldClearStoreSession {
		return
	}
	unavailable := s.markTerminalSessionRoutesUnavailable(session.nodeID)
	slog.Info(
		"terminal session routes unavailable",
		"executor_kind", session.executorKind,
		"unavailable_route_count", unavailable,
	)
	if shouldClearStoreSession && s.store != nil {
		if err := s.store.ClearSession(session.nodeID, session.sessionID); err != nil {
			slog.Error(
				"failed to clear worker session by node+session",
				"node_id",
				session.nodeID,
				"session_id",
				session.sessionID,
				"error",
				err,
			)
		}
	}
}

func writerLoop(stream grpc.BidiStreamingServer[registryv1.ConnectRequest, registryv1.ConnectResponse], session *activeSession) error {
	for {
		select {
		case <-session.done:
			return nil
		case response := <-session.controlOutbound:
			if response == nil {
				continue
			}
			if err := stream.Send(response); err != nil {
				return err
			}
		default:
		}

		select {
		case <-session.done:
			return nil
		case response := <-session.controlOutbound:
			if response == nil {
				continue
			}
			if err := stream.Send(response); err != nil {
				return err
			}
		case response := <-session.commandOutbound:
			if response == nil {
				continue
			}
			if err := stream.Send(response); err != nil {
				return err
			}
		}
	}
}

func newConnectAck(sessionID string, heartbeatIntervalSec int32, recoveryCandidates ...*registryv1.TerminalSessionRecoveryCandidate) *registryv1.ConnectResponse {
	return &registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_ConnectAck{
			ConnectAck: &registryv1.ConnectAck{
				SessionId:                         sessionID,
				HeartbeatIntervalSec:              heartbeatIntervalSec,
				TerminalSessionRecoveryCandidates: recoveryCandidates,
			},
		},
	}
}

func newTerminalSessionRecoveryAck() *registryv1.ConnectResponse {
	return &registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_TerminalSessionRecoveryAck{
			TerminalSessionRecoveryAck: &registryv1.TerminalSessionRecoveryAck{},
		},
	}
}

func newHeartbeatAck(heartbeatIntervalSec int32) *registryv1.ConnectResponse {
	return &registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_HeartbeatAck{
			HeartbeatAck: &registryv1.HeartbeatAck{
				HeartbeatIntervalSec: heartbeatIntervalSec,
			},
		},
	}
}
