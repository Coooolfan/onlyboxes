package runner

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/internal/logging"
	"github.com/onlyboxes/onlyboxes/worker/internal/sessionclient"
	"github.com/onlyboxes/onlyboxes/worker/worker-sys/internal/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	minHeartbeatInterval          = 1 * time.Second
	initialReconnectDelay         = 1 * time.Second
	maxReconnectDelay             = 15 * time.Second
	computerUseCapabilityName     = "computeruse"
	computerUseCapabilityDeclared = "computerUse"
	readImageCapabilityName       = "readimage"
	readImageCapabilityDeclared   = "readImage"
)

var waitReconnect = waitReconnectDelay
var applyJitter = jitterDuration

func Run(ctx context.Context, cfg config.Config) error {
	if strings.TrimSpace(cfg.WorkerID) == "" {
		return errors.New("WORKER_ID is required")
	}
	if strings.TrimSpace(cfg.WorkerSecret) == "" {
		return errors.New("WORKER_SECRET is required")
	}

	executor := newComputerUseExecutor(computerUseExecutorConfig{
		OutputLimitBytes: cfg.ComputerUseOutputLimitByte,
		WhitelistMode:    cfg.ComputerUseWhitelistMode,
		Whitelist:        cfg.ComputerUseWhitelist,
	})
	originalRunComputerUse := runComputerUse
	runComputerUse = executor.Execute
	originalRunReadImage := runReadImage
	runReadImage = newReadImageExecutor(cfg.ReadImageAllowedPaths).Execute
	defer func() {
		runComputerUse = originalRunComputerUse
		runReadImage = originalRunReadImage
	}()
	logging.Infof(
		"computerUse whitelist configured: mode=%s count=%d",
		cfg.ComputerUseWhitelistMode,
		len(cfg.ComputerUseWhitelist),
	)
	logging.Infof(
		"readImage allowed paths configured: count=%d",
		len(cfg.ReadImageAllowedPaths),
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

func nextReconnectDelay(current time.Duration) time.Duration {
	return sessionclient.NextReconnectDelay(current, initialReconnectDelay, maxReconnectDelay)
}

func waitReconnectDelay(ctx context.Context, delay time.Duration) error {
	return sessionclient.WaitReconnectDelay(ctx, delay, initialReconnectDelay)
}
