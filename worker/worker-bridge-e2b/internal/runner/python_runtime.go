package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/logging"
)

const pythonExecCleanupTimeout = 10 * time.Second

type pythonExecRunner struct {
	backend    e2bBackend
	template   string
	timeoutSec int
}

func newPythonExecRunner(backend e2bBackend, template string, timeoutSec int) *pythonExecRunner {
	return &pythonExecRunner{
		backend:    backend,
		template:   strings.TrimSpace(template),
		timeoutSec: timeoutSec,
	}
}

func (r *pythonExecRunner) Execute(ctx context.Context, code string) (pythonExecRunResult, error) {
	if r == nil || r.backend == nil {
		return pythonExecRunResult{}, errors.New("E2B python executor is unavailable")
	}
	timeoutSec := r.timeoutSec
	if deadline, ok := ctx.Deadline(); ok {
		remaining := int(time.Until(deadline).Seconds()) + 1
		if remaining > 0 && remaining < timeoutSec {
			timeoutSec = remaining
		}
	}
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	sandbox, err := r.backend.Create(ctx, r.template, timeoutSec)
	if err != nil {
		return pythonExecRunResult{}, fmt.Errorf("create E2B python sandbox: %w", err)
	}
	defer r.killSandbox(sandbox.ID)

	encoded := base64.StdEncoding.EncodeToString([]byte(code))
	command := "printf '%s' '" + encoded + "' | base64 -d > /tmp/onlyboxes-pythonexec.py && uv run /tmp/onlyboxes-pythonexec.py"
	result, err := r.backend.Run(ctx, sandbox, command, -1)
	if err != nil {
		return pythonExecRunResult{}, err
	}
	return pythonExecRunResult{
		Output:   result.Stdout,
		Stderr:   result.Stderr,
		ExitCode: result.ExitCode,
	}, nil
}

func (r *pythonExecRunner) killSandbox(sandboxID string) {
	ctx, cancel := context.WithTimeout(context.Background(), pythonExecCleanupTimeout)
	defer cancel()
	if err := r.backend.Kill(ctx, sandboxID); err != nil {
		logging.Warnf("pythonExec cleanup failed: sandbox_id=%s err=%v", sandboxID, err)
	}
}
