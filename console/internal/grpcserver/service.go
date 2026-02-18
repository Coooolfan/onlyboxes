package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/api/pkg/registryauth"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maxNodeIDLength               = 128
	echoCapabilityName            = "echo"
	defaultEchoTimeout            = 5 * time.Second
	defaultCloseMessage           = "session closed"
	defaultCapabilityMaxInflight  = 4
	maxProvisioningCreateAttempts = 8
	heartbeatAckEnqueueTimeout    = 500 * time.Millisecond
	controlOutboundBufferSize     = 32
	commandOutboundBufferSize     = 128
	defaultTaskRetentionWindow    = 10 * time.Minute
	defaultCommandDispatchTimeout = 60 * time.Second
)

var ErrNoEchoWorker = errors.New("no online worker supports echo")
var ErrEchoTimeout = errors.New("echo command timed out")
var ErrNoCapabilityWorker = errors.New("no online worker supports capability")
var ErrNoWorkerCapacity = errors.New("no online worker capacity for capability")
var ErrTaskRequestInProgress = errors.New("task request already in progress")

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

type RegistryService struct {
	registryv1.UnimplementedWorkerRegistryServiceServer

	store                *registry.Store
	credentialsMu        sync.RWMutex
	credentials          map[string]string
	heartbeatIntervalSec int32
	offlineTTLSec        int32
	replayWindow         time.Duration
	nonceCache           *nonceCache
	nowFn                func() time.Time
	newSessionIDFn       func() (string, error)
	newCommandIDFn       func() (string, error)
	newTaskIDFn          func() (string, error)
	taskRetention        time.Duration

	sessionsMu sync.RWMutex
	sessions   map[string]*activeSession
	roundRobin uint64

	tasksMu sync.RWMutex
	// Task lifecycle index:
	// - tasks stores live/recent task records by task_id.
	// - taskByRequestKey maps request_id to task_id after record creation.
	// - taskRequestReservations tracks request_id currently being validated/created.
	tasks                   map[string]*taskRecord
	taskByRequestKey        map[string]string
	taskRequestReservations map[string]struct{}
}

func NewRegistryService(
	store *registry.Store,
	initialCredentials map[string]string,
	heartbeatIntervalSec int32,
	offlineTTLSec int32,
	replayWindow time.Duration,
) *RegistryService {
	credentialCopy := make(map[string]string, len(initialCredentials))
	for workerID, secret := range initialCredentials {
		credentialCopy[workerID] = secret
	}
	return &RegistryService{
		store:                   store,
		credentials:             credentialCopy,
		heartbeatIntervalSec:    heartbeatIntervalSec,
		offlineTTLSec:           offlineTTLSec,
		replayWindow:            replayWindow,
		nonceCache:              newNonceCache(),
		nowFn:                   time.Now,
		newSessionIDFn:          generateUUIDv4,
		newCommandIDFn:          generateUUIDv4,
		newTaskIDFn:             generateUUIDv4,
		taskRetention:           defaultTaskRetentionWindow,
		sessions:                make(map[string]*activeSession),
		tasks:                   make(map[string]*taskRecord),
		taskByRequestKey:        make(map[string]string),
		taskRequestReservations: make(map[string]struct{}),
	}
}

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

	now := s.nowFn()
	if !s.withinReplayWindow(now, hello.GetTimestampUnixMs()) {
		return status.Error(codes.Unauthenticated, "timestamp is outside replay window")
	}

	secret, ok := s.getCredential(hello.GetNodeId())
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

	session := newActiveSession(hello.GetNodeId(), sessionID, hello)
	replaced := s.swapSession(session)
	if replaced != nil {
		replaced.close(status.Error(codes.FailedPrecondition, "session replaced by a newer connection"))
	}
	defer func() {
		s.removeSession(session)
		session.close(retErr)
	}()

	s.store.Upsert(hello, sessionID, now)

	writerErrCh := make(chan error, 1)
	go func() {
		writerErrCh <- writerLoop(stream, session)
	}()

	if err := session.enqueueControl(stream.Context(), newConnectAck(sessionID, now, s.heartbeatIntervalSec, s.offlineTTLSec)); err != nil {
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
	if capability == echoCapabilityName {
		if message, ok := parseEchoPayload(payloadJSON); ok {
			dispatch.GetCommandDispatch().Echo = &registryv1.EchoCommand{Message: message}
		}
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
	workers := s.store.ListOnlineByCapability(capability, now, time.Duration(s.offlineTTLSec)*time.Second)
	if len(workers) == 0 {
		return nil, ErrNoCapabilityWorker
	}

	start := int(atomic.AddUint64(&s.roundRobin, 1) - 1)
	type candidate struct {
		session  *activeSession
		inflight int
	}
	minInflight := int(^uint(0) >> 1)
	preferred := make([]candidate, 0, len(workers))
	fallback := make([]candidate, 0, len(workers))
	hasSession := false

	for i := 0; i < len(workers); i++ {
		index := (start + i) % len(workers)
		session := s.getSession(workers[index].NodeID)
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

	if err := session.enqueueControl(ctx, newHeartbeatAck(now, s.heartbeatIntervalSec, s.offlineTTLSec)); err != nil {
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

func (s *RegistryService) GetWorkerSecret(nodeID string) (string, bool) {
	secret, ok := s.getCredential(nodeID)
	if !ok || strings.TrimSpace(secret) == "" {
		return "", false
	}
	return secret, true
}

func (s *RegistryService) CreateProvisionedWorker(now time.Time, offlineTTL time.Duration) (string, string, error) {
	for attempt := 0; attempt < maxProvisioningCreateAttempts; attempt++ {
		workerID, err := generateUUIDv4()
		if err != nil {
			return "", "", fmt.Errorf("generate worker_id: %w", err)
		}
		workerSecret, err := generateSecretHex(32)
		if err != nil {
			return "", "", fmt.Errorf("generate worker_secret: %w", err)
		}

		if !s.putCredentialIfAbsent(workerID, workerSecret) {
			continue
		}

		seeded := s.store.SeedProvisionedWorkers([]registry.ProvisionedWorker{
			{
				NodeID: workerID,
				Labels: map[string]string{
					"source": "console-ui",
				},
			},
		}, now, offlineTTL)
		if seeded == 1 {
			return workerID, workerSecret, nil
		}

		s.deleteCredential(workerID)
	}
	return "", "", errors.New("failed to allocate unique worker_id")
}

func (s *RegistryService) DeleteProvisionedWorker(nodeID string) bool {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" {
		return false
	}

	if !s.deleteCredential(trimmedNodeID) {
		return false
	}

	s.disconnectWorker(trimmedNodeID, "worker credential revoked")
	s.store.Delete(trimmedNodeID)
	return true
}

func (s *RegistryService) getCredential(nodeID string) (string, bool) {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" {
		return "", false
	}

	s.credentialsMu.RLock()
	defer s.credentialsMu.RUnlock()

	secret, ok := s.credentials[trimmedNodeID]
	return secret, ok
}

func (s *RegistryService) putCredentialIfAbsent(nodeID string, secret string) bool {
	trimmedNodeID := strings.TrimSpace(nodeID)
	trimmedSecret := strings.TrimSpace(secret)
	if trimmedNodeID == "" || trimmedSecret == "" {
		return false
	}

	s.credentialsMu.Lock()
	defer s.credentialsMu.Unlock()

	if _, exists := s.credentials[trimmedNodeID]; exists {
		return false
	}
	s.credentials[trimmedNodeID] = trimmedSecret
	return true
}

func (s *RegistryService) deleteCredential(nodeID string) bool {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" {
		return false
	}

	s.credentialsMu.Lock()
	defer s.credentialsMu.Unlock()

	if _, exists := s.credentials[trimmedNodeID]; !exists {
		return false
	}
	delete(s.credentials, trimmedNodeID)
	return true
}

func (s *RegistryService) disconnectWorker(nodeID string, reason string) {
	trimmedNodeID := strings.TrimSpace(nodeID)
	if trimmedNodeID == "" {
		return
	}

	s.sessionsMu.Lock()
	session := s.sessions[trimmedNodeID]
	if session != nil {
		delete(s.sessions, trimmedNodeID)
	}
	s.sessionsMu.Unlock()

	if session != nil {
		session.close(status.Error(codes.PermissionDenied, reason))
	}
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
	} else if echo := result.GetEcho(); echo != nil {
		outcome.message = echo.GetMessage()
		outcome.payloadJSON = buildEchoPayload(echo.GetMessage())
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

	if len(capabilitySet) == 0 {
		for _, language := range hello.GetLanguages() {
			if language == nil {
				continue
			}
			name := normalizeCapability(language.GetLanguage())
			if name == "" {
				continue
			}
			capabilitySet[name] = &sessionCapability{maxInflight: defaultCapabilityMaxInflight}
		}
	}
	return capabilitySet
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
