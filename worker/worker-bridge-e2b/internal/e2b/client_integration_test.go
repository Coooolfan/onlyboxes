package e2b

import (
	"context"
	"os"
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
