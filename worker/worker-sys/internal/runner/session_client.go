package runner

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-sys/internal/config"
	"github.com/onlyboxes/onlyboxes/worker/worker-sys/internal/logging"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

const (
	commandExecSlotCapacity = 1
	sessionBusyErrorCode    = "session_busy"
	sessionBusyErrorMessage = "session busy"
)

func runSession(ctx context.Context, cfg config.Config) error {
	conn, err := dial(ctx, cfg)
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

	resp, err := recvWithTimeout(ctx, cfg.CallTimeout, stream.Recv)
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

	heartbeatInterval := durationFromServer(ack.GetHeartbeatIntervalSec(), cfg.HeartbeatInterval)
	logging.Infof("worker connected: node_id=%s node_name=%s session_id=%s", hello.GetNodeId(), hello.GetNodeName(), sessionID)

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	outbound := make(chan *registryv1.ConnectRequest, 64)
	heartbeatAckCh := make(chan *registryv1.HeartbeatAck, 16)
	sessionErrCh := make(chan error, 4)
	commandExecSlots := make(chan struct{}, commandExecSlotCapacity)
	commandExecSlots <- struct{}{}

	go senderLoop(sessionCtx, stream, outbound, sessionErrCh)
	go receiverLoop(sessionCtx, stream, outbound, heartbeatAckCh, sessionErrCh, commandExecSlots)

	return heartbeatLoop(sessionCtx, outbound, heartbeatAckCh, sessionErrCh, cfg, sessionID, heartbeatInterval)
}

func dial(ctx context.Context, cfg config.Config) (*grpc.ClientConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var creds grpc.DialOption
	if cfg.ConsoleTLS {
		creds = grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{}))
	} else {
		creds = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	return grpc.NewClient(cfg.ConsoleGRPCTarget, creds)
}

func senderLoop(
	ctx context.Context,
	stream grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse],
	outbound <-chan *registryv1.ConnectRequest,
	errCh chan<- error,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-outbound:
			if req == nil {
				continue
			}
			if err := stream.Send(req); err != nil {
				reportSessionErr(errCh, fmt.Errorf("stream send failed: %w", err))
				return
			}
		}
	}
}

func receiverLoop(
	ctx context.Context,
	stream grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse],
	outbound chan<- *registryv1.ConnectRequest,
	heartbeatAckCh chan<- *registryv1.HeartbeatAck,
	errCh chan<- error,
	commandExecSlots chan struct{},
) {
	for {
		resp, err := stream.Recv()
		if err != nil {
			reportSessionErr(errCh, fmt.Errorf("stream receive failed: %w", err))
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
				reportSessionErr(errCh, errors.New("clone command dispatch failed"))
				return
			}

			if !handleCommandDispatch(ctx, outbound, errCh, commandExecSlots, dispatchCopy, buildCommandResultWithContext) {
				return
			}
		default:
			reportSessionErr(errCh, errors.New("unexpected response frame"))
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
	commandExecSlots chan struct{},
	dispatch *registryv1.CommandDispatch,
	executeFn func(context.Context, *registryv1.CommandDispatch) *registryv1.ConnectRequest,
) bool {
	if dispatch == nil {
		reportSessionErr(errCh, errors.New("command dispatch is required"))
		return false
	}
	if executeFn == nil {
		executeFn = buildCommandResultWithContext
	}

	if !tryAcquireCommandSlot(commandExecSlots) {
		busyResultReq := buildSessionBusyCommandResult(dispatch)
		if tryEnqueueRequest(ctx, outbound, busyResultReq) {
			return true
		}
		if err := ctx.Err(); err != nil {
			return false
		}
		reportSessionErr(errCh, errors.New("enqueue session_busy result: outbound queue is full"))
		return false
	}

	go func(dispatch *registryv1.CommandDispatch) {
		defer releaseCommandSlot(commandExecSlots)
		resultReq := executeFn(ctx, dispatch)
		if sendErr := enqueueRequest(ctx, outbound, resultReq); sendErr != nil {
			if errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded) {
				return
			}
			reportSessionErr(errCh, fmt.Errorf("enqueue command result: %w", sendErr))
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
	interval := heartbeatInterval
	consecutiveAckTimeouts := 0

	for {
		waitFor := applyJitter(interval, cfg.HeartbeatJitter)
		timer := time.NewTimer(waitFor)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case err := <-sessionErrCh:
			timer.Stop()
			return err
		case <-timer.C:
		}

		if err := enqueueRequest(ctx, outbound, &registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_Heartbeat{
				Heartbeat: &registryv1.HeartbeatFrame{
					NodeId:             cfg.WorkerID,
					SessionId:          sessionID,
					ActiveSessionCount: 1,
				},
			},
		}); err != nil {
			return fmt.Errorf("enqueue heartbeat: %w", err)
		}

		ackTimer := time.NewTimer(cfg.CallTimeout)
		waitAck := true
		for waitAck {
			select {
			case <-ctx.Done():
				ackTimer.Stop()
				return ctx.Err()
			case err := <-sessionErrCh:
				ackTimer.Stop()
				return err
			case <-ackTimer.C:
				consecutiveAckTimeouts++
				if consecutiveAckTimeouts >= 2 {
					return context.DeadlineExceeded
				}
				waitAck = false
			case heartbeatAck := <-heartbeatAckCh:
				ackTimer.Stop()
				consecutiveAckTimeouts = 0
				interval = durationFromServer(heartbeatAck.GetHeartbeatIntervalSec(), interval)
				waitAck = false
			}
		}
	}
}

func enqueueRequest(ctx context.Context, outbound chan<- *registryv1.ConnectRequest, req *registryv1.ConnectRequest) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case outbound <- req:
		return nil
	}
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

func tryAcquireCommandSlot(commandExecSlots chan struct{}) bool {
	if commandExecSlots == nil {
		return false
	}
	select {
	case <-commandExecSlots:
		return true
	default:
		return false
	}
}

func releaseCommandSlot(commandExecSlots chan struct{}) {
	if commandExecSlots == nil {
		return
	}
	select {
	case commandExecSlots <- struct{}{}:
	default:
	}
}

func buildSessionBusyCommandResult(dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	commandID := ""
	if dispatch != nil {
		commandID = strings.TrimSpace(dispatch.GetCommandId())
	}
	return commandErrorResult(commandID, sessionBusyErrorCode, sessionBusyErrorMessage)
}

func reportSessionErr(errCh chan<- error, err error) {
	if err == nil {
		return
	}
	select {
	case errCh <- err:
	default:
	}
}

func recvWithTimeout(
	ctx context.Context,
	timeout time.Duration,
	recv func() (*registryv1.ConnectResponse, error),
) (*registryv1.ConnectResponse, error) {
	if timeout <= 0 {
		return recv()
	}

	type recvResult struct {
		resp *registryv1.ConnectResponse
		err  error
	}

	resultCh := make(chan recvResult, 1)
	go func() {
		resp, err := recv()
		resultCh <- recvResult{resp: resp, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, context.DeadlineExceeded
	case result := <-resultCh:
		return result.resp, result.err
	}
}

func durationFromServer(seconds int32, fallback time.Duration) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if fallback <= 0 {
		return 5 * time.Second
	}
	return fallback
}

func jitterDuration(base time.Duration, jitterPct int) time.Duration {
	if base <= 0 {
		return minHeartbeatInterval
	}
	if jitterPct <= 0 {
		if base < minHeartbeatInterval {
			return minHeartbeatInterval
		}
		return base
	}
	if jitterPct > 100 {
		jitterPct = 100
	}

	jitterRange := int64(base) * int64(jitterPct) / 100
	if jitterRange <= 0 {
		if base < minHeartbeatInterval {
			return minHeartbeatInterval
		}
		return base
	}

	limit := jitterRange*2 + 1
	rnd, err := rand.Int(rand.Reader, big.NewInt(limit))
	if err != nil {
		if base < minHeartbeatInterval {
			return minHeartbeatInterval
		}
		return base
	}
	delta := rnd.Int64() - jitterRange
	jittered := base + time.Duration(delta)
	if jittered < minHeartbeatInterval {
		return minHeartbeatInterval
	}
	return jittered
}
