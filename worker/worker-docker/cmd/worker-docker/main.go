package main

import (
	"context"
	"errors"
	"os/signal"
	"syscall"

	"github.com/onlyboxes/onlyboxes/worker/internal/logging"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/config"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/runner"
)

func main() {
	cfg := config.Load()
	logging.Configure(cfg.LogLevel, cfg.LogFormat, cfg.LogAddSource)
	if cfg.ConfigFile != "" {
		logging.Infof("config file loaded: %s", cfg.ConfigFile)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := runner.Run(ctx, cfg); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			logging.Infof("worker stopped: %v", err)
			return
		}
		logging.Fatalf("worker stopped with error: %v", err)
	}
}
