package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

const (
	terminalResourceCapabilityName     = "terminalresource"
	terminalResourceCapabilityDeclared = "terminalResource"
	terminalResourceActionValidate     = "validate"
	terminalResourceActionRead         = "read"
	terminalResourceActionExport       = "export"
	terminalResourceCodeFileNotFound   = "file_not_found"
	terminalResourceCodePathIsDir      = "path_is_directory"
	terminalResourceCodeFileTooLarge   = "file_too_large"
)

type terminalResourcePayload struct {
	SessionID string `json:"session_id"`
	FilePath  string `json:"file_path"`
	Action    string `json:"action,omitempty"`
	SignedURL string `json:"signed_url,omitempty"`
}

type terminalResourceRequest struct {
	SessionID string
	FilePath  string
	Action    string
	SignedURL string
}

type terminalResourceRunResult struct {
	SessionID string `json:"session_id"`
	FilePath  string `json:"file_path"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	Blob      []byte `json:"blob,omitempty"`
}

type terminalResourceProbeResult struct {
	Error    string `json:"error,omitempty"`
	Message  string `json:"message,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Size     int64  `json:"size_bytes"`
	Blob     string `json:"blob,omitempty"`
}

var httpPutFile = putFileToSignedURL

const terminalResourceProbeScript = `
import argparse
import base64
import json
import mimetypes
import os
import sys

parser = argparse.ArgumentParser()
parser.add_argument("--action", choices=["validate", "read"], default="validate")
parser.add_argument("--file-path", required=True)
parser.add_argument("--max-read-bytes", type=int, required=True)
args = parser.parse_args()

target = args.file_path
if not os.path.exists(target):
    print(json.dumps({"error": "file_not_found", "message": "file not found"}))
    sys.exit(10)
if os.path.isdir(target):
    print(json.dumps({"error": "path_is_directory", "message": "path is directory"}))
    sys.exit(11)

size_bytes = os.path.getsize(target)
mime_type, _ = mimetypes.guess_type(target)
if not mime_type:
    mime_type = "application/octet-stream"

if args.action == "validate":
    print(json.dumps({"mime_type": mime_type, "size_bytes": size_bytes}))
    sys.exit(0)

limit = args.max_read_bytes
if size_bytes > limit:
    print(json.dumps({
        "error": "file_too_large",
        "message": "file exceeds read limit",
        "mime_type": mime_type,
        "size_bytes": size_bytes,
    }))
    sys.exit(12)

with open(target, "rb") as fh:
    content = fh.read(limit + 1)
if len(content) > limit:
    print(json.dumps({
        "error": "file_too_large",
        "message": "file exceeds read limit",
        "mime_type": mime_type,
        "size_bytes": len(content),
    }))
    sys.exit(12)

print(json.dumps({
    "mime_type": mime_type,
    "size_bytes": len(content),
    "blob": base64.b64encode(content).decode("ascii"),
}))
`

func (m *terminalSessionManager) ResolveResource(ctx context.Context, req terminalResourceRequest) (terminalResourceRunResult, error) {
	if m == nil {
		return terminalResourceRunResult{}, newTerminalExecError("execution_failed", terminalExecNotReadyMessage)
	}

	sessionID := strings.TrimSpace(req.SessionID)
	filePath := strings.TrimSpace(req.FilePath)
	if sessionID == "" || filePath == "" {
		return terminalResourceRunResult{}, newTerminalExecError(terminalExecCodeInvalidPayload, "session_id and file_path are required")
	}

	action := normalizeTerminalResourceAction(req.Action)
	if action == "" {
		return terminalResourceRunResult{}, newTerminalExecError(terminalExecCodeInvalidPayload, "action must be validate, read, or export")
	}
	signedURL := strings.TrimSpace(req.SignedURL)
	if action == terminalResourceActionExport && signedURL == "" {
		return terminalResourceRunResult{}, newTerminalExecError(terminalExecCodeInvalidPayload, "signed_url is required for export")
	}

	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	if !ok || session == nil {
		m.mu.Unlock()
		return terminalResourceRunResult{}, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}
	if session.busy {
		m.mu.Unlock()
		return terminalResourceRunResult{}, newTerminalExecError(terminalExecCodeSessionBusy, terminalExecBusyMessage)
	}
	session.busy = true
	containerName := session.containerName
	m.mu.Unlock()

	resourceResult, err := m.resolveResourceInSession(ctx, sessionID, containerName, filePath, action, signedURL)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			m.destroySession(sessionID)
			return terminalResourceRunResult{}, err
		}
		var terminalErr *terminalExecError
		if errors.As(err, &terminalErr) && terminalErr.Code() == terminalExecCodeSessionNotFound {
			m.destroySession(sessionID)
			return terminalResourceRunResult{}, err
		}
		m.markSessionIdle(sessionID)
		return terminalResourceRunResult{}, err
	}

	if _, ok := m.markSessionIdle(sessionID); !ok {
		return terminalResourceRunResult{}, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}
	return resourceResult, nil
}

func (m *terminalSessionManager) resolveResourceInSession(
	ctx context.Context,
	sessionID string,
	containerName string,
	filePath string,
	action string,
	signedURL string,
) (terminalResourceRunResult, error) {
	probeAction := action
	if action == terminalResourceActionExport {
		probeAction = terminalResourceActionValidate
	}

	probe, err := probeTerminalResource(ctx, containerName, filePath, probeAction, m.outputLimitBytes)
	if err != nil {
		return terminalResourceRunResult{}, err
	}

	result := buildTerminalResourceResult(sessionID, filePath, probe)
	if action == terminalResourceActionExport {
		if err := exportTerminalResource(ctx, containerName, filePath, signedURL); err != nil {
			return terminalResourceRunResult{}, err
		}
		return result, nil
	}
	if action != terminalResourceActionRead {
		return result, nil
	}

	blobValue := strings.TrimSpace(probe.Blob)
	if blobValue == "" {
		result.Blob = []byte{}
		return result, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(blobValue)
	if err != nil {
		return terminalResourceRunResult{}, fmt.Errorf("decode resource blob: %w", err)
	}
	result.Blob = decoded
	return result, nil
}

func probeTerminalResource(
	ctx context.Context,
	containerName string,
	filePath string,
	action string,
	maxReadBytes int,
) (terminalResourceProbeResult, error) {
	execResult := runDockerCommand(ctx, terminalExecDockerResourceArgs(containerName, action, filePath, maxReadBytes)...)
	if execResult.Err != nil {
		return terminalResourceProbeResult{}, fmt.Errorf("docker exec failed: %w", execResult.Err)
	}
	if isNoSuchContainerMessage(execResult.Stderr) {
		return terminalResourceProbeResult{}, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}

	if execResult.ExitCode != 0 {
		if probe, ok := tryDecodeTerminalResourceProbeOutput(execResult.Stdout); ok {
			if code := strings.TrimSpace(probe.Error); code != "" {
				message := terminalResourceErrorMessage(code, probe.Message)
				return terminalResourceProbeResult{}, newTerminalExecError(code, message)
			}
		}
		return terminalResourceProbeResult{}, fmt.Errorf(
			"docker exec failed: %s",
			terminalResourceDockerFailureMessage("exit code", execResult.ExitCode, execResult.Stderr, execResult.Stdout),
		)
	}

	probe, err := decodeTerminalResourceProbeOutput(execResult.Stdout)
	if err != nil {
		return terminalResourceProbeResult{}, fmt.Errorf("invalid terminalResource result: %w", err)
	}
	if code := strings.TrimSpace(probe.Error); code != "" {
		message := terminalResourceErrorMessage(code, probe.Message)
		return terminalResourceProbeResult{}, newTerminalExecError(code, message)
	}
	return probe, nil
}

func exportTerminalResource(ctx context.Context, containerName string, filePath string, signedURL string) error {
	tempFile, err := os.CreateTemp("", "onlyboxes-export-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	if closeErr := tempFile.Close(); closeErr != nil {
		os.Remove(tempPath)
		return fmt.Errorf("close temp file: %w", closeErr)
	}
	defer os.Remove(tempPath)

	copyResult := runDockerCommand(ctx, terminalExecDockerCopyArgs(containerName, filePath, tempPath)...)
	if copyResult.Err != nil {
		return fmt.Errorf("docker cp failed: %w", copyResult.Err)
	}
	if isNoSuchContainerMessage(copyResult.Stderr) {
		return newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}
	if copyResult.ExitCode != 0 {
		return fmt.Errorf(
			"docker cp failed: %s",
			dockerCommandFailureMessage("exit code", copyResult.ExitCode, copyResult.Stderr),
		)
	}
	if err := httpPutFile(ctx, signedURL, tempPath); err != nil {
		return err
	}
	return nil
}

func putFileToSignedURL(ctx context.Context, signedURL string, filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open export file: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat export file: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, signedURL, file)
	if err != nil {
		return fmt.Errorf("build upload request: %w", err)
	}
	request.ContentLength = stat.Size()

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("upload export file: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = response.Status
		}
		return fmt.Errorf("upload export file failed: %s", message)
	}
	return nil
}

func buildTerminalResourceResult(sessionID string, filePath string, probe terminalResourceProbeResult) terminalResourceRunResult {
	mimeType := strings.TrimSpace(probe.MIMEType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return terminalResourceRunResult{
		SessionID: sessionID,
		FilePath:  filePath,
		MIMEType:  mimeType,
		SizeBytes: probe.Size,
	}
}

func terminalExecDockerResourceArgs(containerName string, action string, filePath string, maxReadBytes int) []string {
	limit := maxReadBytes
	if limit <= 0 {
		limit = 1
	}
	return []string{
		"exec",
		containerName,
		"python3",
		"-c",
		terminalResourceProbeScript,
		"--action",
		action,
		"--file-path",
		filePath,
		"--max-read-bytes",
		strconv.Itoa(limit),
	}
}

func terminalExecDockerCopyArgs(containerName string, filePath string, targetPath string) []string {
	return []string{
		"cp",
		containerName + ":" + filePath,
		targetPath,
	}
}

func normalizeTerminalResourceAction(action string) string {
	switch strings.TrimSpace(strings.ToLower(action)) {
	case "":
		return terminalResourceActionValidate
	case terminalResourceActionValidate:
		return terminalResourceActionValidate
	case terminalResourceActionRead:
		return terminalResourceActionRead
	case terminalResourceActionExport:
		return terminalResourceActionExport
	default:
		return ""
	}
}

func decodeTerminalResourceProbeOutput(stdout string) (terminalResourceProbeResult, error) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return terminalResourceProbeResult{}, errors.New("empty output")
	}
	decoded := terminalResourceProbeResult{}
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return terminalResourceProbeResult{}, err
	}
	return decoded, nil
}

func tryDecodeTerminalResourceProbeOutput(stdout string) (terminalResourceProbeResult, bool) {
	decoded, err := decodeTerminalResourceProbeOutput(stdout)
	if err != nil {
		return terminalResourceProbeResult{}, false
	}
	return decoded, true
}

func terminalResourceDockerFailureMessage(prefix string, value int, stderr string, stdout string) string {
	message := dockerCommandFailureMessage(prefix, value, stderr)
	if strings.TrimSpace(stderr) != "" {
		return message
	}

	trimmedStdout := strings.TrimSpace(stdout)
	if trimmedStdout == "" {
		return message
	}
	if len(trimmedStdout) > 256 {
		trimmedStdout = trimmedStdout[:256] + "..."
	}
	return message + ", stdout=" + trimmedStdout
}

func terminalResourceErrorMessage(code string, fallback string) string {
	if trimmed := strings.TrimSpace(fallback); trimmed != "" {
		return trimmed
	}
	switch strings.TrimSpace(code) {
	case terminalResourceCodeFileNotFound:
		return "file not found"
	case terminalResourceCodePathIsDir:
		return "path is directory"
	case terminalResourceCodeFileTooLarge:
		return "file exceeds read limit"
	default:
		return "terminal resource operation failed"
	}
}
