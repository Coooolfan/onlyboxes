package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/internal/logging"
	"github.com/onlyboxes/onlyboxes/worker/internal/sessionclient"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

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
	recoveryResp, err := sessionclient.RecvWithTimeout(ctx, cfg.CallTimeout, stream.Recv)
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

	go sessionclient.SenderLoop(sessionCtx, stream, outbound, sessionErrCh)
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
	recoverTerminalSessions := recoverTerminalSessionsFn
	go func() {
		resultCh <- recoverTerminalSessions(recoveryCtx, candidates)
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
			summary := commandDispatchSummaryForLog(capability, dispatch.GetPayloadJson())
			logging.Infof("command dispatch received: command_id=%s capability=%s summary=%s", commandID, capability, summary)

			dispatchCopy, ok := proto.Clone(dispatch).(*registryv1.CommandDispatch)
			if !ok || dispatchCopy == nil {
				sessionclient.ReportError(errCh, errors.New("clone command dispatch failed"))
				return
			}

			go func(dispatch *registryv1.CommandDispatch) {
				resultReq := buildCommandResultWithContext(ctx, dispatch)
				if sendErr := sessionclient.Enqueue(ctx, outbound, resultReq); sendErr != nil {
					if errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded) {
						return
					}
					sessionclient.ReportError(errCh, fmt.Errorf("enqueue command result: %w", sendErr))
				}
			}(dispatchCopy)
		default:
			sessionclient.ReportError(errCh, errors.New("unexpected response frame"))
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
	return sessionclient.HeartbeatLoop(ctx, outbound, heartbeatAckCh, sessionErrCh, sessionclient.HeartbeatConfig{
		WorkerID: cfg.WorkerID, SessionID: sessionID, Interval: heartbeatInterval,
		JitterPercent: cfg.HeartbeatJitter, CallTimeout: cfg.CallTimeout,
		ActiveCount: activeSessionCountFn, ApplyJitter: applyJitter,
	})
}

func jitterDuration(base time.Duration, jitterPct int) time.Duration {
	return sessionclient.JitterDuration(base, jitterPct, minHeartbeatInterval)
}

func waitReconnectDelay(ctx context.Context, delay time.Duration) error {
	return sessionclient.WaitReconnectDelay(ctx, delay, initialReconnectDelay)
}

func nextReconnectDelay(current time.Duration) time.Duration {
	return sessionclient.NextReconnectDelay(current, initialReconnectDelay, maxReconnectDelay)
}
