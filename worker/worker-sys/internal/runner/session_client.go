package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/internal/logging"
	"github.com/onlyboxes/onlyboxes/worker/internal/sessionclient"
	"github.com/onlyboxes/onlyboxes/worker/worker-sys/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const (
	sessionBusyErrorCode    = "session_busy"
	sessionBusyErrorMessage = "session busy"
)

// commandSlots bounds concurrent command execution per capability. The slots are
// kept separate so a readImage call cannot block a computerUse call.
type commandSlots struct {
	slots map[string]chan struct{}
}

func newCommandSlots(cfg config.Config) *commandSlots {
	return &commandSlots{
		slots: map[string]chan struct{}{
			computerUseCapabilityName: newSlotChannel(cfg.ComputerUseMaxInflight),
			readImageCapabilityName:   newSlotChannel(cfg.ReadImageMaxInflight),
		},
	}
}

func newSlotChannel(capacity int) chan struct{} {
	if capacity <= 0 {
		capacity = 1
	}
	slots := make(chan struct{}, capacity)
	for i := 0; i < capacity; i++ {
		slots <- struct{}{}
	}
	return slots
}

// tryAcquire reserves a slot for the capability. Unknown capabilities are let
// through so the executor can report unsupported_capability rather than
// session_busy.
func (s *commandSlots) tryAcquire(capability string) bool {
	if s == nil {
		return false
	}
	slots, ok := s.slots[capability]
	if !ok {
		return true
	}
	select {
	case <-slots:
		return true
	default:
		return false
	}
}

func (s *commandSlots) release(capability string) {
	if s == nil {
		return
	}
	slots, ok := s.slots[capability]
	if !ok {
		return
	}
	select {
	case slots <- struct{}{}:
	default:
	}
}

func runSession(ctx context.Context, cfg config.Config) error {
	conn, err := sessionclient.Dial(ctx, cfg.ConsoleGRPCTarget, cfg.ConsoleTLS)
	if err != nil {
		return fmt.Errorf("dial console: %w", err)
	}
	defer conn.Close()

	client := registryv1.NewWorkerRegistryServiceClient(conn)
	stream, err := client.Connect(ctx)
	if err != nil {
		return fmt.Errorf("open connect stream: %w", err)
	}
	defer stream.CloseSend()

	hello, err := buildHello(cfg)
	if err != nil {
		return fmt.Errorf("build hello: %w", err)
	}

	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Hello{Hello: hello},
	}); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	resp, err := sessionclient.RecvWithTimeout(ctx, cfg.CallTimeout, stream.Recv)
	if err != nil {
		return fmt.Errorf("recv connect_ack: %w", err)
	}
	ack := resp.GetConnectAck()
	if ack == nil {
		return fmt.Errorf("unexpected first response frame")
	}
	sessionID := strings.TrimSpace(ack.GetSessionId())
	if sessionID == "" {
		return fmt.Errorf("connect_ack.session_id is required")
	}

	heartbeatInterval := sessionclient.DurationFromServer(ack.GetHeartbeatIntervalSec(), cfg.HeartbeatInterval)
	logging.Infof("worker connected: node_id=%s node_name=%s session_id=%s", hello.GetNodeId(), hello.GetNodeName(), sessionID)

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	outbound := make(chan *registryv1.ConnectRequest, 64)
	heartbeatAckCh := make(chan *registryv1.HeartbeatAck, 16)
	sessionErrCh := make(chan error, 4)
	commandExecSlots := newCommandSlots(cfg)

	go sessionclient.SenderLoop(sessionCtx, stream, outbound, sessionErrCh)
	go receiverLoop(sessionCtx, stream, outbound, heartbeatAckCh, sessionErrCh, commandExecSlots)

	return heartbeatLoop(sessionCtx, outbound, heartbeatAckCh, sessionErrCh, cfg, sessionID, heartbeatInterval)
}

func receiverLoop(
	ctx context.Context,
	stream grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse],
	outbound chan<- *registryv1.ConnectRequest,
	heartbeatAckCh chan<- *registryv1.HeartbeatAck,
	errCh chan<- error,
	commandExecSlots *commandSlots,
) {
	for {
		resp, err := stream.Recv()
		if err != nil {
			sessionclient.ReportError(errCh, fmt.Errorf("stream receive failed: %w", err))
			return
		}

		switch {
		case resp.GetHeartbeatAck() != nil:
			select {
			case <-ctx.Done():
				return
			case heartbeatAckCh <- resp.GetHeartbeatAck():
			}
		case resp.GetCommandDispatch() != nil:
			dispatch := resp.GetCommandDispatch()
			capability := strings.TrimSpace(strings.ToLower(dispatch.GetCapability()))
			commandID := strings.TrimSpace(dispatch.GetCommandId())
			commandText := commandDispatchTextForLog(capability, dispatch.GetPayloadJson())
			logging.Infof(
				"command dispatch received: command_id=%s capability=%s command=%s",
				commandID,
				capability,
				commandText,
			)

			dispatchCopy, ok := proto.Clone(dispatch).(*registryv1.CommandDispatch)
			if !ok || dispatchCopy == nil {
				sessionclient.ReportError(errCh, errors.New("clone command dispatch failed"))
				return
			}

			if !handleCommandDispatch(ctx, outbound, errCh, commandExecSlots, dispatchCopy, buildCommandResultWithContext) {
				return
			}
		default:
			sessionclient.ReportError(errCh, errors.New("unexpected response frame"))
			return
		}
	}
}

func commandDispatchTextForLog(capability string, payload []byte) string {
	rawPayload := strings.TrimSpace(string(payload))
	switch capability {
	case computerUseCapabilityName:
		decoded := computerUsePayload{}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return rawPayload
		}
		command := strings.TrimSpace(decoded.Command)
		if command == "" {
			return rawPayload
		}
		return command
	case readImageCapabilityName:
		decoded := readImagePayload{}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return rawPayload
		}
		filePath := strings.TrimSpace(decoded.FilePath)
		if filePath == "" {
			return rawPayload
		}
		action := normalizeReadImageAction(decoded.Action)
		if action == "" {
			return filePath
		}
		return action + " " + filePath
	default:
		return rawPayload
	}
}

func handleCommandDispatch(
	ctx context.Context,
	outbound chan<- *registryv1.ConnectRequest,
	errCh chan<- error,
	commandExecSlots *commandSlots,
	dispatch *registryv1.CommandDispatch,
	executeFn func(context.Context, *registryv1.CommandDispatch) *registryv1.ConnectRequest,
) bool {
	if dispatch == nil {
		sessionclient.ReportError(errCh, errors.New("command dispatch is required"))
		return false
	}
	if executeFn == nil {
		executeFn = buildCommandResultWithContext
	}

	capability := strings.TrimSpace(strings.ToLower(dispatch.GetCapability()))
	if !commandExecSlots.tryAcquire(capability) {
		busyResultReq := buildSessionBusyCommandResult(dispatch)
		if tryEnqueueRequest(ctx, outbound, busyResultReq) {
			return true
		}
		if err := ctx.Err(); err != nil {
			return false
		}
		sessionclient.ReportError(errCh, errors.New("enqueue session_busy result: outbound queue is full"))
		return false
	}

	go func(dispatch *registryv1.CommandDispatch) {
		defer commandExecSlots.release(capability)
		resultReq := executeFn(ctx, dispatch)
		if sendErr := sessionclient.Enqueue(ctx, outbound, resultReq); sendErr != nil {
			if errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded) {
				return
			}
			sessionclient.ReportError(errCh, fmt.Errorf("enqueue command result: %w", sendErr))
		}
	}(dispatch)
	return true
}

func heartbeatLoop(
	ctx context.Context,
	outbound chan<- *registryv1.ConnectRequest,
	heartbeatAckCh <-chan *registryv1.HeartbeatAck,
	sessionErrCh <-chan error,
	cfg config.Config,
	sessionID string,
	heartbeatInterval time.Duration,
) error {
	return sessionclient.HeartbeatLoop(ctx, outbound, heartbeatAckCh, sessionErrCh, sessionclient.HeartbeatConfig{
		WorkerID: cfg.WorkerID, SessionID: sessionID, Interval: heartbeatInterval,
		JitterPercent: cfg.HeartbeatJitter, CallTimeout: cfg.CallTimeout,
		ActiveCount: func() int32 { return 1 }, ApplyJitter: applyJitter,
	})
}

func tryEnqueueRequest(ctx context.Context, outbound chan<- *registryv1.ConnectRequest, req *registryv1.ConnectRequest) bool {
	select {
	case <-ctx.Done():
		return false
	case outbound <- req:
		return true
	default:
		return false
	}
}

func buildSessionBusyCommandResult(dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	commandID := ""
	if dispatch != nil {
		commandID = strings.TrimSpace(dispatch.GetCommandId())
	}
	return commandErrorResult(commandID, sessionBusyErrorCode, sessionBusyErrorMessage)
}

func jitterDuration(base time.Duration, jitterPct int) time.Duration {
	return sessionclient.JitterDuration(base, jitterPct, minHeartbeatInterval)
}
