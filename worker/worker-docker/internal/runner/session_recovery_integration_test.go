package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

func TestIntegrationTerminalContainerSurvivesManagerRestart(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1 to run against Docker")
	}
	sessionID := "integration:docker-session-recovery"
	containerName := terminalSessionResourceName(sessionID)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = runDockerCommand(cleanupCtx, pythonExecDockerRemoveArgs(containerName)...)
	})

	baseConfig := terminalSessionManagerConfig{
		LeaseMinSec:        1,
		LeaseMaxSec:        600,
		LeaseDefaultSec:    300,
		DockerImage:        defaultTerminalExecDockerImage,
		SessionMaxInflight: 1,
	}
	firstConfig := baseConfig
	firstConfig.PreserveOnClose = true
	first := newTerminalSessionManager(firstConfig)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	created, err := first.Execute(ctx, terminalExecRequest{
		Command:         "printf recovery-ok > /tmp/onlyboxes-recovery.txt",
		SessionID:       sessionID,
		CreateIfMissing: true,
		LeaseTTLSec:     intPointerForDockerRecovery(300),
	})
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	if !created.Created {
		first.Close()
		t.Fatal("initial session was not created")
	}
	first.Close()

	lease := time.UnixMilli(created.LeaseExpiresUnixMS)
	second := newTerminalSessionManager(baseConfig)
	defer second.Close()
	if status := second.recoverOne(ctx, sessionID, containerName, lease); status != registryv1.TerminalSessionRecoveryResult_RECOVERED {
		t.Fatalf("recover status=%s", status)
	}
	recovered, err := second.Execute(ctx, terminalExecRequest{
		Command:     "cat /tmp/onlyboxes-recovery.txt",
		SessionID:   sessionID,
		LeaseTTLSec: intPointerForDockerRecovery(300),
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Created || recovered.Stdout != "recovery-ok" {
		t.Fatalf("unexpected recovered result: %#v", recovered)
	}
}

func intPointerForDockerRecovery(value int) *int { return &value }

func TestIntegrationTerminalContainerSurvivesForcedWorkerTermination(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1 to run against Docker")
	}
	if os.Getenv("DOCKER_RECOVERY_CRASH_HELPER") == "1" {
		runDockerRecoveryCrashHelper(t)
		return
	}

	sessionID := fmt.Sprintf("integration:docker-crash-recovery-%d", time.Now().UnixNano())
	containerName := terminalSessionResourceName(sessionID)
	readyFile, err := os.CreateTemp("", "onlyboxes-docker-crash-ready-*")
	if err != nil {
		t.Fatal(err)
	}
	readyPath := readyFile.Name()
	_ = readyFile.Close()
	_ = os.Remove(readyPath)
	t.Cleanup(func() {
		_ = os.Remove(readyPath)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = runDockerCommand(cleanupCtx, pythonExecDockerRemoveArgs(containerName)...)
	})
	cmd := exec.Command(os.Args[0], "-test.run=^TestIntegrationTerminalContainerSurvivesForcedWorkerTermination$", "-test.v")
	cmd.Env = append(os.Environ(), "DOCKER_RECOVERY_CRASH_HELPER=1", "DOCKER_RECOVERY_SESSION_ID="+sessionID, "DOCKER_RECOVERY_READY_FILE="+readyPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	leaseUnixMs := int64(0)
	deadline := time.NewTimer(time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for leaseUnixMs == 0 {
		select {
		case err := <-waitCh:
			t.Fatalf("crash helper exited before creating container: %v", err)
		case <-deadline.C:
			_ = cmd.Process.Kill()
			<-waitCh
			t.Fatal("timed out waiting for crash helper")
		case <-ticker.C:
			content, readErr := os.ReadFile(readyPath)
			if readErr != nil {
				continue
			}
			if _, scanErr := fmt.Sscan(strings.TrimSpace(string(content)), &leaseUnixMs); scanErr != nil {
				t.Fatalf("parse helper lease: %v", scanErr)
			}
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-waitCh; err == nil {
		t.Fatal("crash helper was not force terminated")
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec: 1, LeaseMaxSec: 600, LeaseDefaultSec: 300,
		DockerImage: defaultTerminalExecDockerImage, SessionMaxInflight: 1,
	})
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	results := manager.Recover(ctx, []*registryv1.TerminalSessionRecoveryCandidate{{
		SessionId: sessionID, LeaseExpiresUnixMs: leaseUnixMs,
	}})
	if len(results) != 1 || results[0].GetStatus() != registryv1.TerminalSessionRecoveryResult_RECOVERED {
		t.Fatalf("unexpected recovery result: %#v", results)
	}
	recovered, err := manager.Execute(ctx, terminalExecRequest{
		Command: "cat /tmp/onlyboxes-forced-recovery.txt", SessionID: sessionID,
		LeaseTTLSec: intPointerForDockerRecovery(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Created || recovered.Stdout != "forced-recovery-ok" {
		t.Fatalf("unexpected recovered result: %#v", recovered)
	}
}

func runDockerRecoveryCrashHelper(t *testing.T) {
	sessionID := strings.TrimSpace(os.Getenv("DOCKER_RECOVERY_SESSION_ID"))
	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec: 1, LeaseMaxSec: 600, LeaseDefaultSec: 300,
		DockerImage: defaultTerminalExecDockerImage, SessionMaxInflight: 1, PreserveOnClose: true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	created, err := manager.Execute(ctx, terminalExecRequest{
		Command:   "printf forced-recovery-ok > /tmp/onlyboxes-forced-recovery.txt",
		SessionID: sessionID, CreateIfMissing: true, LeaseTTLSec: intPointerForDockerRecovery(300),
	})
	if err != nil {
		t.Fatal(err)
	}
	readyPath := strings.TrimSpace(os.Getenv("DOCKER_RECOVERY_READY_FILE"))
	if err := os.WriteFile(readyPath, []byte(fmt.Sprintf("%d", created.LeaseExpiresUnixMS)), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}
