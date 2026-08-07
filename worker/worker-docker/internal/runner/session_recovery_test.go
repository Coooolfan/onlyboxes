package runner

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

func TestTerminalRecoveryResourceNameUsesFullHashWithoutSessionID(t *testing.T) {
	sessionID := "owner-secret:session-secret"
	name := terminalSessionResourceName(sessionID)
	if !strings.HasPrefix(name, terminalExecContainerPrefix) || len(name) != len(terminalExecContainerPrefix)+64 {
		t.Fatalf("unexpected recovery resource name %q", name)
	}
	if strings.Contains(name, "owner-secret") || name != terminalSessionResourceName(sessionID) {
		t.Fatalf("resource name exposed or did not deterministically map the session: %q", name)
	}
}

func TestTerminalRecoveryRestoresContainerAndExactLease(t *testing.T) {
	original := runDockerCommand
	t.Cleanup(func() { runDockerCommand = original })

	sessionID := "owner-a:session-a"
	name := terminalSessionResourceName(sessionID)
	hash := terminalSessionIDHash(sessionID)
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		joined := strings.Join(args, " ")
		switch {
		case len(args) > 0 && args[0] == "inspect":
			return dockerCommandResult{ExitCode: 0, Stdout: fmt.Sprintf("{\"%s\":\"%s\",\"%s\":\"1\"}\trunning\n", terminalExecSessionLabelKey, hash, terminalExecSchemaLabelKey)}
		case strings.Contains(joined, "label="+terminalExecSessionLabelKey+"="+hash):
			return dockerCommandResult{ExitCode: 0, Stdout: name + "\n"}
		case len(args) > 0 && args[0] == "ps":
			return dockerCommandResult{ExitCode: 0, Stdout: name + "\n"}
		default:
			t.Fatalf("unexpected docker command: %v", args)
			return dockerCommandResult{ExitCode: 1}
		}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{PreserveOnClose: true})
	defer manager.Close()
	lease := time.Now().Add(10 * time.Minute).Truncate(time.Millisecond)
	results := manager.Recover(context.Background(), []*registryv1.TerminalSessionRecoveryCandidate{
		{SessionId: sessionID, LeaseExpiresUnixMs: lease.UnixMilli()},
	})
	if len(results) != 1 || results[0].GetStatus() != registryv1.TerminalSessionRecoveryResult_RECOVERED {
		t.Fatalf("unexpected recovery result: %#v", results)
	}
	if manager.ActiveSessionCount() != 1 {
		t.Fatalf("active session count=%d, want 1", manager.ActiveSessionCount())
	}
	manager.mu.Lock()
	recovered := manager.sessions[sessionID]
	manager.mu.Unlock()
	if recovered == nil || !recovered.leaseExpiresAt.Equal(lease) || recovered.inflight != 0 {
		t.Fatalf("unexpected recovered session: %#v", recovered)
	}
}

func TestTerminalRecoveryRejectsAndRemovesDuplicateContainers(t *testing.T) {
	original := runDockerCommand
	t.Cleanup(func() { runDockerCommand = original })

	sessionID := "owner-a:session-duplicate"
	name := terminalSessionResourceName(sessionID)
	hash := terminalSessionIDHash(sessionID)
	removed := map[string]bool{}
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		joined := strings.Join(args, " ")
		switch {
		case len(args) > 0 && args[0] == "inspect":
			return dockerCommandResult{ExitCode: 0, Stdout: fmt.Sprintf("{\"%s\":\"%s\",\"%s\":\"1\"}\trunning\n", terminalExecSessionLabelKey, hash, terminalExecSchemaLabelKey)}
		case strings.Contains(joined, "label="+terminalExecSessionLabelKey+"="+hash):
			return dockerCommandResult{ExitCode: 0, Stdout: name + "\n" + name + "-duplicate\n"}
		case len(args) > 0 && args[0] == "rm":
			removed[args[len(args)-1]] = true
			return dockerCommandResult{ExitCode: 0}
		case len(args) > 0 && args[0] == "ps":
			return dockerCommandResult{ExitCode: 0}
		default:
			t.Fatalf("unexpected docker command: %v", args)
			return dockerCommandResult{ExitCode: 1}
		}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{PreserveOnClose: true})
	defer manager.Close()
	results := manager.Recover(context.Background(), []*registryv1.TerminalSessionRecoveryCandidate{
		{SessionId: sessionID, LeaseExpiresUnixMs: time.Now().Add(time.Minute).UnixMilli()},
	})
	if len(results) != 1 || results[0].GetStatus() != registryv1.TerminalSessionRecoveryResult_INVALID {
		t.Fatalf("unexpected recovery result: %#v", results)
	}
	if !removed[name] || !removed[name+"-duplicate"] {
		t.Fatalf("duplicate containers were not isolated: %#v", removed)
	}
}

func TestTerminalRecoveryRemovesContainerWithInvalidLabels(t *testing.T) {
	original := runDockerCommand
	t.Cleanup(func() { runDockerCommand = original })

	sessionID := "owner-a:session-invalid"
	name := terminalSessionResourceName(sessionID)
	removed := false
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		switch {
		case len(args) > 0 && args[0] == "inspect":
			return dockerCommandResult{ExitCode: 0, Stdout: fmt.Sprintf("{\"%s\":\"wrong\",\"%s\":\"1\"}\trunning\n", terminalExecSessionLabelKey, terminalExecSchemaLabelKey)}
		case len(args) > 0 && args[0] == "rm":
			removed = args[len(args)-1] == name
			return dockerCommandResult{ExitCode: 0}
		case len(args) > 0 && args[0] == "ps":
			return dockerCommandResult{ExitCode: 0}
		default:
			t.Fatalf("unexpected docker command: %v", args)
			return dockerCommandResult{ExitCode: 1}
		}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{PreserveOnClose: true})
	defer manager.Close()
	results := manager.Recover(context.Background(), []*registryv1.TerminalSessionRecoveryCandidate{
		{SessionId: sessionID, LeaseExpiresUnixMs: time.Now().Add(time.Minute).UnixMilli()},
	})
	if len(results) != 1 || results[0].GetStatus() != registryv1.TerminalSessionRecoveryResult_INVALID {
		t.Fatalf("unexpected recovery result: %#v", results)
	}
	if !removed {
		t.Fatal("container with invalid recovery labels was not isolated")
	}
}

func TestTerminalRecoveryIsIdempotentAndCleansOnlyOwnedOrphans(t *testing.T) {
	original := runDockerCommand
	t.Cleanup(func() { runDockerCommand = original })

	sessionID := "owner-a:session-idempotent"
	name := terminalSessionResourceName(sessionID)
	orphan := terminalExecContainerPrefix + "orphan"
	hash := terminalSessionIDHash(sessionID)
	inspectCalls := 0
	removed := map[string]bool{}
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		joined := strings.Join(args, " ")
		switch {
		case len(args) > 0 && args[0] == "inspect":
			inspectCalls++
			return dockerCommandResult{ExitCode: 0, Stdout: fmt.Sprintf("{\"%s\":\"%s\",\"%s\":\"1\"}\trunning\n", terminalExecSessionLabelKey, hash, terminalExecSchemaLabelKey)}
		case strings.Contains(joined, "label="+terminalExecSessionLabelKey+"="+hash):
			return dockerCommandResult{ExitCode: 0, Stdout: name + "\n"}
		case len(args) > 0 && args[0] == "ps":
			return dockerCommandResult{ExitCode: 0, Stdout: name + "\n" + orphan + "\nforeign-container\n"}
		case len(args) > 0 && args[0] == "rm":
			removed[args[len(args)-1]] = true
			return dockerCommandResult{ExitCode: 0}
		default:
			t.Fatalf("unexpected docker command: %v", args)
			return dockerCommandResult{ExitCode: 1}
		}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{PreserveOnClose: true})
	defer manager.Close()
	lease := time.Now().Add(time.Minute).Truncate(time.Millisecond)
	candidate := []*registryv1.TerminalSessionRecoveryCandidate{{SessionId: sessionID, LeaseExpiresUnixMs: lease.UnixMilli()}}
	for range 2 {
		results := manager.Recover(context.Background(), candidate)
		if len(results) != 1 || results[0].GetStatus() != registryv1.TerminalSessionRecoveryResult_RECOVERED {
			t.Fatalf("unexpected recovery result: %#v", results)
		}
	}
	if inspectCalls != 1 || manager.ActiveSessionCount() != 1 {
		t.Fatalf("recovery was not idempotent: inspect_calls=%d active=%d", inspectCalls, manager.ActiveSessionCount())
	}
	if !removed[orphan] || removed[name] || removed["foreign-container"] {
		t.Fatalf("unsafe orphan cleanup: %#v", removed)
	}
}

func TestTerminalRecoveryReportsMissingAndCleansExpiredCandidate(t *testing.T) {
	original := runDockerCommand
	t.Cleanup(func() { runDockerCommand = original })

	missingID := "owner-a:session-missing"
	expiredID := "owner-a:session-expired"
	expiredName := terminalSessionResourceName(expiredID)
	removedExpired := false
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		switch {
		case len(args) > 0 && args[0] == "inspect":
			return dockerCommandResult{ExitCode: 1, Stderr: "No such container"}
		case len(args) > 0 && args[0] == "rm":
			if args[len(args)-1] == expiredName {
				removedExpired = true
			}
			return dockerCommandResult{ExitCode: 0}
		case len(args) > 0 && args[0] == "ps":
			return dockerCommandResult{ExitCode: 0}
		default:
			t.Fatalf("unexpected docker command: %v", args)
			return dockerCommandResult{ExitCode: 1}
		}
	}
	manager := newTerminalSessionManager(terminalSessionManagerConfig{PreserveOnClose: true})
	defer manager.Close()
	results := manager.Recover(context.Background(), []*registryv1.TerminalSessionRecoveryCandidate{
		{SessionId: missingID, LeaseExpiresUnixMs: time.Now().Add(time.Minute).UnixMilli()},
		{SessionId: expiredID, LeaseExpiresUnixMs: time.Now().Add(-time.Minute).UnixMilli()},
	})
	if results[0].GetStatus() != registryv1.TerminalSessionRecoveryResult_MISSING ||
		results[1].GetStatus() != registryv1.TerminalSessionRecoveryResult_INVALID {
		t.Fatalf("unexpected recovery results: %#v", results)
	}
	if !removedExpired || manager.ActiveSessionCount() != 0 {
		t.Fatalf("expired resource was not cleaned: removed=%v active=%d", removedExpired, manager.ActiveSessionCount())
	}
}

func TestConcurrentTerminalRecoveryUsesOneReservation(t *testing.T) {
	original := runDockerCommand
	t.Cleanup(func() { runDockerCommand = original })
	sessionID := "owner-a:session-concurrent"
	name := terminalSessionResourceName(sessionID)
	hash := terminalSessionIDHash(sessionID)
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		joined := strings.Join(args, " ")
		switch {
		case len(args) > 0 && args[0] == "inspect":
			return dockerCommandResult{ExitCode: 0, Stdout: fmt.Sprintf("{\"%s\":\"%s\",\"%s\":\"1\"}\trunning\n", terminalExecSessionLabelKey, hash, terminalExecSchemaLabelKey)}
		case strings.Contains(joined, "label="+terminalExecSessionLabelKey+"="+hash):
			return dockerCommandResult{ExitCode: 0, Stdout: name + "\n"}
		case len(args) > 0 && args[0] == "ps":
			return dockerCommandResult{ExitCode: 0, Stdout: name + "\n"}
		default:
			t.Fatalf("unexpected docker command: %v", args)
			return dockerCommandResult{ExitCode: 1}
		}
	}
	manager := newTerminalSessionManager(terminalSessionManagerConfig{PreserveOnClose: true})
	defer manager.Close()
	candidate := []*registryv1.TerminalSessionRecoveryCandidate{{
		SessionId: sessionID, LeaseExpiresUnixMs: time.Now().Add(time.Minute).UnixMilli(),
	}}
	done := make(chan []*registryv1.TerminalSessionRecoveryResult, 2)
	for range 2 {
		go func() { done <- manager.Recover(context.Background(), candidate) }()
	}
	for range 2 {
		if result := <-done; len(result) != 1 || result[0].GetStatus() != registryv1.TerminalSessionRecoveryResult_RECOVERED {
			t.Fatalf("unexpected concurrent recovery result: %#v", result)
		}
	}
	if manager.ActiveSessionCount() != 1 {
		t.Fatalf("concurrent recovery reserved %d sessions, want 1", manager.ActiveSessionCount())
	}
}
