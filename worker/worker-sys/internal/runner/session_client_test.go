package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-sys/internal/config"
)

func TestCommandExecSlotAcquireRelease(t *testing.T) {
	commandExecSlots := newCommandSlots(config.Config{})

	if !commandExecSlots.tryAcquire(computerUseCapabilityName) {
		t.Fatalf("expected first slot acquire to succeed")
	}
	if commandExecSlots.tryAcquire(computerUseCapabilityName) {
		t.Fatalf("expected second slot acquire to fail while slot is held")
	}

	commandExecSlots.release(computerUseCapabilityName)
	if !commandExecSlots.tryAcquire(computerUseCapabilityName) {
		t.Fatalf("expected slot acquire to succeed after release")
	}
}

func TestHandleCommandDispatchBusyReturnsSessionBusyWithoutExecution(t *testing.T) {
	commandExecSlots := newCommandSlots(config.Config{})
	if !commandExecSlots.tryAcquire(computerUseCapabilityName) {
		t.Fatalf("expected to acquire slot for busy-state setup")
	}
	defer commandExecSlots.release(computerUseCapabilityName)

	outbound := make(chan *registryv1.ConnectRequest, 1)
	errCh := make(chan error, 1)
	executed := make(chan struct{}, 1)

	ok := handleCommandDispatch(
		context.Background(),
		outbound,
		errCh,
		commandExecSlots,
		&registryv1.CommandDispatch{
			CommandId:   "cmd-busy",
			Capability:  computerUseCapabilityDeclared,
			PayloadJson: []byte(`{"command":"pwd"}`),
		},
		func(context.Context, *registryv1.CommandDispatch) *registryv1.ConnectRequest {
			executed <- struct{}{}
			return commandErrorResult("cmd-busy", "execution_failed", "unexpected execution")
		},
	)
	if !ok {
		t.Fatalf("expected dispatch handling to continue")
	}

	select {
	case <-executed:
		t.Fatalf("execute function should not run while slot is busy")
	default:
	}

	select {
	case err := <-errCh:
		t.Fatalf("expected no session error, got %v", err)
	default:
	}

	select {
	case req := <-outbound:
		result := req.GetCommandResult()
		if result == nil {
			t.Fatalf("expected command_result payload")
		}
		if result.GetCommandId() != "cmd-busy" {
			t.Fatalf("expected command_id cmd-busy, got %q", result.GetCommandId())
		}
		if result.GetError() == nil {
			t.Fatalf("expected session_busy command error")
		}
		if result.GetError().GetCode() != sessionBusyErrorCode {
			t.Fatalf("expected error code %q, got %q", sessionBusyErrorCode, result.GetError().GetCode())
		}
		if result.GetError().GetMessage() != sessionBusyErrorMessage {
			t.Fatalf("expected error message %q, got %q", sessionBusyErrorMessage, result.GetError().GetMessage())
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for session_busy result")
	}
}

func TestHandleCommandDispatchRunsExecutionAndReleasesSlot(t *testing.T) {
	commandExecSlots := newCommandSlots(config.Config{})

	outbound := make(chan *registryv1.ConnectRequest, 1)
	errCh := make(chan error, 1)
	executed := make(chan struct{}, 1)

	ok := handleCommandDispatch(
		context.Background(),
		outbound,
		errCh,
		commandExecSlots,
		&registryv1.CommandDispatch{
			CommandId:   "cmd-run",
			Capability:  computerUseCapabilityDeclared,
			PayloadJson: []byte(`{"command":"pwd"}`),
		},
		func(context.Context, *registryv1.CommandDispatch) *registryv1.ConnectRequest {
			executed <- struct{}{}
			return commandErrorResult("cmd-run", "execution_failed", "forced test failure")
		},
	)
	if !ok {
		t.Fatalf("expected dispatch handling to continue")
	}

	select {
	case <-executed:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for execute function")
	}

	select {
	case req := <-outbound:
		result := req.GetCommandResult()
		if result == nil {
			t.Fatalf("expected command_result payload")
		}
		if result.GetCommandId() != "cmd-run" {
			t.Fatalf("expected command_id cmd-run, got %q", result.GetCommandId())
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for command result enqueue")
	}

	select {
	case err := <-errCh:
		t.Fatalf("expected no session error, got %v", err)
	default:
	}

	if !commandExecSlots.tryAcquire(computerUseCapabilityName) {
		t.Fatalf("expected slot to be released after execution")
	}
}

func TestCommandDispatchTextForLogPayloads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		capability string
		payload    []byte
		want       string
	}{
		{
			name:       "computer_use_command",
			capability: computerUseCapabilityName,
			payload:    []byte(`{"command":"pwd"}`),
			want:       "pwd",
		},
		{
			name:       "computer_use_multiline_command",
			capability: computerUseCapabilityName,
			payload:    []byte("{\"command\":\"echo first\\necho second\"}"),
			want:       "echo first\necho second",
		},
		{
			name:       "computer_use_invalid_payload_falls_back_to_raw",
			capability: computerUseCapabilityName,
			payload:    []byte("not-json"),
			want:       "not-json",
		},
		{
			name:       "computer_use_empty_command_falls_back_to_raw",
			capability: computerUseCapabilityName,
			payload:    []byte(`{"command":"   "}`),
			want:       `{"command":"   "}`,
		},
		{
			name:       "read_image_payload_logs_action_and_path",
			capability: readImageCapabilityName,
			payload:    []byte(`{"session_id":"computerUse","file_path":"/tmp/a.png","action":"read"}`),
			want:       "read /tmp/a.png",
		},
		{
			name:       "read_image_invalid_action_logs_path",
			capability: readImageCapabilityName,
			payload:    []byte(`{"session_id":"computerUse","file_path":"/tmp/a.png","action":"invalid"}`),
			want:       "/tmp/a.png",
		},
		{
			name:       "read_image_empty_path_falls_back_to_raw",
			capability: readImageCapabilityName,
			payload:    []byte(`{"session_id":"computerUse","file_path":"   ","action":"read"}`),
			want:       `{"session_id":"computerUse","file_path":"   ","action":"read"}`,
		},
		{
			name:       "unsupported_capability_uses_raw_payload",
			capability: "unknown",
			payload:    []byte(`{"command":"pwd"}`),
			want:       `{"command":"pwd"}`,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := commandDispatchTextForLog(tc.capability, tc.payload)
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestHeartbeatLoopToleratesSingleAckTimeout(t *testing.T) {
	originalApplyJitter := applyJitter
	applyJitter = func(base time.Duration, _ int) time.Duration {
		return base
	}
	defer func() {
		applyJitter = originalApplyJitter
	}()

	cfg := config.Config{
		WorkerID:        "worker-1",
		CallTimeout:     20 * time.Millisecond,
		HeartbeatJitter: 0,
	}

	outbound := make(chan *registryv1.ConnectRequest, 4)
	heartbeatAckCh := make(chan *registryv1.HeartbeatAck, 1)
	sessionErrCh := make(chan error, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- heartbeatLoop(ctx, outbound, heartbeatAckCh, sessionErrCh, cfg, "session-1", 10*time.Millisecond)
	}()

	receivedHeartbeats := 0
	timeout := time.After(2 * time.Second)
	for receivedHeartbeats < 2 {
		select {
		case <-timeout:
			t.Fatal("timed out waiting for second heartbeat")
		case req := <-outbound:
			if req.GetHeartbeat() == nil {
				t.Fatalf("expected heartbeat frame, got %#v", req.GetPayload())
			}
			if req.GetHeartbeat().GetActiveSessionCount() != 1 {
				t.Fatalf("expected active_session_count=1, got %d", req.GetHeartbeat().GetActiveSessionCount())
			}
			receivedHeartbeats++
			if receivedHeartbeats == 2 {
				heartbeatAckCh <- &registryv1.HeartbeatAck{HeartbeatIntervalSec: 1}
				cancel()
			}
		}
	}

	err := <-doneCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled after single ack timeout recovery, got %v", err)
	}
}

func TestHeartbeatLoopFailsAfterTwoConsecutiveAckTimeouts(t *testing.T) {
	originalApplyJitter := applyJitter
	applyJitter = func(base time.Duration, _ int) time.Duration {
		return base
	}
	defer func() {
		applyJitter = originalApplyJitter
	}()

	cfg := config.Config{
		WorkerID:        "worker-1",
		CallTimeout:     20 * time.Millisecond,
		HeartbeatJitter: 0,
	}

	outbound := make(chan *registryv1.ConnectRequest, 8)
	heartbeatAckCh := make(chan *registryv1.HeartbeatAck, 1)
	sessionErrCh := make(chan error, 1)

	err := heartbeatLoop(context.Background(), outbound, heartbeatAckCh, sessionErrCh, cfg, "session-1", 10*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded after two consecutive ack timeouts, got %v", err)
	}

	receivedHeartbeats := 0
	for {
		select {
		case req := <-outbound:
			if req.GetHeartbeat() != nil {
				receivedHeartbeats++
			}
		default:
			if receivedHeartbeats < 2 {
				t.Fatalf("expected at least two heartbeat frames before failure, got %d", receivedHeartbeats)
			}
			return
		}
	}
}
