package runner

import (
	"context"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-sys/internal/config"
)

func TestCommandSlotsDefaultToOnePerCapability(t *testing.T) {
	slots := newCommandSlots(config.Config{})

	if !slots.tryAcquire(computerUseCapabilityName) {
		t.Fatalf("expected first computerUse acquire to succeed")
	}
	if slots.tryAcquire(computerUseCapabilityName) {
		t.Fatalf("expected second computerUse acquire to fail at the default limit")
	}

	slots.release(computerUseCapabilityName)
	if !slots.tryAcquire(computerUseCapabilityName) {
		t.Fatalf("expected acquire to succeed after release")
	}
}

// The single shared slot used to make a readImage call block computerUse.
func TestCommandSlotsAreIndependentPerCapability(t *testing.T) {
	slots := newCommandSlots(config.Config{})

	if !slots.tryAcquire(readImageCapabilityName) {
		t.Fatalf("expected readImage acquire to succeed")
	}
	if !slots.tryAcquire(computerUseCapabilityName) {
		t.Fatalf("computerUse must not be blocked by an in-flight readImage")
	}

	slots.release(readImageCapabilityName)
	slots.release(computerUseCapabilityName)
}

func TestCommandSlotsHonourConfiguredLimits(t *testing.T) {
	slots := newCommandSlots(config.Config{
		ComputerUseMaxInflight: 3,
		ReadImageMaxInflight:   2,
	})

	for i := 0; i < 3; i++ {
		if !slots.tryAcquire(computerUseCapabilityName) {
			t.Fatalf("expected computerUse acquire %d to succeed", i+1)
		}
	}
	if slots.tryAcquire(computerUseCapabilityName) {
		t.Fatalf("expected computerUse to be exhausted after 3 acquires")
	}

	for i := 0; i < 2; i++ {
		if !slots.tryAcquire(readImageCapabilityName) {
			t.Fatalf("expected readImage acquire %d to succeed", i+1)
		}
	}
	if slots.tryAcquire(readImageCapabilityName) {
		t.Fatalf("expected readImage to be exhausted after 2 acquires")
	}
}

// Unknown capabilities must reach the executor so it can report
// unsupported_capability instead of a misleading session_busy.
func TestCommandSlotsAllowUnknownCapability(t *testing.T) {
	slots := newCommandSlots(config.Config{})

	if !slots.tryAcquire("nosuchcapability") {
		t.Fatalf("unknown capability must not be rejected by the slot pool")
	}
	slots.release("nosuchcapability")
}

func TestHandleCommandDispatchConcurrentComputerUse(t *testing.T) {
	slots := newCommandSlots(config.Config{ComputerUseMaxInflight: 2})

	outbound := make(chan *registryv1.ConnectRequest, 4)
	errCh := make(chan error, 4)
	release := make(chan struct{})
	started := make(chan struct{}, 4)

	execute := func(context.Context, *registryv1.CommandDispatch) *registryv1.ConnectRequest {
		started <- struct{}{}
		<-release
		return commandErrorResult("cmd", "execution_failed", "done")
	}

	for i := 0; i < 2; i++ {
		ok := handleCommandDispatch(
			context.Background(),
			outbound,
			errCh,
			slots,
			&registryv1.CommandDispatch{
				CommandId:   "cmd-concurrent",
				Capability:  computerUseCapabilityDeclared,
				PayloadJson: []byte(`{"command":"pwd"}`),
			},
			execute,
		)
		if !ok {
			t.Fatalf("dispatch %d should have been accepted", i+1)
		}
	}

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for concurrent execution %d", i+1)
		}
	}

	// A third dispatch exceeds the configured limit and reports session_busy.
	ok := handleCommandDispatch(
		context.Background(),
		outbound,
		errCh,
		slots,
		&registryv1.CommandDispatch{
			CommandId:   "cmd-overflow",
			Capability:  computerUseCapabilityDeclared,
			PayloadJson: []byte(`{"command":"pwd"}`),
		},
		execute,
	)
	if !ok {
		t.Fatalf("expected overflow dispatch to be handled")
	}

	select {
	case req := <-outbound:
		result := req.GetCommandResult()
		if result == nil {
			t.Fatalf("expected command_result payload")
		}
		if result.GetError().GetCode() != sessionBusyErrorCode {
			t.Fatalf("expected %s, got %q", sessionBusyErrorCode, result.GetError().GetCode())
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for session_busy result")
	}

	close(release)
}
