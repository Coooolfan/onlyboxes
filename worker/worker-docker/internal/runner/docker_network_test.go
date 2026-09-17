package runner

import (
	"context"
	"strings"
	"testing"
)

func TestEnsureTerminalProxyNetworkCreatesIsolatedBridge(t *testing.T) {
	original := runDockerCommand
	t.Cleanup(func() { runDockerCommand = original })
	inspectCalls := 0
	var createArgs []string
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		if len(args) >= 2 && args[0] == "network" && args[1] == "inspect" {
			inspectCalls++
			if inspectCalls == 1 {
				return dockerCommandResult{ExitCode: 1, Stderr: "network not found"}
			}
			return dockerCommandResult{ExitCode: 0, Stdout: "bridge|false\n"}
		}
		if len(args) >= 2 && args[0] == "network" && args[1] == "create" {
			createArgs = append([]string(nil), args...)
			return dockerCommandResult{ExitCode: 0}
		}
		return dockerCommandResult{ExitCode: 1, Stderr: "unexpected command"}
	}

	if err := ensureSandboxDockerNetwork(context.Background(), terminalProxyDockerNetwork); err != nil {
		t.Fatalf("ensure network: %v", err)
	}
	if inspectCalls != 2 {
		t.Fatalf("expected inspect before and after create, got %d", inspectCalls)
	}
	joined := strings.Join(createArgs, " ")
	if !strings.Contains(joined, "--driver bridge") ||
		!strings.Contains(joined, "com.docker.network.bridge.enable_icc=false") ||
		!strings.HasSuffix(joined, terminalProxyDockerNetwork) {
		t.Fatalf("network create did not enforce isolation: %#v", createArgs)
	}
}

func TestEnsureSandboxDockerNetworkUsesConfiguredName(t *testing.T) {
	original := runDockerCommand
	t.Cleanup(func() { runDockerCommand = original })
	inspectCalls := 0
	var createArgs []string
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		if len(args) >= 2 && args[0] == "network" && args[1] == "inspect" {
			inspectCalls++
			if got := args[len(args)-1]; got != "custom-network" {
				t.Fatalf("expected configured network name, got %q", got)
			}
			if inspectCalls == 1 {
				return dockerCommandResult{ExitCode: 1, Stderr: "network not found"}
			}
			return dockerCommandResult{ExitCode: 0, Stdout: "bridge|false\n"}
		}
		createArgs = append([]string(nil), args...)
		return dockerCommandResult{ExitCode: 0}
	}

	if err := ensureSandboxDockerNetwork(context.Background(), " custom-network "); err != nil {
		t.Fatalf("ensure configured network: %v", err)
	}
	if !strings.HasSuffix(strings.Join(createArgs, " "), "custom-network") {
		t.Fatalf("network create did not use configured name: %#v", createArgs)
	}
}

func TestEnsureTerminalProxyNetworkReusesValidBridge(t *testing.T) {
	original := runDockerCommand
	t.Cleanup(func() { runDockerCommand = original })
	createCalled := false
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		if len(args) >= 2 && args[0] == "network" && args[1] == "inspect" {
			return dockerCommandResult{Stdout: "bridge|false\n", ExitCode: 0}
		}
		createCalled = true
		return dockerCommandResult{ExitCode: 0}
	}

	if err := ensureSandboxDockerNetwork(context.Background(), terminalProxyDockerNetwork); err != nil {
		t.Fatalf("ensure network: %v", err)
	}
	if createCalled {
		t.Fatalf("valid existing network should be reused")
	}
}

func TestEnsureTerminalProxyNetworkRejectsInterContainerCommunication(t *testing.T) {
	original := runDockerCommand
	t.Cleanup(func() { runDockerCommand = original })
	runDockerCommand = func(_ context.Context, _ ...string) dockerCommandResult {
		return dockerCommandResult{Stdout: "bridge|true\n", ExitCode: 0}
	}

	err := ensureSandboxDockerNetwork(context.Background(), terminalProxyDockerNetwork)
	if err == nil || !strings.Contains(err.Error(), "disable inter-container communication") {
		t.Fatalf("expected unsafe network rejection, got %v", err)
	}
}
