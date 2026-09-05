package config

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	defaultConsoleTarget             = "127.0.0.1:50051"
	defaultHeartbeatInterval         = 5
	defaultHeartbeatJitter           = 20
	defaultPythonExecImage           = "ghcr.io/astral-sh/uv:python3.12-bookworm-slim"
	defaultPythonExecMemoryMiB       = 256
	defaultPythonExecCPULimit        = "1.0"
	defaultPythonExecMaxProcesses    = 128
	defaultTerminalExecImage         = "coolfan1024/onlyboxes-runtime:default"
	defaultTerminalExecMemoryMiB     = 256
	defaultTerminalExecCPULimit      = "1.0"
	defaultTerminalExecMaxProcesses  = 128
	defaultTerminalLeaseMin          = 60
	defaultTerminalLeaseMax          = 1800
	defaultTerminalLeaseTTL          = 60
	defaultTerminalOutputMax         = 1024 * 1024
	defaultTerminalSessionInflight   = 1
	defaultTerminalMaxActiveSessions = 0
	defaultProxyListenAddr           = ":8091"
	defaultLogLevel                  = "info"
	defaultLogFormat                 = "json"
	defaultLogAddSource              = false
	defaultMaxInflight               = 4
)

type Config struct {
	ConfigFile                  string
	ConsoleGRPCTarget           string
	ConsoleTLS                  bool
	WorkerID                    string
	WorkerSecret                string
	HeartbeatInterval           time.Duration
	HeartbeatJitter             int
	CallTimeout                 time.Duration
	NodeName                    string
	PythonExecDockerImage       string
	PythonExecMemoryLimit       string
	PythonExecCPULimit          string
	PythonExecPidsLimit         int
	TerminalExecDockerImage     string
	TerminalExecMemoryLimit     string
	TerminalExecCPULimit        string
	TerminalExecPidsLimit       int
	Labels                      map[string]string
	TerminalLeaseMinSec         int
	TerminalLeaseMaxSec         int
	TerminalLeaseDefaultSec     int
	TerminalOutputLimitBytes    int
	TerminalExportMaxBytes      int
	TerminalSessionMaxInflight  int
	TerminalMaxActiveSessions   int
	EchoMaxInflight             int
	PythonExecMaxInflight       int
	TerminalExecMaxInflight     int
	TerminalResourceMaxInflight int
	ProxyEnabled                bool
	ProxyListenAddr             string
	ProxyAdvertiseAddr          string
	LogLevel                    string
	LogFormat                   string
	LogAddSource                bool
}

func Load() Config {
	src := newSource()

	heartbeatSec := src.positiveInt("WORKER_HEARTBEAT_INTERVAL_SEC", defaultHeartbeatInterval)
	heartbeatJitter := src.percent("WORKER_HEARTBEAT_JITTER_PCT", defaultHeartbeatJitter)
	callTimeoutSec := src.positiveInt("WORKER_CALL_TIMEOUT_SEC", defaultCallTimeoutSec(heartbeatSec))
	terminalLeaseMinSec := src.positiveInt("WORKER_TERMINAL_LEASE_MIN_SEC", defaultTerminalLeaseMin)
	terminalLeaseMaxSec := src.positiveInt("WORKER_TERMINAL_LEASE_MAX_SEC", defaultTerminalLeaseMax)
	if terminalLeaseMaxSec < terminalLeaseMinSec {
		terminalLeaseMaxSec = terminalLeaseMinSec
	}
	terminalLeaseDefaultSec := src.positiveInt("WORKER_TERMINAL_LEASE_DEFAULT_SEC", defaultTerminalLeaseTTL)
	terminalLeaseDefaultSec = min(max(terminalLeaseDefaultSec, terminalLeaseMinSec), terminalLeaseMaxSec)
	terminalOutputLimitBytes := src.positiveInt("WORKER_TERMINAL_OUTPUT_LIMIT_BYTES", defaultTerminalOutputMax)
	terminalExportMaxBytes := src.positiveInt("WORKER_TERMINAL_EXPORT_MAX_BYTES", 0)

	labelsCSV := src.Get("WORKER_LABELS")

	return Config{
		ConfigFile:                  src.Path(),
		ConsoleGRPCTarget:           src.stringValue("WORKER_CONSOLE_GRPC_TARGET", defaultConsoleTarget),
		ConsoleTLS:                  src.Get("WORKER_CONSOLE_INSECURE") != "true",
		WorkerID:                    strings.TrimSpace(src.Get("WORKER_ID")),
		WorkerSecret:                strings.TrimSpace(src.Get("WORKER_SECRET")),
		HeartbeatInterval:           time.Duration(heartbeatSec) * time.Second,
		HeartbeatJitter:             heartbeatJitter,
		CallTimeout:                 time.Duration(callTimeoutSec) * time.Second,
		NodeName:                    src.Get("WORKER_NODE_NAME"),
		PythonExecDockerImage:       src.stringValue("WORKER_PYTHON_EXEC_DOCKER_IMAGE", defaultPythonExecImage),
		PythonExecMemoryLimit:       src.dockerMemoryLimitMiB("WORKER_PYTHON_EXEC_MEMORY_MIB", defaultPythonExecMemoryMiB),
		PythonExecCPULimit:          src.dockerCPULimit("WORKER_PYTHON_EXEC_CPUS", defaultPythonExecCPULimit),
		PythonExecPidsLimit:         src.positiveInt("WORKER_PYTHON_EXEC_MAX_PROCESSES", defaultPythonExecMaxProcesses),
		TerminalExecDockerImage:     src.stringValue("WORKER_TERMINAL_EXEC_DOCKER_IMAGE", defaultTerminalExecImage),
		TerminalExecMemoryLimit:     src.dockerMemoryLimitMiB("WORKER_TERMINAL_EXEC_MEMORY_MIB", defaultTerminalExecMemoryMiB),
		TerminalExecCPULimit:        src.dockerCPULimit("WORKER_TERMINAL_EXEC_CPUS", defaultTerminalExecCPULimit),
		TerminalExecPidsLimit:       src.positiveInt("WORKER_TERMINAL_EXEC_MAX_PROCESSES", defaultTerminalExecMaxProcesses),
		Labels:                      parseLabels(labelsCSV),
		TerminalLeaseMinSec:         terminalLeaseMinSec,
		TerminalLeaseMaxSec:         terminalLeaseMaxSec,
		TerminalLeaseDefaultSec:     terminalLeaseDefaultSec,
		TerminalOutputLimitBytes:    terminalOutputLimitBytes,
		TerminalExportMaxBytes:      terminalExportMaxBytes,
		TerminalSessionMaxInflight:  src.positiveInt("WORKER_TERMINAL_SESSION_MAX_INFLIGHT", defaultTerminalSessionInflight),
		TerminalMaxActiveSessions:   src.nonNegativeInt("WORKER_TERMINAL_MAX_ACTIVE_SESSIONS", defaultTerminalMaxActiveSessions),
		EchoMaxInflight:             src.positiveInt("WORKER_ECHO_MAX_INFLIGHT", defaultMaxInflight),
		PythonExecMaxInflight:       src.positiveInt("WORKER_PYTHON_EXEC_MAX_INFLIGHT", defaultMaxInflight),
		TerminalExecMaxInflight:     src.positiveInt("WORKER_TERMINAL_EXEC_MAX_INFLIGHT", defaultMaxInflight),
		TerminalResourceMaxInflight: src.positiveInt("WORKER_TERMINAL_RESOURCE_MAX_INFLIGHT", defaultMaxInflight),
		ProxyEnabled:                src.boolValue("WORKER_PROXY_ENABLED", false),
		ProxyListenAddr:             strings.TrimSpace(src.stringValue("WORKER_PROXY_LISTEN_ADDR", defaultProxyListenAddr)),
		ProxyAdvertiseAddr:          strings.TrimSpace(src.Get("WORKER_PROXY_ADVERTISE_ADDR")),
		LogLevel:                    src.logLevel("WORKER_LOG_LEVEL", defaultLogLevel),
		LogFormat:                   src.logFormat("WORKER_LOG_FORMAT", defaultLogFormat),
		LogAddSource:                src.boolValue("WORKER_LOG_ADD_SOURCE", defaultLogAddSource),
	}
}

func (s source) stringValue(key string, defaultValue string) string {
	value := s.Get(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func (s source) positiveInt(key string, defaultValue int) int {
	value := strings.TrimSpace(s.Get(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return defaultValue
	}
	return parsed
}

func (s source) nonNegativeInt(key string, defaultValue int) int {
	value := strings.TrimSpace(s.Get(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return defaultValue
	}
	return parsed
}

func (s source) dockerCPULimit(key string, defaultValue string) string {
	value := strings.TrimSpace(s.Get(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed <= 0 {
		return defaultValue
	}
	return value
}

func (s source) dockerMemoryLimitMiB(key string, defaultValueMiB int) string {
	value := s.positiveInt(key, defaultValueMiB)
	return strconv.Itoa(value) + "m"
}

func (s source) percent(key string, defaultValue int) int {
	value := strings.TrimSpace(s.Get(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 || parsed > 100 {
		return defaultValue
	}
	return parsed
}

func (s source) boolValue(key string, defaultValue bool) bool {
	switch strings.TrimSpace(strings.ToLower(s.Get(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return defaultValue
	}
}

func (s source) logLevel(key string, defaultValue string) string {
	value := strings.TrimSpace(strings.ToLower(s.Get(key)))
	switch value {
	case "debug", "info", "warn", "error":
		return value
	default:
		return defaultValue
	}
}

func (s source) logFormat(key string, defaultValue string) string {
	value := strings.TrimSpace(strings.ToLower(s.Get(key)))
	switch value {
	case "json", "text":
		return value
	default:
		return defaultValue
	}
}

func defaultCallTimeoutSec(heartbeatSec int) int {
	if heartbeatSec <= 0 {
		heartbeatSec = defaultHeartbeatInterval
	}
	// ceil(2.5 * heartbeatSec) without floating point.
	return (heartbeatSec*5 + 1) / 2
}

func parseLabels(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}
	}

	if strings.HasPrefix(strings.TrimSpace(raw), "{") {
		decoded := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
			return normalizeLabels(decoded)
		}
	}

	parts := strings.Split(raw, ",")
	labels := make(map[string]string, len(parts))
	for _, part := range parts {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		tokens := strings.SplitN(entry, "=", 2)
		if len(tokens) != 2 {
			continue
		}
		key := strings.TrimSpace(tokens[0])
		value := strings.TrimSpace(tokens[1])
		if key == "" {
			continue
		}
		labels[key] = value
	}
	return labels
}

func normalizeLabels(raw map[string]string) map[string]string {
	labels := make(map[string]string, len(raw))
	for key, value := range raw {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		labels[key] = strings.TrimSpace(value)
	}
	return labels
}
