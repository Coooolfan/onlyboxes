package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
)

type pythonExecRunner struct {
	dockerImage string
}

func newPythonExecRunner(dockerImage string) *pythonExecRunner {
	return &pythonExecRunner{
		dockerImage: dockerImage,
	}
}

func (r *pythonExecRunner) Execute(ctx context.Context, code string) (pythonExecRunResult, error) {
	if r == nil {
		return runPythonExecInDockerWithImage(ctx, "", code)
	}
	return runPythonExecInDockerWithImage(ctx, r.dockerImage, code)
}

type dockerCommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

type dockerContainerState struct {
	Status   string
	ExitCode int
}

func runPythonExecInDockerWithImage(ctx context.Context, dockerImage string, code string) (pythonExecRunResult, error) {
	containerName, err := pythonExecContainerNameFn()
	if err != nil {
		return pythonExecRunResult{}, fmt.Errorf("allocate pythonExec container name: %w", err)
	}

	createResult := runDockerCommand(ctx, pythonExecDockerCreateArgsWithImage(containerName, dockerImage, code)...)
	if createResult.Err != nil {
		return pythonExecRunResult{}, fmt.Errorf("docker create failed: %w", createResult.Err)
	}
	if createResult.ExitCode != 0 {
		return pythonExecRunResult{}, fmt.Errorf("docker create failed: %s", dockerCommandFailureMessage("exit code", createResult.ExitCode, createResult.Stderr))
	}

	defer cleanupPythonExecContainer(containerName)

	startResult := runDockerCommand(ctx, pythonExecDockerStartArgs(containerName)...)
	if startResult.Err != nil {
		if errors.Is(startResult.Err, context.DeadlineExceeded) || errors.Is(startResult.Err, context.Canceled) {
			return pythonExecRunResult{}, startResult.Err
		}
		return pythonExecRunResult{}, fmt.Errorf("docker start failed: %w", startResult.Err)
	}

	if startResult.ExitCode == 0 {
		return pythonExecRunResult{
			Output:   startResult.Stdout,
			Stderr:   startResult.Stderr,
			ExitCode: 0,
		}, nil
	}

	state, stateErr := inspectPythonExecContainerState(containerName)
	if stateErr != nil {
		return pythonExecRunResult{}, fmt.Errorf("docker start failed with exit code %d: inspect failed: %w", startResult.ExitCode, stateErr)
	}
	if !isTerminalPythonExecContainerState(state.Status) {
		return pythonExecRunResult{}, fmt.Errorf(
			"docker start failed: state=%s %s",
			state.Status,
			dockerCommandFailureMessage("exit code", startResult.ExitCode, startResult.Stderr),
		)
	}

	return pythonExecRunResult{
		Output:   startResult.Stdout,
		Stderr:   startResult.Stderr,
		ExitCode: state.ExitCode,
	}, nil
}

func runDockerCommandCLI(ctx context.Context, args ...string) dockerCommandResult {
	command := exec.CommandContext(ctx, "docker", args...)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	err := command.Run()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return dockerCommandResult{
				Stdout: stdout.String(),
				Stderr: stderr.String(),
				Err:    err,
			}
		}

		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return dockerCommandResult{
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				ExitCode: exitErr.ExitCode(),
			}
		}

		return dockerCommandResult{
			Stdout: stdout.String(),
			Stderr: stderr.String(),
			Err:    err,
		}
	}

	return dockerCommandResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
	}
}

func pythonExecDockerCreateArgs(containerName string, code string) []string {
	return pythonExecDockerCreateArgsWithImage(containerName, defaultPythonExecDockerImage, code)
}

func pythonExecDockerCreateArgsWithImage(containerName string, dockerImage string, code string) []string {
	resolvedDockerImage := strings.TrimSpace(dockerImage)
	if resolvedDockerImage == "" {
		resolvedDockerImage = defaultPythonExecDockerImage
	}

	return []string{
		"create",
		"--name", containerName,
		"--label", pythonExecManagedLabel,
		"--label", pythonExecCapabilityLabel,
		"--label", pythonExecRuntimeLabel,
		"--memory", defaultPythonExecMemoryLimit,
		"--cpus", defaultPythonExecCPULimit,
		"--pids-limit", strconv.Itoa(defaultPythonExecPidsLimit),
		resolvedDockerImage,
		"python",
		"-c",
		code,
	}
}

func pythonExecDockerStartArgs(containerName string) []string {
	return []string{"start", "-a", containerName}
}

func pythonExecDockerInspectArgs(containerName string) []string {
	return []string{
		"inspect",
		"-f",
		"{{.State.Status}}|{{.State.ExitCode}}",
		containerName,
	}
}

func pythonExecDockerRemoveArgs(containerName string) []string {
	return []string{"rm", "-f", containerName}
}

func inspectPythonExecContainerState(containerName string) (dockerContainerState, error) {
	inspectCtx, cancel := context.WithTimeout(context.Background(), pythonExecInspectTimeout)
	defer cancel()

	result := runDockerCommand(inspectCtx, pythonExecDockerInspectArgs(containerName)...)
	if result.Err != nil {
		return dockerContainerState{}, result.Err
	}
	if result.ExitCode != 0 {
		return dockerContainerState{}, errors.New(dockerCommandFailureMessage("exit code", result.ExitCode, result.Stderr))
	}

	parts := strings.Split(strings.TrimSpace(result.Stdout), "|")
	if len(parts) != 2 {
		return dockerContainerState{}, fmt.Errorf("unexpected docker inspect output: %q", strings.TrimSpace(result.Stdout))
	}

	exitCode, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return dockerContainerState{}, fmt.Errorf("invalid container exit code: %w", err)
	}

	return dockerContainerState{
		Status:   strings.TrimSpace(parts[0]),
		ExitCode: exitCode,
	}, nil
}

func cleanupPythonExecContainer(containerName string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), pythonExecCleanupTimeout)
	defer cancel()

	result := runDockerCommand(cleanupCtx, pythonExecDockerRemoveArgs(containerName)...)
	if result.Err != nil {
		log.Printf("pythonExec cleanup failed: container=%s err=%v", containerName, result.Err)
		return
	}
	if result.ExitCode != 0 && !isNoSuchContainerMessage(result.Stderr) {
		log.Printf(
			"pythonExec cleanup failed: container=%s %s",
			containerName,
			dockerCommandFailureMessage("exit code", result.ExitCode, result.Stderr),
		)
	}
}

func newPythonExecContainerName() (string, error) {
	suffix, err := randomHex(8)
	if err != nil {
		return "", err
	}
	return pythonExecContainerPrefix + suffix, nil
}

func dockerCommandFailureMessage(prefix string, value int, stderr string) string {
	message := fmt.Sprintf("%s=%d", prefix, value)
	if trimmed := strings.TrimSpace(stderr); trimmed != "" {
		message = message + ", stderr=" + trimmed
	}
	return message
}

func isTerminalPythonExecContainerState(state string) bool {
	normalized := strings.TrimSpace(strings.ToLower(state))
	return normalized == "exited" || normalized == "dead"
}

func isNoSuchContainerMessage(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "no such container")
}

func randomHex(byteLength int) (string, error) {
	if byteLength <= 0 {
		return "", errors.New("byte length must be positive")
	}

	raw := make([]byte, byteLength)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
