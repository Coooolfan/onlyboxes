package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b"
)

const (
	terminalResourceCapabilityName     = "terminalresource"
	terminalResourceCapabilityDeclared = "terminalResource"
	terminalResourceActionValidate     = "validate"
	terminalResourceActionRead         = "read"
	terminalResourceActionExport       = "export"
	terminalExportModeWorker           = "worker"
	terminalExportModeSandbox          = "sandbox"
	terminalResourceCodeFileNotFound   = "file_not_found"
	terminalResourceCodePathIsDir      = "path_is_directory"
	terminalResourceCodeFileTooLarge   = "file_too_large"
)

const sandboxExportScript = `import base64
import http.client
import json
import os
import sys
import urllib.parse

config = json.loads(base64.b64decode(sys.argv[1]))
file_path = config["file_path"]
url = urllib.parse.urlsplit(config["signed_url"])
if url.scheme not in ("http", "https") or not url.hostname:
    raise ValueError("signed_url must be an absolute HTTP or HTTPS URL")

target = url.path or "/"
if url.query:
    target += "?" + url.query
headers = dict(config.get("headers") or {})
headers["Content-Length"] = str(os.path.getsize(file_path))
connection_type = http.client.HTTPSConnection if url.scheme == "https" else http.client.HTTPConnection
connection = connection_type(url.hostname, url.port)
try:
    with open(file_path, "rb") as source:
        connection.request("PUT", target, body=source, headers=headers)
        response = connection.getresponse()
        response.read(1024)
        if response.status < 200 or response.status >= 300:
            raise RuntimeError("upload returned HTTP status %d" % response.status)
finally:
    connection.close()
`

type terminalResourcePayload struct {
	SessionID string            `json:"session_id"`
	FilePath  string            `json:"file_path"`
	Action    string            `json:"action,omitempty"`
	SignedURL string            `json:"signed_url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

type terminalResourceRequest struct {
	SessionID string
	FilePath  string
	Action    string
	SignedURL string
	Headers   map[string]string
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
}

func (m *terminalSessionManager) ResolveResource(ctx context.Context, req terminalResourceRequest) (terminalResourceRunResult, error) {
	if m == nil || m.backend == nil {
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
	if action == terminalResourceActionExport && strings.TrimSpace(req.SignedURL) == "" {
		return terminalResourceRunResult{}, newTerminalExecError(terminalExecCodeInvalidPayload, "signed_url is required for export")
	}

	session, _, err := m.claimSession(sessionID, timeZero, false)
	if err != nil {
		return terminalResourceRunResult{}, err
	}
	if err := m.awaitSessionReady(ctx, session, false); err != nil {
		return terminalResourceRunResult{}, err
	}
	result, err := m.resolveResource(ctx, session, filePath, action, req.SignedURL, req.Headers)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			m.releaseAndDestroySession(sessionID)
			return terminalResourceRunResult{}, err
		}
		if errors.Is(err, e2b.ErrSandboxNotFound) {
			m.releaseAndDestroySession(sessionID)
			return terminalResourceRunResult{}, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
		}
		m.releaseSession(sessionID)
		return terminalResourceRunResult{}, err
	}
	if _, ok := m.releaseSession(sessionID); !ok {
		return terminalResourceRunResult{}, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}
	return result, nil
}

var timeZero = time.Time{}

func (m *terminalSessionManager) resolveResource(
	ctx context.Context,
	session *terminalSession,
	filePath, action, signedURL string,
	headers map[string]string,
) (terminalResourceRunResult, error) {
	probe, err := m.probeResource(ctx, session, filePath)
	if err != nil {
		return terminalResourceRunResult{}, err
	}
	result := terminalResourceRunResult{
		SessionID: session.sessionID,
		FilePath:  filePath,
		MIMEType:  probe.MIMEType,
		SizeBytes: probe.Size,
	}
	switch action {
	case terminalResourceActionValidate:
		return result, nil
	case terminalResourceActionRead:
		file, err := m.backend.ReadFile(ctx, session.sandbox, filePath, int64(m.outputLimitBytes))
		if errors.Is(err, e2b.ErrFileNotFound) {
			return terminalResourceRunResult{}, newTerminalExecError(terminalResourceCodeFileNotFound, "file not found")
		}
		if errors.Is(err, e2b.ErrFileTooLarge) {
			return terminalResourceRunResult{}, newTerminalExecError(terminalResourceCodeFileTooLarge, "file exceeds read limit")
		}
		if err != nil {
			return terminalResourceRunResult{}, err
		}
		result.MIMEType = file.MIMEType
		result.SizeBytes = file.Size
		result.Blob = file.Content
		return result, nil
	case terminalResourceActionExport:
		if m.exportMaxBytes > 0 && probe.Size > int64(m.exportMaxBytes) {
			return terminalResourceRunResult{}, newTerminalExecError(terminalResourceCodeFileTooLarge, "file exceeds export limit")
		}
		if err := m.exportResource(ctx, session.sandbox, filePath, signedURL, headers); err != nil {
			return terminalResourceRunResult{}, err
		}
		return result, nil
	default:
		return terminalResourceRunResult{}, newTerminalExecError(terminalExecCodeInvalidPayload, "invalid action")
	}
}

func (m *terminalSessionManager) probeResource(ctx context.Context, session *terminalSession, filePath string) (terminalResourceProbeResult, error) {
	encodedPath := base64.StdEncoding.EncodeToString([]byte(filePath))
	script := `import base64,json,mimetypes,os; p=base64.b64decode("` + encodedPath + `").decode(); ` +
		`print(json.dumps(({"error":"file_not_found","message":"file not found"} if not os.path.exists(p) else ` +
		`({"error":"path_is_directory","message":"path is directory"} if os.path.isdir(p) else ` +
		`{"mime_type":mimetypes.guess_type(p)[0] or "application/octet-stream","size_bytes":os.path.getsize(p)}))))`
	command := "python3 -c '" + script + "'"
	output, err := m.backend.Run(ctx, session.sandbox, command, 64*1024)
	if err != nil {
		return terminalResourceProbeResult{}, err
	}
	if output.ExitCode != 0 {
		return terminalResourceProbeResult{}, fmt.Errorf("probe resource failed: exit_code=%d stderr=%s", output.ExitCode, strings.TrimSpace(output.Stderr))
	}
	probe := terminalResourceProbeResult{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.Stdout)), &probe); err != nil {
		return terminalResourceProbeResult{}, fmt.Errorf("decode resource metadata: %w", err)
	}
	if probe.Error != "" {
		return terminalResourceProbeResult{}, newTerminalExecError(probe.Error, terminalResourceErrorMessage(probe.Error, probe.Message))
	}
	if probe.MIMEType == "" {
		probe.MIMEType = "application/octet-stream"
	}
	return probe, nil
}

func (m *terminalSessionManager) exportResource(
	ctx context.Context,
	sandbox *e2b.Sandbox,
	filePath, signedURL string,
	headers map[string]string,
) error {
	if m.exportMode == terminalExportModeSandbox {
		return m.exportResourceFromSandbox(ctx, sandbox, filePath, signedURL, headers)
	}
	return m.exportResourceThroughWorker(ctx, sandbox, filePath, signedURL, headers)
}

func (m *terminalSessionManager) exportResourceThroughWorker(
	ctx context.Context,
	sandbox *e2b.Sandbox,
	filePath, signedURL string,
	headers map[string]string,
) error {
	source, err := m.backend.OpenFile(ctx, sandbox, filePath)
	if errors.Is(err, e2b.ErrFileNotFound) {
		return newTerminalExecError(terminalResourceCodeFileNotFound, "file not found")
	}
	if err != nil {
		return err
	}
	defer source.Body.Close()
	if m.exportMaxBytes > 0 && source.Size > int64(m.exportMaxBytes) {
		return newTerminalExecError(terminalResourceCodeFileTooLarge, "file exceeds export limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, signedURL, source.Body)
	if err != nil {
		return fmt.Errorf("build export request: %w", err)
	}
	if source.Size >= 0 {
		req.ContentLength = source.Size
	}
	for key, value := range headers {
		if key = strings.TrimSpace(key); key != "" {
			req.Header.Set(key, value)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload export file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = resp.Status
		}
		return fmt.Errorf("upload export file failed: %s", message)
	}
	return nil
}

func (m *terminalSessionManager) exportResourceFromSandbox(
	ctx context.Context,
	sandbox *e2b.Sandbox,
	filePath, signedURL string,
	headers map[string]string,
) error {
	command, err := buildSandboxExportCommand(filePath, signedURL, headers)
	if err != nil {
		return err
	}
	output, err := m.backend.Run(ctx, sandbox, command, 64*1024)
	if err != nil {
		return fmt.Errorf("run sandbox export: %w", err)
	}
	if output.ExitCode != 0 {
		message := strings.TrimSpace(output.Stderr)
		if message == "" {
			message = strings.TrimSpace(output.Stdout)
		}
		if message == "" {
			message = "upload command failed"
		}
		return fmt.Errorf("sandbox export failed: exit_code=%d: %s", output.ExitCode, message)
	}
	return nil
}

func buildSandboxExportCommand(filePath, signedURL string, headers map[string]string) (string, error) {
	payload, err := json.Marshal(struct {
		FilePath  string            `json:"file_path"`
		SignedURL string            `json:"signed_url"`
		Headers   map[string]string `json:"headers,omitempty"`
	}{
		FilePath:  filePath,
		SignedURL: signedURL,
		Headers:   headers,
	})
	if err != nil {
		return "", fmt.Errorf("encode sandbox export request: %w", err)
	}
	encodedScript := base64.StdEncoding.EncodeToString([]byte(sandboxExportScript))
	encodedPayload := base64.StdEncoding.EncodeToString(payload)
	return "python3 -c 'exec(__import__(\"base64\").b64decode(\"" + encodedScript + "\"))' " + encodedPayload, nil
}

func normalizeTerminalExportMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), terminalExportModeSandbox) {
		return terminalExportModeSandbox
	}
	return terminalExportModeWorker
}

func normalizeTerminalResourceAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "", terminalResourceActionValidate:
		return terminalResourceActionValidate
	case terminalResourceActionRead:
		return terminalResourceActionRead
	case terminalResourceActionExport:
		return terminalResourceActionExport
	default:
		return ""
	}
}

func terminalResourceErrorMessage(code, fallback string) string {
	if fallback = strings.TrimSpace(fallback); fallback != "" {
		return fallback
	}
	switch code {
	case terminalResourceCodeFileNotFound:
		return "file not found"
	case terminalResourceCodePathIsDir:
		return "path is directory"
	case terminalResourceCodeFileTooLarge:
		return "file exceeds limit"
	default:
		return "terminal resource operation failed"
	}
}
