package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	computerUseNotReadyMessage       = "computerUse executor is unavailable"
	computerUseCodeInvalidPayload    = "invalid_payload"
	computerUseCodeCommandNotAllowed = "command_not_allowed"
	computerUseWhitelistModePrefix   = "prefix"
	computerUseWhitelistModeExact    = "exact"
	computerUseWhitelistModeAllowAll = "allow_all"

	// computerUseShellWaitDelay bounds how long exec.Cmd will block on stdout/stderr
	// I/O after the command's process exits or the context is canceled. Without this
	// guard, a grandchild process that inherited the pipes (e.g. via `setsid` or
	// `cmd & wait`) can keep `Run()` blocked indefinitely, which would hold the
	// session's single command slot and surface as `session_busy` to the registry.
	computerUseShellWaitDelay = 5 * time.Second
)

type computerUsePayload struct {
	Command string `json:"command"`
}

type computerUseRequest struct {
	Command string
}

type computerUseRunResult struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
}

type computerUseError struct {
	code    string
	message string
}

func (e *computerUseError) Error() string {
	if e == nil {
		return "computerUse execution failed"
	}
	return e.message
}

func (e *computerUseError) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func newComputerUseError(code string, message string) *computerUseError {
	return &computerUseError{
		code:    strings.TrimSpace(code),
		message: strings.TrimSpace(message),
	}
}

type computerUseExecutorConfig struct {
	OutputLimitBytes int
	WhitelistMode    string
	Whitelist        []string
}

type computerUseExecutor struct {
	outputLimitBytes int
	whitelistMode    string
	whitelist        []string
}

func newComputerUseExecutor(cfg computerUseExecutorConfig) *computerUseExecutor {
	outputLimit := cfg.OutputLimitBytes
	if outputLimit <= 0 {
		outputLimit = 1024 * 1024
	}

	return &computerUseExecutor{
		outputLimitBytes: outputLimit,
		whitelistMode:    cfg.WhitelistMode,
		whitelist:        cfg.Whitelist,
	}
}

func (e *computerUseExecutor) Execute(ctx context.Context, req computerUseRequest) (computerUseRunResult, error) {
	if e == nil {
		return computerUseRunResult{}, newComputerUseError("execution_failed", computerUseNotReadyMessage)
	}

	command := strings.TrimSpace(req.Command)
	if command == "" {
		return computerUseRunResult{}, newComputerUseError(computerUseCodeInvalidPayload, "command is required")
	}
	if !e.isCommandAllowed(command) {
		return computerUseRunResult{}, newComputerUseError(computerUseCodeCommandNotAllowed, "command is blocked by whitelist policy")
	}

	execCmd := buildShellCommand(ctx, command)
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	execCmd.Stdout = &stdoutBuf
	execCmd.Stderr = &stderrBuf

	err := execCmd.Run()
	exitCode := 0
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return computerUseRunResult{}, err
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return computerUseRunResult{}, fmt.Errorf("shell execution failed: %w", err)
		}
	} else if execCmd.ProcessState != nil {
		exitCode = execCmd.ProcessState.ExitCode()
	}

	stdout, stdoutTruncated := truncateByBytes(stdoutBuf.String(), e.outputLimitBytes)
	stderr, stderrTruncated := truncateByBytes(stderrBuf.String(), e.outputLimitBytes)

	return computerUseRunResult{
		Stdout:          stdout,
		Stderr:          stderr,
		ExitCode:        exitCode,
		StdoutTruncated: stdoutTruncated,
		StderrTruncated: stderrTruncated,
	}, nil
}

func buildShellCommand(ctx context.Context, command string) *exec.Cmd {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		wrapped := "[Console]::OutputEncoding=[Text.Encoding]::UTF8; $OutputEncoding=[Text.Encoding]::UTF8; " + command
		cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", wrapped)
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-lc", command)
	}
	cmd.WaitDelay = computerUseShellWaitDelay
	configureProcessIsolation(cmd)
	return cmd
}

func truncateByBytes(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 {
		return value, false
	}
	if len(value) <= maxBytes {
		return value, false
	}
	return value[:maxBytes], true
}

func (e *computerUseExecutor) isCommandAllowed(command string) bool {
	if e == nil {
		return false
	}

	switch e.whitelistMode {
	case computerUseWhitelistModeAllowAll:
		return true
	case computerUseWhitelistModePrefix:
		for _, entry := range e.whitelist {
			if strings.HasPrefix(command, entry) {
				return true
			}
		}
		return false
	default:
		for _, entry := range e.whitelist {
			if command == entry {
				return true
			}
		}
		return false
	}
}
