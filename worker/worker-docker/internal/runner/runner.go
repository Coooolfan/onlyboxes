package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	heartbeatLogEvery     = 12
	minHeartbeatInterval  = 1 * time.Second
	initialReconnectDelay = 1 * time.Second
	maxReconnectDelay     = 15 * time.Second
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

	return heartbeatLoop(ctx, stream, cfg, sessionID, heartbeatInterval)
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

func heartbeatLoop(
	ctx context.Context,
	stream grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse],
	cfg config.Config,
	sessionID string,
	heartbeatInterval time.Duration,
) error {
	successCount := 0
	interval := heartbeatInterval

	for {
		waitFor := applyJitter(interval, cfg.HeartbeatJitter)
		timer := time.NewTimer(waitFor)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}

		if err := stream.Send(&registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_Heartbeat{
				Heartbeat: &registryv1.HeartbeatFrame{
					NodeId:       cfg.WorkerID,
					SessionId:    sessionID,
					SentAtUnixMs: time.Now().UnixMilli(),
				},
			},
		}); err != nil {
			return fmt.Errorf("send heartbeat: %w", err)
		}

		resp, err := recvWithTimeout(ctx, cfg.CallTimeout, stream.Recv)
		if err != nil {
			return fmt.Errorf("recv heartbeat_ack: %w", err)
		}

		if heartbeatAck := resp.GetHeartbeatAck(); heartbeatAck != nil {
			interval = durationFromServer(heartbeatAck.GetHeartbeatIntervalSec(), interval)
			successCount++
			if successCount == 1 || successCount%heartbeatLogEvery == 0 {
				log.Printf("heartbeat ok: node_id=%s session_id=%s count=%d", cfg.WorkerID, sessionID, successCount)
			}
			continue
		}

		if streamErr := resp.GetError(); streamErr != nil {
			return fmt.Errorf("stream error: code=%s message=%s", streamErr.GetCode(), streamErr.GetMessage())
		}

		return fmt.Errorf("unexpected heartbeat response frame")
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
		Languages:       cfg.Languages,
		Labels:          cfg.Labels,
		Version:         cfg.Version,
		TimestampUnixMs: time.Now().UnixMilli(),
		Nonce:           nonce,
	}
	hello.Signature = registryauth.Sign(hello.GetNodeId(), hello.GetTimestampUnixMs(), hello.GetNonce(), cfg.WorkerSecret)
	return hello, nil
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
