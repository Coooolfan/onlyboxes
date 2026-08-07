package e2b

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestIntegrationSandboxCommandAndFile(t *testing.T) {
	if os.Getenv("E2B_INTEGRATION") != "1" {
		t.Skip("set E2B_INTEGRATION=1 to run against E2B")
	}
	apiKey := strings.TrimSpace(os.Getenv("E2B_API_KEY"))
	template := strings.TrimSpace(os.Getenv("E2B_TERMINAL_TEMPLATE"))
	if apiKey == "" || template == "" {
		t.Fatal("E2B_API_KEY and E2B_TERMINAL_TEMPLATE are required")
	}
	client, err := NewClient(Config{
		APIKey:         apiKey,
		APIURL:         strings.TrimSpace(os.Getenv("E2B_API_URL")),
		Domain:         strings.TrimSpace(os.Getenv("E2B_DOMAIN")),
		RequestTimeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sandbox, err := client.Create(ctx, template, 120)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := client.Kill(cleanupCtx, sandbox.ID); err != nil {
			t.Errorf("cleanup sandbox: %v", err)
		}
	})
	result, err := client.Run(
		ctx,
		sandbox,
		`printf 'hello-e2b' > /tmp/onlyboxes-e2b-smoke.txt && printf 'command-ok'`,
		1024,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Stdout != "command-ok" {
		t.Fatalf("unexpected command result: %#v", result)
	}
	file, err := client.ReadFile(ctx, sandbox, "/tmp/onlyboxes-e2b-smoke.txt", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(file.Content) != "hello-e2b" {
		t.Fatalf("unexpected file content %q", string(file.Content))
	}
	if err := client.SetTimeout(ctx, sandbox.ID, 120); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationSandboxRecoveryByMetadata(t *testing.T) {
	if os.Getenv("E2B_INTEGRATION") != "1" {
		t.Skip("set E2B_INTEGRATION=1 to run against E2B")
	}
	apiKey := strings.TrimSpace(os.Getenv("E2B_API_KEY"))
	template := strings.TrimSpace(os.Getenv("E2B_TERMINAL_TEMPLATE"))
	if apiKey == "" || template == "" {
		t.Fatal("E2B_API_KEY and E2B_TERMINAL_TEMPLATE are required")
	}
	client, err := NewClient(Config{
		APIKey:         apiKey,
		APIURL:         strings.TrimSpace(os.Getenv("E2B_API_URL")),
		Domain:         strings.TrimSpace(os.Getenv("E2B_DOMAIN")),
		RequestTimeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	marker := fmt.Sprintf("integration-%d", time.Now().UnixNano())
	metadata := map[string]string{
		"onlyboxes.session_id_hash": marker,
		"onlyboxes.schema_version":  "1",
	}
	sandbox, err := client.CreateWithMetadata(ctx, template, 120, metadata)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := client.Kill(cleanupCtx, sandbox.ID); err != nil {
			t.Errorf("cleanup sandbox: %v", err)
		}
	})
	if _, err := client.Run(ctx, sandbox, `printf 'recovery-ok' > /tmp/onlyboxes-recovery.txt`, 1024); err != nil {
		t.Fatal(err)
	}

	infos, err := client.List(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	var found *SandboxInfo
	for index := range infos {
		if infos[index].ID == sandbox.ID {
			found = &infos[index]
			break
		}
	}
	if found == nil || found.Metadata["onlyboxes.session_id_hash"] != marker {
		t.Fatalf("created sandbox was not discoverable by metadata: %#v", infos)
	}

	reconnected, err := client.Connect(ctx, sandbox.ID, 90)
	if err != nil {
		t.Fatal(err)
	}
	if reconnected.ID != sandbox.ID || strings.TrimSpace(reconnected.AccessToken) == "" {
		t.Fatalf("connect did not return fresh sandbox credentials: %#v", reconnected)
	}
	result, err := client.Run(ctx, reconnected, `cat /tmp/onlyboxes-recovery.txt`, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Stdout != "recovery-ok" {
		t.Fatalf("reconnected sandbox lost filesystem state: %#v", result)
	}
}

func TestIntegrationSandboxSurvivesForcedWorkerTermination(t *testing.T) {
	if os.Getenv("E2B_INTEGRATION") != "1" {
		t.Skip("set E2B_INTEGRATION=1 to run against E2B")
	}
	if os.Getenv("E2B_RECOVERY_CRASH_HELPER") == "1" {
		runE2BRecoveryCrashHelper(t)
		return
	}
	apiKey := strings.TrimSpace(os.Getenv("E2B_API_KEY"))
	template := strings.TrimSpace(os.Getenv("E2B_TERMINAL_TEMPLATE"))
	if apiKey == "" || template == "" {
		t.Fatal("E2B_API_KEY and E2B_TERMINAL_TEMPLATE are required")
	}
	client, err := NewClient(Config{
		APIKey: apiKey, APIURL: strings.TrimSpace(os.Getenv("E2B_API_URL")),
		Domain: strings.TrimSpace(os.Getenv("E2B_DOMAIN")), RequestTimeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("forced-integration-%d", time.Now().UnixNano())
	metadata := map[string]string{
		"onlyboxes.worker":          "worker-bridge-e2b",
		"onlyboxes.session_id_hash": marker,
		"onlyboxes.schema_version":  "1",
	}
	readyFile, err := os.CreateTemp("", "onlyboxes-e2b-crash-ready-*")
	if err != nil {
		t.Fatal(err)
	}
	readyPath := readyFile.Name()
	_ = readyFile.Close()
	_ = os.Remove(readyPath)
	t.Cleanup(func() { _ = os.Remove(readyPath) })
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestIntegrationSandboxSurvivesForcedWorkerTermination$", "-test.v")
	cmd.Env = append(os.Environ(),
		"E2B_RECOVERY_CRASH_HELPER=1",
		"E2B_RECOVERY_READY_FILE="+readyPath,
		"E2B_RECOVERY_METADATA="+string(metadataJSON),
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	sandboxID := ""
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for sandboxID == "" {
		select {
		case err := <-waitCh:
			t.Fatalf("crash helper exited before creating sandbox: %v", err)
		case <-deadline.C:
			_ = cmd.Process.Kill()
			<-waitCh
			t.Fatal("timed out waiting for E2B crash helper")
		case <-ticker.C:
			content, readErr := os.ReadFile(readyPath)
			if readErr == nil {
				sandboxID = strings.TrimSpace(string(content))
			}
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-waitCh; err == nil {
		t.Fatal("crash helper was not force terminated")
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.Kill(cleanupCtx, sandboxID); err != nil {
			t.Errorf("cleanup sandbox: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	infos, err := client.List(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, info := range infos {
		if info.ID == sandboxID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("sandbox %q was not discoverable after worker termination", sandboxID)
	}
	reconnected, err := client.Connect(ctx, sandboxID, 90)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Run(ctx, reconnected, "cat /tmp/onlyboxes-forced-recovery.txt", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Stdout != "forced-recovery-ok" {
		t.Fatalf("sandbox lost state after forced worker termination: %#v", result)
	}
}

func runE2BRecoveryCrashHelper(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("E2B_API_KEY"))
	template := strings.TrimSpace(os.Getenv("E2B_TERMINAL_TEMPLATE"))
	metadata := map[string]string{}
	if err := json.Unmarshal([]byte(os.Getenv("E2B_RECOVERY_METADATA")), &metadata); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{
		APIKey: apiKey, APIURL: strings.TrimSpace(os.Getenv("E2B_API_URL")),
		Domain: strings.TrimSpace(os.Getenv("E2B_DOMAIN")), RequestTimeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sandbox, err := client.CreateWithMetadata(ctx, template, 180, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(ctx, sandbox, "printf forced-recovery-ok > /tmp/onlyboxes-forced-recovery.txt", 1024); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strings.TrimSpace(os.Getenv("E2B_RECOVERY_READY_FILE")), []byte(sandbox.ID), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}
