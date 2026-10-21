package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
	maxProtocolInt32               = int64(1<<31 - 1)
	echoCapabilityName             = "echo"
	pythonExecCapabilityName       = "pythonexec"
	pythonExecCapabilityDeclared   = "pythonExec"
	defaultPythonExecDockerImage   = "ghcr.io/astral-sh/uv:python3.12-bookworm-slim"
	defaultPythonExecMemoryLimit   = "256m"
	defaultPythonExecCPULimit      = "1.0"
	defaultPythonExecPidsLimit     = 128
	defaultTerminalExecDockerImage = "coolfan1024/onlyboxes-runtime:default"
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
var runPythonExec = newPythonExecRunner("", "", "", 0, "").Execute
var runTerminalExec = runTerminalExecUnavailable
var runTerminalLeaseRenew = runTerminalLeaseRenewUnavailable
var runTerminalResource = runTerminalResourceUnavailable
var runDockerCommand = runDockerCommandCLI
var pythonExecContainerNameFn = newPythonExecContainerName
var activeSessionCountFn = func() int32 { return 0 }

func Run(ctx context.Context, cfg config.Config) error {
	if strings.TrimSpace(cfg.WorkerID) == "" {
		return errors.New("WORKER_ID is required")
	}
	if strings.TrimSpace(cfg.WorkerSecret) == "" {
		return errors.New("WORKER_SECRET is required")
	}
	if err := validateTerminalMaxActiveSessions(cfg.TerminalMaxActiveSessions); err != nil {
		return err
	}
	if err := cfg.Snapshot.Validate(); err != nil {
		return err
	}
	if !cfg.Snapshot.Enabled && cfg.Snapshot.TmpDir != "" {
		if _, err := os.Lstat(filepath.Join(cfg.Snapshot.TmpDir, "journal.json")); err == nil {
			return errors.New("snapshot journal exists; persistence must remain enabled for recovery")
		}
	}
	if err := validateProxyConfig(cfg); err != nil {
		return err
	}
	terminalDockerNetwork, pythonDockerNetwork := resolveDockerNetworks(cfg)
	if terminalDockerNetwork != "" {
		if err := ensureSandboxDockerNetwork(ctx, terminalDockerNetwork); err != nil {
			return fmt.Errorf("initialize sandbox docker network: %w", err)
		}
	}

	terminalManager := newTerminalSessionManager(terminalSessionManagerConfig{
		PersistenceEnabled: cfg.Snapshot.Enabled,
		WorkerID:           cfg.WorkerID,
		LeaseMinSec:        cfg.TerminalLeaseMinSec,
		LeaseMaxSec:        cfg.TerminalLeaseMaxSec,
		LeaseDefaultSec:    cfg.TerminalLeaseDefaultSec,
		OutputLimitBytes:   cfg.TerminalOutputLimitBytes,
		ExportMaxBytes:     cfg.TerminalExportMaxBytes,
		DockerImage:        cfg.TerminalExecDockerImage,
		MemoryLimit:        cfg.TerminalExecMemoryLimit,
		CPULimit:           cfg.TerminalExecCPULimit,
		PidsLimit:          cfg.TerminalExecPidsLimit,
		DockerNetwork:      terminalDockerNetwork,
		SessionMaxInflight: cfg.TerminalSessionMaxInflight,
		MaxActiveSessions:  cfg.TerminalMaxActiveSessions,
		PreserveOnClose:    true,
	})
	var snapshots *snapshotController
	var snapshotMaintenanceDone chan struct{}
	var snapshotJournalFile *snapshotJournal
	if cfg.Snapshot.Enabled {
		var err error
		if _, statErr := os.Lstat(filepath.Join(cfg.Snapshot.TmpDir, "journal.json")); os.IsNotExist(statErr) {
			var owned bytesBuffer
			if err := dockerSnapshotCLI(ctx, nil, &owned, "ps", "-a", "--filter", "label=onlyboxes.worker_id="+cfg.WorkerID, "--filter", "label=onlyboxes.persistence=true", "--format", "{{.ID}}"); err != nil {
				terminalManager.Close()
				return err
			}
			if strings.TrimSpace(owned.String()) != "" {
				terminalManager.Close()
				return errors.New("persistent Docker resources exist but their operation journal is missing")
			}
		}
		snapshotJournalFile, err = openSnapshotJournal(cfg.Snapshot.TmpDir, cfg.WorkerID)
		if err != nil {
			terminalManager.Close()
			return err
		}
		backend, err := newDockerSnapshotRuntime(ctx, cfg.Snapshot.HelperImage)
		if err != nil {
			_ = snapshotJournalFile.Close()
			terminalManager.Close()
			return fmt.Errorf("initialize snapshot backend: %w", err)
		}
		leases, err := startSnapshotLeaseGuard(ctx, snapshotJournalFile, cfg.Snapshot.LeaseGuardImage, cfg.Snapshot.LeaseGuardDockerSocket, backend.platform)
		if err != nil {
			_ = snapshotJournalFile.Close()
			terminalManager.Close()
			return err
		}
		snapshots, err = newSnapshotController(ctx, terminalManager, snapshotJournalFile, backend, snapshotControllerConfig{LeaseGuard: leases, MaxInflight: cfg.Snapshot.MaxInflight, TmpQuotaBytes: cfg.Snapshot.TmpQuotaBytes, CacheMaxBytes: cfg.Snapshot.CacheMaxBytes, CacheTTL: cfg.Snapshot.CacheTTL, DrainTimeout: cfg.Snapshot.DrainTimeout, TransferConcurrency: cfg.Snapshot.TransferConcurrency, TransferIdleTimeout: cfg.Snapshot.TransferIdleTimeout})
		if err != nil {
			_ = snapshotJournalFile.Close()
			terminalManager.Close()
			return err
		}
		terminalManager.mu.Lock()
		terminalManager.persistenceDisconnected = true
		terminalManager.mu.Unlock()
	}
	originalSnapshots := workerSnapshots.Swap(snapshots)
	originalPersistenceRecovery := recoverPersistenceSessionsFn
	if snapshots != nil {
		recoverPersistenceSessionsFn = snapshots.Recover
	}
	defer func() {
		if snapshots != nil {
			_ = snapshots.Close(context.Background())
			stop, done := context.WithTimeout(context.Background(), 10*time.Second)
			if err := snapshots.stopForShutdown(stop); err != nil {
				logging.Warnf("snapshot backend shutdown could not confirm all retained resources stopped")
			}
			done()
		}
		workerSnapshots.Store(originalSnapshots)
		recoverPersistenceSessionsFn = originalPersistenceRecovery
		if snapshotMaintenanceDone != nil {
			<-snapshotMaintenanceDone
		}
		if snapshotJournalFile != nil {
			_ = snapshotJournalFile.Close()
		}
	}()
	pythonRunner := newPythonExecRunner(
		cfg.PythonExecDockerImage,
		cfg.PythonExecMemoryLimit,
		cfg.PythonExecCPULimit,
		cfg.PythonExecPidsLimit,
		pythonDockerNetwork,
	)
	originalRunPythonExec := runPythonExec
	runPythonExec = pythonRunner.Execute
	originalRunTerminalExec := runTerminalExec
	runTerminalExec = terminalManager.Execute
	originalRunTerminalLeaseRenew := runTerminalLeaseRenew
	runTerminalLeaseRenew = terminalManager.RenewLease
	originalRunTerminalResource := runTerminalResource
	runTerminalResource = terminalManager.ResolveResource
	originalActiveSessionCountFn := activeSessionCountFn
	activeSessionCountFn = terminalManager.ActiveSessionCount
	originalRecoverTerminalSessionsFn := recoverTerminalSessionsFn
	recoverTerminalSessionsFn = terminalManager.Recover
	defer func() {
		runPythonExec = originalRunPythonExec
		runTerminalExec = originalRunTerminalExec
		runTerminalLeaseRenew = originalRunTerminalLeaseRenew
		runTerminalResource = originalRunTerminalResource
		activeSessionCountFn = originalActiveSessionCountFn
		recoverTerminalSessionsFn = originalRecoverTerminalSessionsFn
		if snapshots != nil {
			_ = snapshots.Close(context.Background())
		}
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
		"terminalExec configured: lease_min_sec=%d lease_max_sec=%d lease_default_sec=%d output_limit_bytes=%d session_max_inflight=%d max_active_sessions=%d",
		terminalManager.leaseMinSec,
		terminalManager.leaseMaxSec,
		terminalManager.leaseDefaultSec,
		terminalManager.outputLimitBytes,
		terminalManager.sessionMaxInflight,
		terminalManager.maxActiveSessions,
	)

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if snapshots != nil {
		snapshotMaintenanceDone = make(chan struct{})
		go func() { defer close(snapshotMaintenanceDone); snapshots.maintain(runCtx) }()
	}
	var proxyErrCh chan error
	proxyWaited := false
	if cfg.ProxyEnabled {
		proxyHandler, err := newSandboxProxyHandler(cfg.WorkerID, cfg.WorkerSecret, terminalManager)
		if err != nil {
			return fmt.Errorf("initialize sandbox proxy: %w", err)
		}
		proxyErrCh = make(chan error, 1)
		proxyReadyCh := make(chan net.Addr, 1)
		go func() {
			err := runSandboxProxyServer(runCtx, cfg, proxyHandler, proxyReadyCh)
			proxyErrCh <- err
			cancelRun()
		}()
		select {
		case address := <-proxyReadyCh:
			logging.Infof("sandbox proxy listening: addr=%s advertise_addr=%s", address.String(), cfg.ProxyAdvertiseAddr)
		case err := <-proxyErrCh:
			proxyWaited = true
			return fmt.Errorf("sandbox proxy server: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		}
		defer func() {
			cancelRun()
			if !proxyWaited {
				<-proxyErrCh
			}
		}()
	}

	reconnectDelay := initialReconnectDelay
	for {
		if err := runCtx.Err(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if proxyErrCh != nil {
				proxyErr := <-proxyErrCh
				proxyWaited = true
				return fmt.Errorf("sandbox proxy server: %w", proxyErr)
			}
			return err
		}

		err := runSession(runCtx, cfg)
		if err == nil {
			return nil
		}

		if errCtx := runCtx.Err(); errCtx != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if proxyErrCh != nil {
				proxyErr := <-proxyErrCh
				proxyWaited = true
				return fmt.Errorf("sandbox proxy server: %w", proxyErr)
			}
			return errCtx
		}

		if status.Code(err) == codes.FailedPrecondition {
			logging.Warnf("registry session rejected for node_id=%s: %v", cfg.WorkerID, err)
			reconnectDelay = initialReconnectDelay
		} else {
			logging.Warnf("registry session interrupted: %v", err)
		}

		if err := waitReconnect(runCtx, reconnectDelay); err != nil {
			return err
		}
		reconnectDelay = nextReconnectDelay(reconnectDelay)
	}
}

func resolveDockerNetworks(cfg config.Config) (terminal string, python string) {
	configured := strings.TrimSpace(cfg.DockerNetwork)
	if configured != "" {
		return configured, configured
	}
	if cfg.ProxyEnabled {
		return terminalProxyDockerNetwork, ""
	}
	return "", ""
}

func validateTerminalMaxActiveSessions(maxActiveSessions int) error {
	if maxActiveSessions < 0 || int64(maxActiveSessions) > maxProtocolInt32 {
		return errors.New("WORKER_TERMINAL_MAX_ACTIVE_SESSIONS must be between 0 and 2147483647")
	}
	return nil
}
