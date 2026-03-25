package runner

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/config"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/logging"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	minHeartbeatInterval           = 1 * time.Second
	initialReconnectDelay          = 1 * time.Second
	maxReconnectDelay              = 15 * time.Second
	echoCapabilityName             = "echo"
	pythonExecCapabilityName       = "pythonexec"
	pythonExecCapabilityDeclared   = "pythonExec"
	defaultPythonExecDockerImage   = "ghcr.io/astral-sh/uv:python3.12-bookworm-slim"
	defaultPythonExecMemoryLimit   = "256m"
	defaultPythonExecCPULimit      = "1.0"
	defaultPythonExecPidsLimit     = 128
	defaultTerminalExecDockerImage = "coolfan1024/onlyboxes-default-worker:0.0.3"
	defaultTerminalExecMemoryLimit = "256m"
	defaultTerminalExecCPULimit    = "1.0"
	defaultTerminalExecPidsLimit   = 128
	pythonExecContainerPrefix      = "onlyboxes-pythonexec-"
	pythonExecManagedLabel         = "onlyboxes.managed=true"
	pythonExecCapabilityLabel      = "onlyboxes.capability=pythonExec"
	pythonExecRuntimeLabel         = "onlyboxes.runtime=worker-docker"
	pythonExecCleanupTimeout       = 3 * time.Second
	pythonExecInspectTimeout       = 2 * time.Second
	defaultMaxInflight             = 4
)

var waitReconnect = waitReconnectDelay
var applyJitter = jitterDuration
var runPythonExec = newPythonExecRunner("").Execute
var runTerminalExec = runTerminalExecUnavailable
var runTerminalResource = runTerminalResourceUnavailable
var runDockerCommand = runDockerCommandCLI
var pythonExecContainerNameFn = newPythonExecContainerName

func Run(ctx context.Context, cfg config.Config) error {
	if strings.TrimSpace(cfg.WorkerID) == "" {
		return errors.New("WORKER_ID is required")
	}
	if strings.TrimSpace(cfg.WorkerSecret) == "" {
		return errors.New("WORKER_SECRET is required")
	}

	terminalManager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      cfg.TerminalLeaseMinSec,
		LeaseMaxSec:      cfg.TerminalLeaseMaxSec,
		LeaseDefaultSec:  cfg.TerminalLeaseDefaultSec,
		OutputLimitBytes: cfg.TerminalOutputLimitBytes,
		DockerImage:      cfg.TerminalExecDockerImage,
		MemoryLimit:      defaultTerminalExecMemoryLimit,
		CPULimit:         defaultTerminalExecCPULimit,
		PidsLimit:        defaultTerminalExecPidsLimit,
	})
	pythonRunner := newPythonExecRunner(cfg.PythonExecDockerImage)
	originalRunPythonExec := runPythonExec
	runPythonExec = pythonRunner.Execute
	originalRunTerminalExec := runTerminalExec
	runTerminalExec = terminalManager.Execute
	originalRunTerminalResource := runTerminalResource
	runTerminalResource = terminalManager.ResolveResource
	defer func() {
		runPythonExec = originalRunPythonExec
		runTerminalExec = originalRunTerminalExec
		runTerminalResource = originalRunTerminalResource
		terminalManager.Close()
	}()

	pythonImage := strings.TrimSpace(cfg.PythonExecDockerImage)
	if pythonImage == "" {
		pythonImage = defaultPythonExecDockerImage
	}
	logging.Infof("pythonExec configured: image=%s", pythonImage)
	logging.Infof(
		"terminalExec configured: image=%s",
		terminalManager.dockerImage,
	)
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
