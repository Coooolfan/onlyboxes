package sessionclient

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"math/big"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

func Dial(ctx context.Context, target string, useTLS bool) (*grpc.ClientConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	credentialsOption := grpc.WithTransportCredentials(insecure.NewCredentials())
	if useTLS {
		credentialsOption = grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{}))
	}
	return grpc.NewClient(target, credentialsOption)
}

func SenderLoop(
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
				ReportError(errCh, fmt.Errorf("stream send failed: %w", err))
				return
			}
		}
	}
}

type HeartbeatConfig struct {
	WorkerID      string
	SessionID     string
	Interval      time.Duration
	JitterPercent int
	CallTimeout   time.Duration
	ActiveCount   func() int32
	ApplyJitter   func(time.Duration, int) time.Duration
}

func HeartbeatLoop(
	ctx context.Context,
	outbound chan<- *registryv1.ConnectRequest,
	heartbeatAckCh <-chan *registryv1.HeartbeatAck,
	sessionErrCh <-chan error,
	config HeartbeatConfig,
) error {
	interval := config.Interval
	consecutiveAckTimeouts := 0
	for {
		waitFor := config.ApplyJitter(interval, config.JitterPercent)
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

		activeCount := config.ActiveCount()
		if err := Enqueue(ctx, outbound, &registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_Heartbeat{Heartbeat: &registryv1.HeartbeatFrame{
				NodeId: config.WorkerID, SessionId: config.SessionID, ActiveSessionCount: activeCount,
			}},
		}); err != nil {
			return fmt.Errorf("enqueue heartbeat: %w", err)
		}

		ackTimer := time.NewTimer(config.CallTimeout)
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
				interval = DurationFromServer(heartbeatAck.GetHeartbeatIntervalSec(), interval)
				waitAck = false
			}
		}
	}
}

func Enqueue(ctx context.Context, outbound chan<- *registryv1.ConnectRequest, req *registryv1.ConnectRequest) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case outbound <- req:
		return nil
	}
}

func ReportError(errCh chan<- error, err error) {
	if err == nil {
		return
	}
	select {
	case errCh <- err:
	default:
	}
}

func RecvWithTimeout(
	ctx context.Context,
	timeout time.Duration,
	recv func() (*registryv1.ConnectResponse, error),
) (*registryv1.ConnectResponse, error) {
	if timeout <= 0 {
		return recv()
	}
	type result struct {
		response *registryv1.ConnectResponse
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		response, err := recv()
		resultCh <- result{response: response, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, context.DeadlineExceeded
	case result := <-resultCh:
		return result.response, result.err
	}
}

func DurationFromServer(seconds int32, fallback time.Duration) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}

func JitterDuration(base time.Duration, jitterPercent int, minimum time.Duration) time.Duration {
	if base <= 0 {
		base = minimum
	}
	if jitterPercent <= 0 {
		return base
	}
	maxDelta := int64(base) * int64(jitterPercent) / 100
	if maxDelta <= 0 {
		return base
	}
	random, err := rand.Int(rand.Reader, big.NewInt(maxDelta*2+1))
	if err != nil {
		return base
	}
	jittered := base + time.Duration(random.Int64()-maxDelta)
	if jittered < minimum {
		return minimum
	}
	return jittered
}

func WaitReconnectDelay(ctx context.Context, delay, initial time.Duration) error {
	if delay <= 0 {
		delay = initial
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

func NextReconnectDelay(current, initial, maximum time.Duration) time.Duration {
	if current <= 0 {
		return initial
	}
	return min(current*2, maximum)
}
