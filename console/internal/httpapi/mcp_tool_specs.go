package httpapi

import (
	"log/slog"
	"regexp"
)

var mcpToolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

const (
	mcpServerName                  = "onlyboxes"
	pythonExecCapabilityName       = "pythonExec"
	terminalExecCapabilityName     = "terminalExec"
	terminalResourceCapabilityName = "terminalResource"
	computerUseCapabilityName      = "computerUse"
	readImageCapabilityName        = "readImage"
	exportFileToolName             = "exportFile"
	computerUseSessionID           = "computerUse"
	defaultMCPEchoTimeoutMS        = defaultEchoTimeoutMS
	minMCPTaskTimeoutMS            = 1
	defaultMCPTaskTimeoutMS        = defaultTaskTimeoutMS
	maxMCPTaskTimeoutMS            = maxTaskTimeoutMS
	minMCPTerminalLeaseSec         = 1
	maxMCPTerminalLeaseSec         = 86400
	mcpEchoToolTitle               = "Echo Message"
	mcpPythonExecToolTitle         = "Python Execute"
	mcpTerminalExecToolTitle       = "Terminal Execute"
	mcpComputerUseToolTitle        = "Computer Use"
	mcpReadImageToolTitle          = "Read Image"
	mcpExportFileToolTitle         = "Export File"

	ExportReturnSchemaAll       = "ALL"
	ExportReturnSchemaSignedURL = "SIGNED_URL"
	ExportReturnSchemaObjectKey = "OBJECTKEY"
)

var mcpServerVersion = consoleVersion()

type mcpEchoToolInput struct {
	Message   string `json:"message"`
	TimeoutMS *int   `json:"timeout_ms,omitempty"`
}

type mcpEchoToolOutput struct {
	Message string `json:"message"`
}

type mcpPythonExecToolInput struct {
	Code      string `json:"code"`
	TimeoutMS *int   `json:"timeout_ms,omitempty"`
}

type mcpPythonExecToolOutput struct {
	Output   string `json:"output"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

type mcpTerminalExecToolInput struct {
	Command         string `json:"command"`
	SessionID       string `json:"session_id,omitempty"`
	CreateIfMissing bool   `json:"create_if_missing,omitempty"`
	LeaseTTLSec     *int   `json:"lease_ttl_sec,omitempty"`
	TimeoutMS       *int   `json:"timeout_ms,omitempty"`
}

type mcpTerminalExecToolOutput struct {
	SessionID          string `json:"session_id"`
	Created            bool   `json:"created"`
	Stdout             string `json:"stdout"`
	Stderr             string `json:"stderr"`
	ExitCode           int    `json:"exit_code"`
	StdoutTruncated    bool   `json:"stdout_truncated"`
	StderrTruncated    bool   `json:"stderr_truncated"`
	LeaseExpiresUnixMS int64  `json:"lease_expires_unix_ms"`
}

type mcpComputerUseToolInput struct {
	Command   string `json:"command"`
	TimeoutMS *int   `json:"timeout_ms,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

type mcpComputerUseToolOutput struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
}

type mcpReadImageToolInput struct {
	SessionID string `json:"session_id"`
	FilePath  string `json:"file_path"`
	TimeoutMS *int   `json:"timeout_ms,omitempty"`
}

type mcpExportFileToolInput struct {
	SessionID string `json:"session_id"`
	FilePath  string `json:"file_path"`
	TimeoutMS *int   `json:"timeout_ms,omitempty"`
}

type mcpExportFileToolOutput struct {
	SignedURL string `json:"signed_url,omitempty"`
	ObjectKey string `json:"object_key,omitempty"`
	FileName  string `json:"filename,omitempty"`
}

type pythonExecPayload struct {
	Code string `json:"code"`
}

var mcpEchoToolDescription = "Echoes the input message exactly as returned by an online worker. Use this tool for connectivity checks, request tracing, and latency baselines. Do not use it for code execution, file operations, or long-running work. timeout_ms is an end-to-end dispatch timeout in milliseconds (1-60000, default 5000)."

var mcpPythonExecToolDescription = "Executes Python code in an ephemeral container and returns stdout, stderr, and exit_code. Each invocation runs in a fresh, isolated environment that is destroyed immediately upon completion — no filesystem state, installed packages, or variables persist across calls. If you need to retain files or share state between steps, use terminalExec instead. Third-party packages are NOT available by default and cannot be installed via pip at runtime. The only way to use third-party dependencies is PEP 723 inline script metadata: add a '# /// script' block at the top of your code to declare dependencies (e.g. '# dependencies = [\"requests\"]') — they will be automatically installed before execution. Do not use it for long-running jobs. timeout_ms is a synchronous execution timeout in milliseconds (1-600000, default 60000). A non-zero exit_code is returned as normal tool output, not as a protocol error."

var mcpTerminalExecToolDescription = "Executes shell commands in a container-backed terminal session. Sessions are ephemeral by default but can be made persistent by reusing session_id across calls. Commands are executed with sh -lc, and common tools are preinstalled (python3/pip/venv, git, curl/wget, jq, ripgrep, fd-find, tree, file, zip/unzip, sqlite3, agent-browser). For headless browser automation, run `agent-browser --help` for usage. Omitting session_id creates a new session per call with no state carried over — equivalent to a one-shot execution. To retain filesystem state across calls, supply the same session_id on every call. create_if_missing controls what happens when the given session_id does not exist on the worker: false (default) returns a session_not_found error; true creates the session instead. lease_ttl_sec extends session lease within configured bounds. timeout_ms is a synchronous execution timeout in milliseconds (1-600000, default 60000)."

var mcpComputerUseToolDescription = "Executes shell commands directly on the caller-owned worker-sys host OS via /bin/sh -lc. Unlike terminalExec, this tool runs on the bare host without container isolation and is stateless — each invocation is independent with no session persistence. Only one command runs at a time (single concurrency). This tool is account-scoped and requires a user-created worker-sys. timeout_ms is a synchronous execution timeout in milliseconds (1-600000, default 60000). request_id provides idempotency for retries."

var mcpReadImageToolDescription = "Reads a file and returns it as inline image content when mime type is image/*. For unsupported mime types, returns a text explanation. When session_id is exactly \"computerUse\", routing uses the caller-owned worker-sys (readImage); otherwise routing uses terminalResource for terminal sessions."

var mcpExportFileToolDescription = "Exports a file from a session to the configured S3-compatible object store and returns a presigned download URL, object key, and filename. Pass the session_id returned by terminalExec to export from a terminal session, or the exact value \"computerUse\" to export from the caller-owned worker-sys host. timeout_ms is a synchronous execution timeout in milliseconds (1-600000, default 60000)."

var mcpEchoInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"message"},
	"properties": map[string]any{
		"message": map[string]any{
			"type":        "string",
			"description": "Message to be echoed back unchanged. Empty or whitespace-only values are rejected.",
		},
		"timeout_ms": map[string]any{
			"type":        "integer",
			"description": "Optional end-to-end dispatch timeout in milliseconds.",
			"minimum":     minEchoTimeoutMS,
			"maximum":     maxEchoTimeoutMS,
			"default":     defaultMCPEchoTimeoutMS,
		},
	},
}

var mcpEchoOutputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"message"},
	"properties": map[string]any{
		"message": map[string]any{
			"type":        "string",
			"description": "Echoed message returned by the worker.",
		},
	},
}

var mcpPythonExecInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"code"},
	"properties": map[string]any{
		"code": map[string]any{
			"type":        "string",
			"description": "Python source code to execute in the worker sandbox. Third-party packages cannot be installed at runtime; to use them you must declare dependencies via PEP 723 inline script metadata (a '# /// script' block at the top of the file) — this is the only supported mechanism. Empty or whitespace-only values are rejected.",
		},
		"timeout_ms": map[string]any{
			"type":        "integer",
			"description": "Optional synchronous execution timeout in milliseconds for this tool call.",
			"minimum":     minMCPTaskTimeoutMS,
			"maximum":     maxMCPTaskTimeoutMS,
			"default":     defaultMCPTaskTimeoutMS,
		},
	},
}

var mcpPythonExecOutputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"output", "stderr", "exit_code"},
	"properties": map[string]any{
		"output": map[string]any{
			"type":        "string",
			"description": "Captured stdout from Python execution.",
		},
		"stderr": map[string]any{
			"type":        "string",
			"description": "Captured stderr from Python execution.",
		},
		"exit_code": map[string]any{
			"type":        "integer",
			"description": "Process exit code from Python execution. Non-zero is reported as normal tool output.",
		},
	},
}

var mcpTerminalExecInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"command"},
	"properties": map[string]any{
		"command": map[string]any{
			"type":        "string",
			"description": "Shell command to run in the session container via sh -lc. Empty or whitespace-only values are rejected.",
		},
		"session_id": map[string]any{
			"type":        "string",
			"description": "Optional session identifier. Reuse it to keep filesystem state.",
		},
		"create_if_missing": map[string]any{
			"type":        "boolean",
			"description": "When true and session_id is missing on worker, create the session instead of returning session_not_found.",
			"default":     false,
		},
		"lease_ttl_sec": map[string]any{
			"type":        "integer",
			"description": "Optional lease duration in seconds for session expiry extension.",
			"minimum":     minMCPTerminalLeaseSec,
			"maximum":     maxMCPTerminalLeaseSec,
		},
		"timeout_ms": map[string]any{
			"type":        "integer",
			"description": "Optional synchronous execution timeout in milliseconds for this tool call.",
			"minimum":     minMCPTaskTimeoutMS,
			"maximum":     maxMCPTaskTimeoutMS,
			"default":     defaultMCPTaskTimeoutMS,
		},
	},
}

var mcpTerminalExecOutputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required": []string{
		"session_id",
		"created",
		"stdout",
		"stderr",
		"exit_code",
		"stdout_truncated",
		"stderr_truncated",
		"lease_expires_unix_ms",
	},
	"properties": map[string]any{
		"session_id": map[string]any{"type": "string"},
		"created":    map[string]any{"type": "boolean"},
		"stdout":     map[string]any{"type": "string"},
		"stderr":     map[string]any{"type": "string"},
		"exit_code":  map[string]any{"type": "integer"},
		"stdout_truncated": map[string]any{
			"type": "boolean",
		},
		"stderr_truncated": map[string]any{
			"type": "boolean",
		},
		"lease_expires_unix_ms": map[string]any{
			"type": "integer",
		},
	},
}

var mcpComputerUseInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"command"},
	"properties": map[string]any{
		"command": map[string]any{
			"type":        "string",
			"description": "Shell command to run on worker-sys host via /bin/sh -lc. Empty or whitespace-only values are rejected.",
		},
		"timeout_ms": map[string]any{
			"type":        "integer",
			"description": "Optional synchronous execution timeout in milliseconds for this tool call.",
			"minimum":     minMCPTaskTimeoutMS,
			"maximum":     maxMCPTaskTimeoutMS,
			"default":     defaultMCPTaskTimeoutMS,
		},
		"request_id": map[string]any{
			"type":        "string",
			"description": "Optional idempotency key scoped to the caller account.",
		},
	},
}

var mcpComputerUseOutputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required": []string{
		"stdout",
		"stderr",
		"exit_code",
		"stdout_truncated",
		"stderr_truncated",
	},
	"properties": map[string]any{
		"stdout": map[string]any{"type": "string"},
		"stderr": map[string]any{"type": "string"},
		"exit_code": map[string]any{
			"type": "integer",
		},
		"stdout_truncated": map[string]any{
			"type": "boolean",
		},
		"stderr_truncated": map[string]any{
			"type": "boolean",
		},
	},
}

var mcpReadImageInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"session_id", "file_path"},
	"properties": map[string]any{
		"session_id": map[string]any{
			"type":        "string",
			"description": "Terminal session identifier returned by terminalExec. Use exact value \"computerUse\" to route to the caller-owned worker-sys (readImage).",
		},
		"file_path": map[string]any{
			"type":        "string",
			"description": "Path to the file in the terminal session filesystem.",
		},
		"timeout_ms": map[string]any{
			"type":        "integer",
			"description": "Optional synchronous execution timeout in milliseconds for this tool call.",
			"minimum":     minMCPTaskTimeoutMS,
			"maximum":     maxMCPTaskTimeoutMS,
			"default":     defaultMCPTaskTimeoutMS,
		},
	},
}

var mcpExportFileInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"session_id", "file_path"},
	"properties": map[string]any{
		"session_id": map[string]any{
			"type":        "string",
			"description": "Session identifier. Use exact value \"computerUse\" to route to the caller-owned worker-sys (readImage); other values route to terminalResource for terminal sessions.",
		},
		"file_path": map[string]any{
			"type":        "string",
			"description": "Path to the file in the terminal session filesystem.",
		},
		"timeout_ms": map[string]any{
			"type":        "integer",
			"description": "Optional synchronous execution timeout in milliseconds for this tool call.",
			"minimum":     minMCPTaskTimeoutMS,
			"maximum":     maxMCPTaskTimeoutMS,
			"default":     defaultMCPTaskTimeoutMS,
		},
	},
}

var mcpExportFileOutputSchemaSignedURL = map[string]any{
	"type":        "string",
	"description": "Presigned download URL for the exported object.",
}

var mcpExportFileOutputSchemaObjectKey = map[string]any{
	"type":        "string",
	"description": "Object key written to the configured bucket.",
}

var mcpExportFileOutputSchemaFileName = map[string]any{
	"type":        "string",
	"description": "Original basename derived from file_path.",
}

// deepCopySchema returns a deep copy of a schema fragment represented as
// map[string]any, []string, or []any. It is used to derive per-request
// overrides without mutating the package-level schema vars.
func deepCopySchema(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopySchema(t)
	case []string:
		cp := make([]string, len(t))
		copy(cp, t)
		return cp
	case []any:
		cp := make([]any, len(t))
		for i, item := range t {
			cp[i] = deepCopyValue(item)
		}
		return cp
	default:
		return v
	}
}

// applyToolDescriptionOverride returns the final Description string given a
// default and an optional override pointer. Empty string is treated as invalid
// (warn + fallback). A nil pointer means no override was requested.
func applyToolDescriptionOverride(defaultValue string, override *string, logger *slog.Logger, toolName string) string {
	if override == nil {
		return defaultValue
	}
	if *override == "" {
		if logger != nil {
			logger.Warn("ignoring empty MCP tool description override; falling back to default",
				"tool", toolName)
		}
		return defaultValue
	}
	return *override
}

// applyToolNameOverride returns the final Name string given a default and an
// optional override pointer. Empty strings or values not matching
// mcpToolNamePattern fall back to defaultValue with a warn log. A nil pointer
// means no override was requested.
//
// Conflict-against-other-tools (override duplicating another tool's exposed
// name or built-in default) is enforced one layer up by the handler-level
// resolveExposedName closure, which has visibility into the full tool catalog.
func applyToolNameOverride(defaultValue string, override *string, logger *slog.Logger, toolName string) string {
	if override == nil {
		return defaultValue
	}
	if *override == "" {
		if logger != nil {
			logger.Warn("ignoring empty MCP tool name override; falling back to default",
				"tool", toolName)
		}
		return defaultValue
	}
	if !mcpToolNamePattern.MatchString(*override) {
		if logger != nil {
			logger.Warn("ignoring invalid MCP tool name override; must match ^[a-zA-Z0-9_-]{1,64}$",
				"tool", toolName, "override", *override)
		}
		return defaultValue
	}
	return *override
}

// applyToolTitleOverride mirrors applyToolDescriptionOverride for the Title
// field. Empty string is treated as invalid.
func applyToolTitleOverride(defaultValue string, override *string, logger *slog.Logger, toolName string) string {
	if override == nil {
		return defaultValue
	}
	if *override == "" {
		if logger != nil {
			logger.Warn("ignoring empty MCP tool title override; falling back to default",
				"tool", toolName)
		}
		return defaultValue
	}
	return *override
}

// applyInputSchemaOverride returns a deep-copied input schema with per-param
// descriptions overridden. For params whose override is an empty string, the
// property is removed from `properties` and `required`, and `additionalProperties`
// is flipped to true so that clients may still transmit the field without being
// rejected by JSON Schema validation. Non-empty overrides replace only the
// `description` field of the corresponding property, leaving type/minimum/
// maximum/default untouched.
//
// If no overrides apply (paramOverrides empty or only non-matching keys), the
// original schema pointer is returned unchanged to preserve the strict-mode
// default (additionalProperties: false).
func applyInputSchemaOverride(base map[string]any, paramOverrides map[string]*string, logger *slog.Logger, toolName string) map[string]any {
	if len(paramOverrides) == 0 {
		return base
	}
	// Pre-scan to see if anything matters; avoids an unnecessary deep copy.
	properties, _ := base["properties"].(map[string]any)
	anyHit := false
	for name := range paramOverrides {
		if _, exists := properties[name]; exists {
			anyHit = true
			break
		}
	}
	if !anyHit {
		return base
	}

	out := deepCopySchema(base)
	outProps, _ := out["properties"].(map[string]any)
	outRequired, _ := out["required"].([]string)
	hiddenAny := false

	for name, val := range paramOverrides {
		if val == nil {
			continue
		}
		if _, exists := outProps[name]; !exists {
			// Catalog/schema mismatch; skip silently.
			continue
		}
		if *val == "" {
			// Hide: remove from properties and required.
			wasRequired := false
			for _, r := range outRequired {
				if r == name {
					wasRequired = true
					break
				}
			}
			delete(outProps, name)
			if wasRequired {
				filtered := make([]string, 0, len(outRequired))
				for _, r := range outRequired {
					if r != name {
						filtered = append(filtered, r)
					}
				}
				outRequired = filtered
			}
			if logger != nil {
				logger.Warn("hiding MCP tool parameter from inputSchema; HTTP callers may still pass it",
					"tool", toolName, "param", name, "required", wasRequired)
			}
			hiddenAny = true
			continue
		}
		// Override description in place.
		propMap, ok := outProps[name].(map[string]any)
		if !ok {
			continue
		}
		propMap["description"] = *val
	}

	// Write back required (may have shrunk).
	if _, has := out["required"]; has {
		if len(outRequired) == 0 {
			delete(out, "required")
		} else {
			out["required"] = outRequired
		}
	}
	if hiddenAny {
		out["additionalProperties"] = true
	}
	return out
}

func exportFileOutputSchemaForMode(schema string) map[string]any {
	switch schema {
	case ExportReturnSchemaSignedURL:
		return map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"signed_url"},
			"properties": map[string]any{
				"signed_url": mcpExportFileOutputSchemaSignedURL,
			},
		}
	case ExportReturnSchemaObjectKey:
		return map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"object_key", "filename"},
			"properties": map[string]any{
				"object_key": mcpExportFileOutputSchemaObjectKey,
				"filename":   mcpExportFileOutputSchemaFileName,
			},
		}
	default:
		return map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"signed_url", "object_key", "filename"},
			"properties": map[string]any{
				"signed_url": mcpExportFileOutputSchemaSignedURL,
				"object_key": mcpExportFileOutputSchemaObjectKey,
				"filename":   mcpExportFileOutputSchemaFileName,
			},
		}
	}
}
