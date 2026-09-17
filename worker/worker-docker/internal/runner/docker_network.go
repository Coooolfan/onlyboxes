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

func ensureSandboxDockerNetwork(ctx context.Context, network string) error {
	setupCtx, cancel := context.WithTimeout(ctx, dockerNetworkSetupTimeout)
	defer cancel()

	network = strings.TrimSpace(network)
	if exists, err := inspectSandboxDockerNetwork(setupCtx, network); err != nil {
		return err
	} else if exists {
		return nil
	}

	result := runDockerCommand(setupCtx,
		"network", "create",
		"--driver", "bridge",
		"--opt", "com.docker.network.bridge.enable_icc=false",
		network,
	)
	if result.Err != nil {
		return fmt.Errorf("create sandbox docker network: %w", result.Err)
	}
	if result.ExitCode != 0 {
		// Another worker process may have created the shared network concurrently.
		if exists, inspectErr := inspectSandboxDockerNetwork(setupCtx, network); inspectErr == nil && exists {
			return nil
		}
		return errors.New(dockerCommandFailureMessage("create sandbox docker network exit code", result.ExitCode, result.Stderr))
	}
	exists, err := inspectSandboxDockerNetwork(setupCtx, network)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("sandbox docker network was not found after creation")
	}
	return nil
}

func inspectSandboxDockerNetwork(ctx context.Context, network string) (bool, error) {
	result := runDockerCommand(ctx,
		"network", "inspect",
		"--format", `{{.Driver}}|{{index .Options "com.docker.network.bridge.enable_icc"}}`,
		strings.TrimSpace(network),
	)
	if result.Err != nil {
		return false, fmt.Errorf("inspect sandbox docker network: %w", result.Err)
	}
	if result.ExitCode != 0 {
		message := strings.ToLower(result.Stderr)
		if strings.Contains(message, "not found") || strings.Contains(message, "no such network") {
			return false, nil
		}
		return false, errors.New(dockerCommandFailureMessage("inspect sandbox docker network exit code", result.ExitCode, result.Stderr))
	}
	parts := strings.Split(strings.TrimSpace(result.Stdout), "|")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) != "bridge" {
		return false, errors.New("sandbox docker network must use the bridge driver")
	}
	if strings.TrimSpace(strings.ToLower(parts[1])) != "false" {
		return false, errors.New("sandbox docker network must disable inter-container communication")
	}
	return true, nil
}
