package httpapi

const (
	mcpServerName                  = "onlyboxes-console"
	mcpServerVersion               = "v0.1.0"
	pythonExecCapabilityName       = "pythonExec"
	terminalExecCapabilityName     = "terminalExec"
	terminalResourceCapabilityName = "terminalResource"
	defaultMCPEchoTimeoutMS        = defaultEchoTimeoutMS
	minMCPTaskTimeoutMS            = 1
	defaultMCPTaskTimeoutMS        = defaultTaskTimeoutMS
	maxMCPPythonExecTimeoutMS      = maxTaskTimeoutMS
	minMCPTerminalLeaseSec         = 1
	maxMCPTerminalLeaseSec         = 86400
	mcpEchoToolTitle               = "Echo Message"
	mcpPythonExecToolTitle         = "Python Execute"
	mcpTerminalExecToolTitle       = "Terminal Execute"
	mcpReadImageToolTitle          = "Read Image"
)

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

type mcpReadImageToolInput struct {
	SessionID string `json:"session_id"`
	FilePath  string `json:"file_path"`
	TimeoutMS *int   `json:"timeout_ms,omitempty"`
}

type pythonExecPayload struct {
	Code string `json:"code"`
}

var mcpEchoToolDescription = "Echoes the input message exactly as returned by an online worker supporting the echo capability. Use this tool for connectivity checks, request tracing, and latency baselines. Do not use it for code execution, file operations, or long-running work. timeout_ms is an end-to-end dispatch timeout in milliseconds (1-60000, default 5000)."

var mcpPythonExecToolDescription = "Executes Python code in the worker sandbox via the pythonExec capability and returns stdout, stderr, and exit_code. Use this for short, self-contained snippets. Do not use it for long-running jobs or persistent state. timeout_ms is a synchronous execution timeout in milliseconds (1-600000, default 60000). A non-zero exit_code is returned as normal tool output, not as a protocol error."

var mcpTerminalExecToolDescription = "Executes shell commands in a persistent Docker-backed terminal session via the terminalExec capability. Reuse session_id to preserve filesystem state across calls. create_if_missing controls missing-session behavior. lease_ttl_sec extends session lease within worker-configured bounds. timeout_ms is a synchronous execution timeout in milliseconds (1-600000, default 60000)."

var mcpReadImageToolDescription = "Reads a file from an existing terminal session and returns it as inline image content when mime type is image/*. For unsupported mime types, returns a text explanation."

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
			"description": "Optional end-to-end dispatch timeout in milliseconds for the echo capability.",
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
			"description": "Python source code to execute in the worker sandbox. Empty or whitespace-only values are rejected.",
		},
		"timeout_ms": map[string]any{
			"type":        "integer",
			"description": "Optional synchronous execution timeout in milliseconds for this tool call.",
			"minimum":     minMCPTaskTimeoutMS,
			"maximum":     maxMCPPythonExecTimeoutMS,
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
			"description": "Shell command to run in the session container. Empty or whitespace-only values are rejected.",
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
			"maximum":     maxMCPPythonExecTimeoutMS,
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

var mcpReadImageInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"session_id", "file_path"},
	"properties": map[string]any{
		"session_id": map[string]any{
			"type":        "string",
			"description": "Terminal session identifier returned by terminalExec.",
		},
		"file_path": map[string]any{
			"type":        "string",
			"description": "Path to the file in the terminal session filesystem.",
		},
		"timeout_ms": map[string]any{
			"type":        "integer",
			"description": "Optional synchronous execution timeout in milliseconds for this tool call.",
			"minimum":     minMCPTaskTimeoutMS,
			"maximum":     maxMCPPythonExecTimeoutMS,
			"default":     defaultMCPTaskTimeoutMS,
		},
	},
}
