package grpcserver

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/api/pkg/registryauth"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxNodeIDLength = 128

type RegistryService struct {
	registryv1.UnimplementedWorkerRegistryServiceServer

	store                *registry.Store
	credentials          map[string]string
	heartbeatIntervalSec int32
	offlineTTLSec        int32
	replayWindow         time.Duration
	nonceCache           *nonceCache
	nowFn                func() time.Time
	newSessionIDFn       func() (string, error)
}

func NewRegistryService(
	store *registry.Store,
	credentials map[string]string,
	heartbeatIntervalSec int32,
	offlineTTLSec int32,
	replayWindow time.Duration,
) *RegistryService {
	credentialCopy := make(map[string]string, len(credentials))
	for workerID, secret := range credentials {
		credentialCopy[workerID] = secret
	}
	return &RegistryService{
		store:                store,
		credentials:          credentialCopy,
		heartbeatIntervalSec: heartbeatIntervalSec,
		offlineTTLSec:        offlineTTLSec,
		replayWindow:         replayWindow,
		nonceCache:           newNonceCache(),
		nowFn:                time.Now,
		newSessionIDFn:       generateUUIDv4,
	}
}

func (s *RegistryService) Connect(stream grpc.BidiStreamingServer[registryv1.ConnectRequest, registryv1.ConnectResponse]) error {
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

	now := s.nowFn()
	if !s.withinReplayWindow(now, hello.GetTimestampUnixMs()) {
		return status.Error(codes.Unauthenticated, "timestamp is outside replay window")
	}

	secret, ok := s.credentials[hello.GetNodeId()]
	if !ok {
		return status.Error(codes.Unauthenticated, "unknown worker_id")
	}
	if !registryauth.Verify(hello.GetNodeId(), hello.GetTimestampUnixMs(), hello.GetNonce(), secret, hello.GetSignature()) {
		return status.Error(codes.Unauthenticated, "invalid signature")
	}
	if err := s.nonceCache.Use(hello.GetNodeId(), hello.GetNonce(), now, s.replayWindow); err != nil {
		return status.Error(codes.Unauthenticated, err.Error())
	}

	sessionID, err := s.newSessionIDFn()
	if err != nil {
		return status.Error(codes.Internal, "failed to create session_id")
	}

	s.store.Upsert(hello, sessionID, now)
	if err := stream.Send(newConnectAck(sessionID, now, s.heartbeatIntervalSec, s.offlineTTLSec)); err != nil {
		return mapStreamError(err)
	}

	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return mapStreamError(err)
		}

		heartbeat := req.GetHeartbeat()
		if heartbeat == nil {
			return status.Error(codes.InvalidArgument, "heartbeat frame is required")
		}
		if strings.TrimSpace(heartbeat.GetSessionId()) == "" {
			return status.Error(codes.InvalidArgument, "session_id is required")
		}
		if heartbeat.GetNodeId() != hello.GetNodeId() {
			return status.Error(codes.InvalidArgument, "node_id mismatch")
		}

		now = s.nowFn()
		if err := s.store.TouchWithSession(heartbeat.GetNodeId(), heartbeat.GetSessionId(), now); err != nil {
			if errors.Is(err, registry.ErrNodeNotFound) {
				return status.Error(codes.NotFound, "node not found")
			}
			if errors.Is(err, registry.ErrSessionMismatch) {
				return status.Error(codes.FailedPrecondition, "session is outdated")
			}
			return status.Error(codes.Internal, "failed to update heartbeat")
		}

		if err := stream.Send(newHeartbeatAck(now, s.heartbeatIntervalSec, s.offlineTTLSec)); err != nil {
			return mapStreamError(err)
		}
	}
}

func (s *RegistryService) withinReplayWindow(now time.Time, timestampUnixMS int64) bool {
	if timestampUnixMS <= 0 {
		return false
	}
	delta := now.Sub(time.UnixMilli(timestampUnixMS))
	if delta < 0 {
		delta = -delta
	}
	return delta <= s.replayWindow
}

func validateHello(hello *registryv1.ConnectHello) error {
	if hello == nil {
		return status.Error(codes.InvalidArgument, "hello frame is required")
	}
	if err := validateNodeID(hello.GetNodeId()); err != nil {
		return err
	}
	if hello.GetTimestampUnixMs() <= 0 {
		return status.Error(codes.InvalidArgument, "timestamp_unix_ms is required")
	}
	if strings.TrimSpace(hello.GetNonce()) == "" {
		return status.Error(codes.InvalidArgument, "nonce is required")
	}
	if strings.TrimSpace(hello.GetSignature()) == "" {
		return status.Error(codes.InvalidArgument, "signature is required")
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

func newConnectAck(sessionID string, now time.Time, heartbeatIntervalSec int32, offlineTTLSec int32) *registryv1.ConnectResponse {
	return &registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_ConnectAck{
			ConnectAck: &registryv1.ConnectAck{
				SessionId:            sessionID,
				ServerTimeUnixMs:     now.UnixMilli(),
				HeartbeatIntervalSec: heartbeatIntervalSec,
				OfflineTtlSec:        offlineTTLSec,
			},
		},
	}
}

func newHeartbeatAck(now time.Time, heartbeatIntervalSec int32, offlineTTLSec int32) *registryv1.ConnectResponse {
	return &registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_HeartbeatAck{
			HeartbeatAck: &registryv1.HeartbeatAck{
				ServerTimeUnixMs:     now.UnixMilli(),
				HeartbeatIntervalSec: heartbeatIntervalSec,
				OfflineTtlSec:        offlineTTLSec,
			},
		},
	}
}

type nonceCache struct {
	mu      sync.Mutex
	entries map[string]map[string]time.Time
}

func newNonceCache() *nonceCache {
	return &nonceCache{
		entries: make(map[string]map[string]time.Time),
	}
}

func (c *nonceCache) Use(nodeID string, nonce string, now time.Time, window time.Duration) error {
	if strings.TrimSpace(nodeID) == "" {
		return errors.New("node_id is required")
	}
	if strings.TrimSpace(nonce) == "" {
		return errors.New("nonce is required")
	}
	if window <= 0 {
		return errors.New("replay window must be positive")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for id, nonces := range c.entries {
		for value, seenAt := range nonces {
			if now.Sub(seenAt) > window {
				delete(nonces, value)
			}
		}
		if len(nonces) == 0 {
			delete(c.entries, id)
		}
	}

	nodeNonces, ok := c.entries[nodeID]
	if !ok {
		nodeNonces = make(map[string]time.Time)
		c.entries[nodeID] = nodeNonces
	}
	if _, exists := nodeNonces[nonce]; exists {
		return fmt.Errorf("nonce replay detected")
	}
	nodeNonces[nonce] = now
	return nil
}
