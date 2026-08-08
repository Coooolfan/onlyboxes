package runner

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/config"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/logging"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
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
	recoveryResults, err := recoverTerminalSessionsWithTimeout(ctx, cfg.CallTimeout, ack.GetTerminalSessionRecoveryCandidates())
	if err != nil {
		return fmt.Errorf("recover terminal sessions: %w", err)
	}
	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_TerminalSessionRecoveryReport{
			TerminalSessionRecoveryReport: &registryv1.TerminalSessionRecoveryReport{Results: recoveryResults},
		},
	}); err != nil {
		return fmt.Errorf("send terminal session recovery report: %w", err)
	}
	recoveryResp, err := recvWithTimeout(ctx, cfg.CallTimeout, stream.Recv)
	if err != nil {
		return fmt.Errorf("recv terminal session recovery ack: %w", err)
	}
	if recoveryResp.GetTerminalSessionRecoveryAck() == nil {
		return fmt.Errorf("unexpected response while waiting for terminal session recovery ack")
	}
	logTerminalRecoverySummary(recoveryResults)
	logging.Infof("worker connected: node_id=%s node_name=%s session_id=%s", hello.GetNodeId(), hello.GetNodeName(), sessionID)

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	outbound := make(chan *registryv1.ConnectRequest, 64)
	heartbeatAckCh := make(chan *registryv1.HeartbeatAck, 16)
	sessionErrCh := make(chan error, 4)

	go senderLoop(sessionCtx, stream, outbound, sessionErrCh)
	go receiverLoop(sessionCtx, stream, outbound, heartbeatAckCh, sessionErrCh)

	return heartbeatLoop(sessionCtx, outbound, heartbeatAckCh, sessionErrCh, cfg, sessionID, heartbeatInterval)
}

func recoverTerminalSessionsWithTimeout(
	ctx context.Context,
	timeout time.Duration,
	candidates []*registryv1.TerminalSessionRecoveryCandidate,
) ([]*registryv1.TerminalSessionRecoveryResult, error) {
	recoveryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resultCh := make(chan []*registryv1.TerminalSessionRecoveryResult, 1)
	go func() {
		resultCh <- recoverTerminalSessionsFn(recoveryCtx, candidates)
	}()
	select {
	case <-recoveryCtx.Done():
		return nil, recoveryCtx.Err()
	case results := <-resultCh:
		return results, nil
	}
}

func logTerminalRecoverySummary(results []*registryv1.TerminalSessionRecoveryResult) {
	counts := map[registryv1.TerminalSessionRecoveryResult_Status]int{}
	for _, result := range results {
		if result != nil {
			counts[result.GetStatus()]++
		}
	}
	logging.Infof(
		"terminal session recovery completed: candidates=%d recovered=%d missing=%d invalid=%d",
		len(results),
		counts[registryv1.TerminalSessionRecoveryResult_RECOVERED],
		counts[registryv1.TerminalSessionRecoveryResult_MISSING],
		counts[registryv1.TerminalSessionRecoveryResult_INVALID],
	)
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
			summary := commandDispatchSummaryForLog(capability, dispatch.GetPayloadJson())
			logging.Infof("command dispatch received: command_id=%s capability=%s summary=%s", commandID, capability, summary)

			dispatchCopy, ok := proto.Clone(dispatch).(*registryv1.CommandDispatch)
			if !ok || dispatchCopy == nil {
				reportSessionErr(errCh, errors.New("clone command dispatch failed"))
				return
			}

			go func(dispatch *registryv1.CommandDispatch) {
				resultReq := buildCommandResultWithContext(ctx, dispatch)
				if sendErr := enqueueRequest(ctx, outbound, resultReq); sendErr != nil {
					if errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded) {
						return
					}
					reportSessionErr(errCh, fmt.Errorf("enqueue command result: %w", sendErr))
				}
			}(dispatchCopy)
		default:
			reportSessionErr(errCh, errors.New("unexpected response frame"))
			return
		}
	}
}

func commandDispatchSummaryForLog(capability string, payload []byte) string {
	parseFailed := fmt.Sprintf("payload_len=%d summary=parse_failed", len(payload))

	switch strings.TrimSpace(strings.ToLower(capability)) {
	case echoCapabilityName:
		decoded := struct {
			Message string `json:"message"`
		}{}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return parseFailed
		}
		if strings.TrimSpace(decoded.Message) == "" {
			return parseFailed
		}
		return fmt.Sprintf("message_len=%d", len(decoded.Message))
	case pythonExecCapabilityName:
		decoded := pythonExecPayload{}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return parseFailed
		}
		if strings.TrimSpace(decoded.Code) == "" {
			return parseFailed
		}
		return fmt.Sprintf("code_len=%d", len(decoded.Code))
	case terminalExecCapabilityName:
		decoded := terminalExecPayload{}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return parseFailed
		}
		if strings.TrimSpace(decoded.Command) == "" {
			return parseFailed
		}

		leaseTTLSec := "default"
		if decoded.LeaseTTLSec != nil {
			leaseTTLSec = strconv.Itoa(*decoded.LeaseTTLSec)
		}
		return fmt.Sprintf(
			"command_len=%d session_id_present=%t create_if_missing=%t lease_ttl_sec=%s",
			len(decoded.Command),
			strings.TrimSpace(decoded.SessionID) != "",
			decoded.CreateIfMissing,
			leaseTTLSec,
		)
	case terminalResourceCapabilityName:
		decoded := terminalResourcePayload{}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return parseFailed
		}
		sessionPresent := strings.TrimSpace(decoded.SessionID) != ""
		path := strings.TrimSpace(decoded.FilePath)
		if !sessionPresent || path == "" {
			return parseFailed
		}

		actionSummary := "default"
		switch strings.TrimSpace(strings.ToLower(decoded.Action)) {
		case "":
			actionSummary = "default"
		case terminalResourceActionValidate:
			actionSummary = terminalResourceActionValidate
		case terminalResourceActionRead:
			actionSummary = terminalResourceActionRead
		case terminalResourceActionExport:
			actionSummary = terminalResourceActionExport
		default:
			actionSummary = "invalid"
		}
		signedURLPresent := strings.TrimSpace(decoded.SignedURL) != ""
		return fmt.Sprintf(
			"action=%s session_id_present=%t file_path_len=%d signed_url_present=%t",
			actionSummary,
			sessionPresent,
			len(path),
			signedURLPresent,
		)
	default:
		return fmt.Sprintf("payload_len=%d summary=unsupported_capability", len(payload))
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
					ActiveSessionCount: activeSessionCountFn(),
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
