package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b"
)

type fakePythonBackend struct {
	mu          sync.Mutex
	template    string
	timeoutSec  int
	command     string
	outputLimit int
	killed      []string
	runResult   e2b.CommandResult
	runErr      error
}

func (f *fakePythonBackend) Create(_ context.Context, template string, timeoutSec int) (*e2b.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.template = template
	f.timeoutSec = timeoutSec
	return &e2b.Sandbox{ID: "python-sandbox"}, nil
}

func (f *fakePythonBackend) SetTimeout(context.Context, string, int) error { return nil }

func (f *fakePythonBackend) Kill(_ context.Context, sandboxID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, sandboxID)
	return nil
}

func (f *fakePythonBackend) Run(_ context.Context, _ *e2b.Sandbox, command string, outputLimit int) (e2b.CommandResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.command = command
	f.outputLimit = outputLimit
	return f.runResult, f.runErr
}

func (f *fakePythonBackend) ReadFile(context.Context, *e2b.Sandbox, string, int64) (e2b.File, error) {
	return e2b.File{}, nil
}

func (f *fakePythonBackend) OpenFile(context.Context, *e2b.Sandbox, string) (e2b.FileReader, error) {
	return e2b.FileReader{Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

func TestPythonExecUsesOneShotUVSandboxAndAlwaysCleansUp(t *testing.T) {
	t.Parallel()
	backend := &fakePythonBackend{
		runResult: e2b.CommandResult{Stdout: "ok\n", Stderr: "warning\n", ExitCode: 3},
	}
	runner := newPythonExecRunner(backend, "python-template", 300)
	result, err := runner.Execute(context.Background(), `print("sensitive source")`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "ok\n" || result.Stderr != "warning\n" || result.ExitCode != 3 {
		t.Fatalf("unexpected result: %#v", result)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.template != "python-template" || backend.timeoutSec != 300 {
		t.Fatalf("unexpected create request: template=%q timeout=%d", backend.template, backend.timeoutSec)
	}
	if !strings.Contains(backend.command, "uv run /tmp/onlyboxes-pythonexec.py") {
		t.Fatalf("python command does not invoke uv: %q", backend.command)
	}
	if strings.Contains(backend.command, "sensitive source") {
		t.Fatalf("raw source was embedded in the shell command")
	}
	if backend.outputLimit != -1 {
		t.Fatalf("python output should be unbounded, got %d", backend.outputLimit)
	}
	if len(backend.killed) != 1 || backend.killed[0] != "python-sandbox" {
		t.Fatalf("sandbox was not cleaned up: %v", backend.killed)
	}
}

func TestPythonExecBoundsSandboxTTLByCommandDeadline(t *testing.T) {
	t.Parallel()
	backend := &fakePythonBackend{}
	runner := newPythonExecRunner(backend, "python-template", 300)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, err := runner.Execute(ctx, "pass"); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.timeoutSec < 1 || backend.timeoutSec > 2 {
		t.Fatalf("expected deadline-bounded sandbox timeout, got %d", backend.timeoutSec)
	}
}

func TestPythonExecCleansUpAfterRunFailure(t *testing.T) {
	t.Parallel()
	backend := &fakePythonBackend{runErr: errors.New("stream failed")}
	runner := newPythonExecRunner(backend, "python-template", 60)
	if _, err := runner.Execute(context.Background(), "pass"); err == nil {
		t.Fatal("expected run error")
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.killed) != 1 || backend.killed[0] != "python-sandbox" {
		t.Fatalf("sandbox was not cleaned up: %v", backend.killed)
	}
}
