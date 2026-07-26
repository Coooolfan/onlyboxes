package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/buildinfo"
)

const (
	defaultConsoleTarget            = "127.0.0.1:50051"
	defaultHeartbeatInterval        = 5
	defaultHeartbeatJitter          = 20
	defaultExecutorKind             = "docker"
	defaultPythonExecImage          = "ghcr.io/astral-sh/uv:python3.12-bookworm-slim"
	defaultPythonExecMemoryMiB      = 256
	defaultPythonExecCPULimit       = "1.0"
	defaultPythonExecMaxProcesses   = 128
	defaultTerminalExecImage        = "coolfan1024/onlyboxes-default-worker:0.0.5"
	defaultTerminalExecMemoryMiB    = 256
	defaultTerminalExecCPULimit     = "1.0"
	defaultTerminalExecMaxProcesses = 128
	defaultTerminalLeaseMin         = 60
	defaultTerminalLeaseMax         = 1800
	defaultTerminalLeaseTTL         = 60
	defaultTerminalOutputMax        = 1024 * 1024
	defaultTerminalSessionInflight  = 1
	defaultLogLevel                 = "info"
	defaultLogFormat                = "json"
	defaultLogAddSource             = false
	defaultMaxInflight              = 4
)

type Config struct {
	ConsoleGRPCTarget           string
	ConsoleTLS                  bool
	WorkerID                    string
	WorkerSecret                string
	HeartbeatInterval           time.Duration
	HeartbeatJitter             int
	CallTimeout                 time.Duration
	NodeName                    string
	ExecutorKind                string
	Version                     string
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
	EchoMaxInflight             int
	PythonExecMaxInflight       int
	TerminalExecMaxInflight     int
	TerminalResourceMaxInflight int
	LogLevel                    string
	LogFormat                   string
	LogAddSource                bool
}

func Load() Config {
	heartbeatSec := parsePositiveIntEnv("WORKER_HEARTBEAT_INTERVAL_SEC", defaultHeartbeatInterval)
	heartbeatJitter := parsePercentEnv("WORKER_HEARTBEAT_JITTER_PCT", defaultHeartbeatJitter)
	callTimeoutSec := parsePositiveIntEnv("WORKER_CALL_TIMEOUT_SEC", defaultCallTimeoutSec(heartbeatSec))
	terminalLeaseMinSec := parsePositiveIntEnv("WORKER_TERMINAL_LEASE_MIN_SEC", defaultTerminalLeaseMin)
	terminalLeaseMaxSec := parsePositiveIntEnv("WORKER_TERMINAL_LEASE_MAX_SEC", defaultTerminalLeaseMax)
	if terminalLeaseMaxSec < terminalLeaseMinSec {
		terminalLeaseMaxSec = terminalLeaseMinSec
	}
	terminalLeaseDefaultSec := parsePositiveIntEnv("WORKER_TERMINAL_LEASE_DEFAULT_SEC", defaultTerminalLeaseTTL)
	terminalLeaseDefaultSec = clampInt(terminalLeaseDefaultSec, terminalLeaseMinSec, terminalLeaseMaxSec)
	terminalOutputLimitBytes := parsePositiveIntEnv("WORKER_TERMINAL_OUTPUT_LIMIT_BYTES", defaultTerminalOutputMax)
	terminalExportMaxBytes := parsePositiveIntEnv("WORKER_TERMINAL_EXPORT_MAX_BYTES", 0)

	labelsCSV := os.Getenv("WORKER_LABELS")
	defaultVersion := strings.TrimSpace(buildinfo.Version)
	if defaultVersion == "" {
		defaultVersion = "dev"
	}

	return Config{
		ConsoleGRPCTarget:           getEnv("WORKER_CONSOLE_GRPC_TARGET", defaultConsoleTarget),
		ConsoleTLS:                  os.Getenv("WORKER_CONSOLE_INSECURE") != "true",
		WorkerID:                    strings.TrimSpace(os.Getenv("WORKER_ID")),
		WorkerSecret:                strings.TrimSpace(os.Getenv("WORKER_SECRET")),
		HeartbeatInterval:           time.Duration(heartbeatSec) * time.Second,
		HeartbeatJitter:             heartbeatJitter,
		CallTimeout:                 time.Duration(callTimeoutSec) * time.Second,
		NodeName:                    os.Getenv("WORKER_NODE_NAME"),
		ExecutorKind:                defaultExecutorKind,
		Version:                     getEnv("WORKER_VERSION", defaultVersion),
		PythonExecDockerImage:       getEnv("WORKER_PYTHON_EXEC_DOCKER_IMAGE", defaultPythonExecImage),
		PythonExecMemoryLimit:       parseDockerMemoryLimitMiBEnv("WORKER_PYTHON_EXEC_MEMORY_MIB", defaultPythonExecMemoryMiB),
		PythonExecCPULimit:          parseDockerCPULimitEnv("WORKER_PYTHON_EXEC_CPUS", defaultPythonExecCPULimit),
		PythonExecPidsLimit:         parsePositiveIntEnv("WORKER_PYTHON_EXEC_MAX_PROCESSES", defaultPythonExecMaxProcesses),
		TerminalExecDockerImage:     getEnv("WORKER_TERMINAL_EXEC_DOCKER_IMAGE", defaultTerminalExecImage),
		TerminalExecMemoryLimit:     parseDockerMemoryLimitMiBEnv("WORKER_TERMINAL_EXEC_MEMORY_MIB", defaultTerminalExecMemoryMiB),
		TerminalExecCPULimit:        parseDockerCPULimitEnv("WORKER_TERMINAL_EXEC_CPUS", defaultTerminalExecCPULimit),
		TerminalExecPidsLimit:       parsePositiveIntEnv("WORKER_TERMINAL_EXEC_MAX_PROCESSES", defaultTerminalExecMaxProcesses),
		Labels:                      parseLabels(labelsCSV),
		TerminalLeaseMinSec:         terminalLeaseMinSec,
		TerminalLeaseMaxSec:         terminalLeaseMaxSec,
		TerminalLeaseDefaultSec:     terminalLeaseDefaultSec,
		TerminalOutputLimitBytes:    terminalOutputLimitBytes,
		TerminalExportMaxBytes:      terminalExportMaxBytes,
		TerminalSessionMaxInflight:  parsePositiveIntEnv("WORKER_TERMINAL_SESSION_MAX_INFLIGHT", defaultTerminalSessionInflight),
		EchoMaxInflight:             parsePositiveIntEnv("WORKER_ECHO_MAX_INFLIGHT", defaultMaxInflight),
		PythonExecMaxInflight:       parsePositiveIntEnv("WORKER_PYTHON_EXEC_MAX_INFLIGHT", defaultMaxInflight),
		TerminalExecMaxInflight:     parsePositiveIntEnv("WORKER_TERMINAL_EXEC_MAX_INFLIGHT", defaultMaxInflight),
		TerminalResourceMaxInflight: parsePositiveIntEnv("WORKER_TERMINAL_RESOURCE_MAX_INFLIGHT", defaultMaxInflight),
		LogLevel:                    parseLogLevelEnv("WORKER_LOG_LEVEL", defaultLogLevel),
		LogFormat:                   parseLogFormatEnv("WORKER_LOG_FORMAT", defaultLogFormat),
		LogAddSource:                parseBoolEnv("WORKER_LOG_ADD_SOURCE", defaultLogAddSource),
	}
}

func getEnv(key string, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func parsePositiveIntEnv(key string, defaultValue int) int {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return defaultValue
	}
	return parsed
}

func parseDockerCPULimitEnv(key string, defaultValue string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed <= 0 {
		return defaultValue
	}
	return value
}

func parseDockerMemoryLimitMiBEnv(key string, defaultValueMiB int) string {
	value := parsePositiveIntEnv(key, defaultValueMiB)
	return strconv.Itoa(value) + "m"
}

func parsePercentEnv(key string, defaultValue int) int {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 || parsed > 100 {
		return defaultValue
	}
	return parsed
}

func parseBoolEnv(key string, defaultValue bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return defaultValue
	}
}

func parseLogLevelEnv(key string, defaultValue string) string {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return defaultValue
	}
	switch value {
	case "debug", "info", "warn", "error":
		return value
	default:
		return defaultValue
	}
}

func parseLogFormatEnv(key string, defaultValue string) string {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return defaultValue
	}
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

func clampInt(value int, minValue int, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}
