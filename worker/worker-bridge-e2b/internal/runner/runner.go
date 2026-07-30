package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/config"
	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b"
	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/logging"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	minHeartbeatInterval         = 1 * time.Second
	initialReconnectDelay        = 1 * time.Second
	maxReconnectDelay            = 15 * time.Second
	echoCapabilityName           = "echo"
	pythonExecCapabilityName     = "pythonexec"
	pythonExecCapabilityDeclared = "pythonExec"
)

var waitReconnect = waitReconnectDelay
var applyJitter = jitterDuration
var runPythonExec = func(context.Context, string) (pythonExecRunResult, error) {
	return pythonExecRunResult{}, errors.New("E2B python executor is unavailable")
}
var runTerminalExec = runTerminalExecUnavailable
var runTerminalResource = runTerminalResourceUnavailable
var activeSessionCountFn = func() int32 { return 0 }

func Run(ctx context.Context, cfg config.Config) error {
	if strings.TrimSpace(cfg.WorkerID) == "" {
		return errors.New("WORKER_ID is required")
	}
	if strings.TrimSpace(cfg.WorkerSecret) == "" {
		return errors.New("WORKER_SECRET is required")
	}
	if strings.TrimSpace(cfg.E2BAPIKey) == "" {
		return errors.New("WORKER_E2B_API_KEY is required")
	}
	if strings.TrimSpace(cfg.E2BPythonTemplate) == "" {
		return errors.New("WORKER_E2B_PYTHON_TEMPLATE is required")
	}
	if strings.TrimSpace(cfg.E2BTerminalTemplate) == "" {
		return errors.New("WORKER_E2B_TERMINAL_TEMPLATE is required")
	}

	backend, err := e2b.NewClient(e2b.Config{
		APIKey:         cfg.E2BAPIKey,
		APIURL:         cfg.E2BAPIURL,
		Domain:         cfg.E2BDomain,
		SandboxURL:     cfg.E2BSandboxURL,
		RequestTimeout: cfg.E2BRequestTimeout,
	})
	if err != nil {
		return fmt.Errorf("configure E2B client: %w", err)
	}

	terminalManager := newTerminalSessionManager(terminalSessionManagerConfig{
		Backend:            backend,
		Template:           cfg.E2BTerminalTemplate,
		LeaseMinSec:        cfg.TerminalLeaseMinSec,
		LeaseMaxSec:        cfg.TerminalLeaseMaxSec,
		LeaseDefaultSec:    cfg.TerminalLeaseDefaultSec,
		OutputLimitBytes:   cfg.TerminalOutputLimitBytes,
		ExportMaxBytes:     cfg.TerminalExportMaxBytes,
		SessionMaxInflight: cfg.TerminalSessionMaxInflight,
	})
	pythonRunner := newPythonExecRunner(
		backend,
		cfg.E2BPythonTemplate,
		cfg.E2BPythonTimeoutSec,
	)
	originalRunPythonExec := runPythonExec
	runPythonExec = pythonRunner.Execute
	originalRunTerminalExec := runTerminalExec
	runTerminalExec = terminalManager.Execute
	originalRunTerminalResource := runTerminalResource
	runTerminalResource = terminalManager.ResolveResource
	originalActiveSessionCountFn := activeSessionCountFn
	activeSessionCountFn = terminalManager.ActiveSessionCount
	defer func() {
		runPythonExec = originalRunPythonExec
		runTerminalExec = originalRunTerminalExec
		runTerminalResource = originalRunTerminalResource
		activeSessionCountFn = originalActiveSessionCountFn
		terminalManager.Close()
	}()

	logging.Infof("pythonExec configured: backend=e2b template=%s", cfg.E2BPythonTemplate)
	logging.Infof("terminalExec configured: backend=e2b template=%s", cfg.E2BTerminalTemplate)
	logging.Infof(
		"terminalExec configured: lease_min_sec=%d lease_max_sec=%d lease_default_sec=%d output_limit_bytes=%d",
		terminalManager.leaseMinSec,
		terminalManager.leaseMaxSec,
		terminalManager.leaseDefaultSec,
		terminalManager.outputLimitBytes,
	)

	reconnectDelay := initialReconnectDelay
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := runSession(ctx, cfg)
		if err == nil {
			return nil
		}

		if errCtx := ctx.Err(); errCtx != nil {
			return errCtx
		}

		if status.Code(err) == codes.FailedPrecondition {
			logging.Warnf("registry session replaced for node_id=%s, reconnecting", cfg.WorkerID)
			reconnectDelay = initialReconnectDelay
		} else {
			logging.Warnf("registry session interrupted: %v", err)
		}

		if err := waitReconnect(ctx, reconnectDelay); err != nil {
			return err
		}
		reconnectDelay = nextReconnectDelay(reconnectDelay)
	}
}
