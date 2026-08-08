package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	terminalProxyDockerNetwork = "onlyboxes-sandbox"
	dockerNetworkSetupTimeout  = 10 * time.Second
)

func ensureTerminalProxyNetwork(ctx context.Context) error {
	setupCtx, cancel := context.WithTimeout(ctx, dockerNetworkSetupTimeout)
	defer cancel()

	if exists, err := inspectTerminalProxyNetwork(setupCtx); err != nil {
		return err
	} else if exists {
		return nil
	}

	result := runDockerCommand(setupCtx,
		"network", "create",
		"--driver", "bridge",
		"--opt", "com.docker.network.bridge.enable_icc=false",
		terminalProxyDockerNetwork,
	)
	if result.Err != nil {
		return fmt.Errorf("create proxy docker network: %w", result.Err)
	}
	if result.ExitCode != 0 {
		// Another worker process may have created the shared network concurrently.
		if exists, inspectErr := inspectTerminalProxyNetwork(setupCtx); inspectErr == nil && exists {
			return nil
		}
		return errors.New(dockerCommandFailureMessage("create proxy docker network exit code", result.ExitCode, result.Stderr))
	}
	exists, err := inspectTerminalProxyNetwork(setupCtx)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("proxy docker network was not found after creation")
	}
	return nil
}

func inspectTerminalProxyNetwork(ctx context.Context) (bool, error) {
	result := runDockerCommand(ctx,
		"network", "inspect",
		"--format", `{{.Driver}}|{{index .Options "com.docker.network.bridge.enable_icc"}}`,
		terminalProxyDockerNetwork,
	)
	if result.Err != nil {
		return false, fmt.Errorf("inspect proxy docker network: %w", result.Err)
	}
	if result.ExitCode != 0 {
		message := strings.ToLower(result.Stderr)
		if strings.Contains(message, "not found") || strings.Contains(message, "no such network") {
			return false, nil
		}
		return false, errors.New(dockerCommandFailureMessage("inspect proxy docker network exit code", result.ExitCode, result.Stderr))
	}
	parts := strings.Split(strings.TrimSpace(result.Stdout), "|")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) != "bridge" {
		return false, errors.New("proxy docker network must use the bridge driver")
	}
	if strings.TrimSpace(strings.ToLower(parts[1])) != "false" {
		return false, errors.New("proxy docker network must disable inter-container communication")
	}
	return true, nil
}
