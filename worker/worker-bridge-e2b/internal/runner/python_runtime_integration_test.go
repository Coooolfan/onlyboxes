package runner

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b"
)

func TestIntegrationPythonExec(t *testing.T) {
	if os.Getenv("E2B_INTEGRATION") != "1" {
		t.Skip("set E2B_INTEGRATION=1 to run against E2B")
	}
	apiKey := strings.TrimSpace(os.Getenv("E2B_API_KEY"))
	template := strings.TrimSpace(os.Getenv("E2B_PYTHON_TEMPLATE"))
	if apiKey == "" || template == "" {
		t.Fatal("E2B_API_KEY and E2B_PYTHON_TEMPLATE are required")
	}
	backend, err := e2b.NewClient(e2b.Config{
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
	result, err := newPythonExecRunner(backend, template, 120).Execute(
		ctx,
		`print("python-template-ok")`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || strings.TrimSpace(result.Output) != "python-template-ok" {
		t.Fatalf("unexpected python result: %#v", result)
	}
}
