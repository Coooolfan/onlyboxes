package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/api/pkg/registryauth"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	minHeartbeatInterval  = 1 * time.Second
	initialReconnectDelay = 1 * time.Second
	maxReconnectDelay     = 15 * time.Second
	echoCapabilityName    = "echo"
	defaultMaxInflight    = 4
)

var waitReconnect = waitReconnectDelay
var applyJitter = jitterDuration

func Run(ctx context.Context, cfg config.Config) error {
	if strings.TrimSpace(cfg.WorkerID) == "" {
		return errors.New("WORKER_ID is required")
	}
	if strings.TrimSpace(cfg.WorkerSecret) == "" {
		return errors.New("WORKER_SECRET is required")
	}

	reconnectDelay := initialReconnectDelay
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := runSession(ctx, cfg)
		if err == nil {
			return nil
		}

		if errCtx := ctx.Err(); errCtx != nil {
			return errCtx
		}

		if status.Code(err) == codes.FailedPrecondition {
			log.Printf("registry session replaced for node_id=%s, reconnecting", cfg.WorkerID)
			reconnectDelay = initialReconnectDelay
		} else {
			log.Printf("registry session interrupted: %v", err)
		}

		if err := waitReconnect(ctx, reconnectDelay); err != nil {
			return err
		}
		reconnectDelay = nextReconnectDelay(reconnectDelay)
	}
}

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
		if streamErr := resp.GetError(); streamErr != nil {
			return fmt.Errorf("connect rejected: code=%s message=%s", streamErr.GetCode(), streamErr.GetMessage())
		}
		return fmt.Errorf("unexpected first response frame")
	}
	sessionID := strings.TrimSpace(ack.GetSessionId())
	if sessionID == "" {
		return fmt.Errorf("connect_ack.session_id is required")
	}

	heartbeatInterval := durationFromServer(ack.GetHeartbeatIntervalSec(), cfg.HeartbeatInterval)
	log.Printf("worker connected: node_id=%s node_name=%s session_id=%s", hello.GetNodeId(), hello.GetNodeName(), sessionID)

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	outbound := make(chan *registryv1.ConnectRequest, 64)
	heartbeatAckCh := make(chan *registryv1.HeartbeatAck, 16)
	sessionErrCh := make(chan error, 4)

	go senderLoop(sessionCtx, stream, outbound, sessionErrCh)
	go receiverLoop(sessionCtx, stream, outbound, heartbeatAckCh, sessionErrCh, cfg.WorkerID)

	return heartbeatLoop(sessionCtx, outbound, heartbeatAckCh, sessionErrCh, cfg, sessionID, heartbeatInterval)
}

func dial(ctx context.Context, cfg config.Config) (*grpc.ClientConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return grpc.NewClient(
		cfg.ConsoleGRPCTarget,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
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
	nodeID string,
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
			if capability == echoCapabilityName && dispatch.GetEcho() != nil {
				log.Printf(
					"command dispatch received: node_id=%s command_id=%s capability=%s message_len=%d",
					nodeID,
					commandID,
					capability,
					len(dispatch.GetEcho().GetMessage()),
				)
			} else {
				log.Printf(
					"command dispatch received: node_id=%s command_id=%s capability=%s",
					nodeID,
					commandID,
					capability,
				)
			}

			resultReq := buildCommandResult(dispatch)
			if sendErr := enqueueRequest(ctx, outbound, resultReq); sendErr != nil {
				reportSessionErr(errCh, fmt.Errorf("enqueue command result: %w", sendErr))
				return
			}
		case resp.GetError() != nil:
			streamErr := resp.GetError()
			reportSessionErr(errCh, fmt.Errorf("stream error frame: code=%s message=%s", streamErr.GetCode(), streamErr.GetMessage()))
			return
		default:
			reportSessionErr(errCh, errors.New("unexpected response frame"))
			return
		}
	}
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
					NodeId:       cfg.WorkerID,
					SessionId:    sessionID,
					SentAtUnixMs: time.Now().UnixMilli(),
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
				return context.DeadlineExceeded
			case heartbeatAck := <-heartbeatAckCh:
				ackTimer.Stop()
				interval = durationFromServer(heartbeatAck.GetHeartbeatIntervalSec(), interval)
				waitAck = false
			}
		}
	}
}

func buildCommandResult(dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	commandID := strings.TrimSpace(dispatch.GetCommandId())
	if commandID == "" {
		return &registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_CommandResult{
				CommandResult: &registryv1.CommandResult{
					CommandId: commandID,
					Error: &registryv1.CommandError{
						Code:    "invalid_command_id",
						Message: "command_id is required",
					},
				},
			},
		}
	}

	if deadline := dispatch.GetDeadlineUnixMs(); deadline > 0 && time.Now().UnixMilli() > deadline {
		return commandErrorResult(commandID, "deadline_exceeded", "command deadline exceeded")
	}

	capability := strings.TrimSpace(strings.ToLower(dispatch.GetCapability()))
	if capability != echoCapabilityName {
		return commandErrorResult(commandID, "unsupported_capability", fmt.Sprintf("capability %q is not supported", dispatch.GetCapability()))
	}

	message := ""
	resultPayload := append([]byte(nil), dispatch.GetPayloadJson()...)
	if len(resultPayload) > 0 {
		decoded := struct {
			Message string `json:"message"`
		}{}
		if err := json.Unmarshal(resultPayload, &decoded); err != nil {
			return commandErrorResult(commandID, "invalid_payload", "payload_json is not valid echo payload")
		}
		message = decoded.Message
	}
	if strings.TrimSpace(message) == "" && dispatch.GetEcho() != nil {
		message = dispatch.GetEcho().GetMessage()
	}
	if strings.TrimSpace(message) == "" {
		return commandErrorResult(commandID, "invalid_payload", "echo payload is required")
	}
	if len(resultPayload) == 0 {
		encoded, err := json.Marshal(struct {
			Message string `json:"message"`
		}{
			Message: message,
		})
		if err != nil {
			return commandErrorResult(commandID, "encode_failed", "failed to encode echo payload")
		}
		resultPayload = encoded
	}

	return &registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_CommandResult{
			CommandResult: &registryv1.CommandResult{
				CommandId:       commandID,
				PayloadJson:     resultPayload,
				CompletedUnixMs: time.Now().UnixMilli(),
				Echo: &registryv1.EchoResult{
					Message: message,
				},
			},
		},
	}
}

func commandErrorResult(commandID string, code string, message string) *registryv1.ConnectRequest {
	return &registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_CommandResult{
			CommandResult: &registryv1.CommandResult{
				CommandId:       commandID,
				CompletedUnixMs: time.Now().UnixMilli(),
				Error: &registryv1.CommandError{
					Code:    code,
					Message: message,
				},
			},
		},
	}
}

func buildHello(cfg config.Config) (*registryv1.ConnectHello, error) {
	nonce, err := randomHex(16)
	if err != nil {
		return nil, err
	}

	nodeName := strings.TrimSpace(cfg.NodeName)
	if nodeName == "" {
		suffix := cfg.WorkerID
		if len(suffix) > 8 {
			suffix = suffix[:8]
		}
		nodeName = fmt.Sprintf("worker-docker-%s", suffix)
	}

	hello := &registryv1.ConnectHello{
		NodeId:          cfg.WorkerID,
		NodeName:        nodeName,
		ExecutorKind:    cfg.ExecutorKind,
		Labels:          cfg.Labels,
		Version:         cfg.Version,
		TimestampUnixMs: time.Now().UnixMilli(),
		Nonce:           nonce,
		Capabilities: []*registryv1.CapabilityDeclaration{
			{
				Name:        echoCapabilityName,
				MaxInflight: defaultMaxInflight,
			},
		},
	}
	hello.Signature = registryauth.Sign(hello.GetNodeId(), hello.GetTimestampUnixMs(), hello.GetNonce(), cfg.WorkerSecret)
	return hello, nil
}

func enqueueRequest(ctx context.Context, outbound chan<- *registryv1.ConnectRequest, req *registryv1.ConnectRequest) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case outbound <- req:
		return nil
	}
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

func randomHex(byteLength int) (string, error) {
	if byteLength <= 0 {
		return "", errors.New("byte length must be positive")
	}

	raw := make([]byte, byteLength)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func jitterDuration(base time.Duration, jitterPct int) time.Duration {
	if base <= 0 {
		base = minHeartbeatInterval
	}
	if jitterPct <= 0 {
		return base
	}

	maxDelta := int64(base) * int64(jitterPct) / 100
	if maxDelta <= 0 {
		return base
	}

	random, err := rand.Int(rand.Reader, big.NewInt(maxDelta*2+1))
	if err != nil {
		return base
	}
	delta := random.Int64() - maxDelta
	jittered := base + time.Duration(delta)
	if jittered < minHeartbeatInterval {
		return minHeartbeatInterval
	}
	return jittered
}

func waitReconnectDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		delay = initialReconnectDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func nextReconnectDelay(current time.Duration) time.Duration {
	if current <= 0 {
		return initialReconnectDelay
	}
	next := current * 2
	if next > maxReconnectDelay {
		return maxReconnectDelay
	}
	return next
}
