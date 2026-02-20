package grpcserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"strings"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
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
	s.credentialsMu.RLock()
	hasher := s.hasher
	s.credentialsMu.RUnlock()
	if hasher != nil {
		if !hasher.Equal(secret, workerSecret) {
			return status.Error(codes.Unauthenticated, "invalid worker credential")
		}
	} else if subtle.ConstantTimeCompare([]byte(secret), []byte(workerSecret)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid worker credential")
	}

	now := s.nowFn()
	sessionID, err := s.newSessionIDFn()
	if err != nil {
		return status.Error(codes.Internal, "failed to create session_id")
	}

	session := newActiveSession(hello.GetNodeId(), sessionID, hello)
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

	if err := session.enqueueControl(stream.Context(), newConnectAck(sessionID, s.heartbeatIntervalSec)); err != nil {
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
		default:
			return status.Error(codes.InvalidArgument, "unsupported frame type")
		}
	}
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
	return nil
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
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	replaced := s.sessions[session.nodeID]
	s.sessions[session.nodeID] = session
	return replaced
}

func (s *RegistryService) removeSession(session *activeSession) {
	if session == nil {
		return
	}
	shouldClearStoreSession := false

	s.sessionsMu.Lock()
	current, ok := s.sessions[session.nodeID]
	if !ok {
		s.sessionsMu.Unlock()
		return
	}
	if current.sessionID != session.sessionID {
		s.sessionsMu.Unlock()
		return
	}
	delete(s.sessions, session.nodeID)
	shouldClearStoreSession = true
	s.sessionsMu.Unlock()
	if shouldClearStoreSession && s.store != nil {
		if err := s.store.ClearSession(session.nodeID, session.sessionID); err != nil {
			log.Printf(
				"failed to clear worker session by node+session: node_id=%s session_id=%s err=%v",
				session.nodeID,
				session.sessionID,
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

func newConnectAck(sessionID string, heartbeatIntervalSec int32) *registryv1.ConnectResponse {
	return &registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_ConnectAck{
			ConnectAck: &registryv1.ConnectAck{
				SessionId:            sessionID,
				HeartbeatIntervalSec: heartbeatIntervalSec,
			},
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
