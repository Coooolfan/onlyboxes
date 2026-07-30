package config

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	defaultConsoleTarget           = "127.0.0.1:50051"
	defaultHeartbeatInterval       = 5
	defaultHeartbeatJitter         = 20
	defaultExecutorKind            = "e2b"
	defaultE2BAPIURL               = "https://api.e2b.app"
	defaultE2BDomain               = "e2b.app"
	defaultE2BRequestTimeout       = 60
	defaultE2BPythonTimeout        = 300
	defaultTerminalLeaseMin        = 60
	defaultTerminalLeaseMax        = 1800
	defaultTerminalLeaseTTL        = 60
	defaultTerminalOutputMax       = 1024 * 1024
	defaultTerminalSessionInflight = 1
	defaultLogLevel                = "info"
	defaultLogFormat               = "json"
	defaultMaxInflight             = 4
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
	ExecutorKind                string
	Labels                      map[string]string
	E2BAPIKey                   string
	E2BAPIURL                   string
	E2BDomain                   string
	E2BSandboxURL               string
	E2BPythonTemplate           string
	E2BTerminalTemplate         string
	E2BRequestTimeout           time.Duration
	E2BPythonTimeoutSec         int
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
	src := newSource()
	heartbeatSec := src.positiveInt("WORKER_HEARTBEAT_INTERVAL_SEC", defaultHeartbeatInterval)
	heartbeatJitter := src.percent("WORKER_HEARTBEAT_JITTER_PCT", defaultHeartbeatJitter)
	callTimeoutSec := src.positiveInt("WORKER_CALL_TIMEOUT_SEC", defaultCallTimeoutSec(heartbeatSec))
	terminalLeaseMinSec := src.positiveInt("WORKER_TERMINAL_LEASE_MIN_SEC", defaultTerminalLeaseMin)
	terminalLeaseMaxSec := src.positiveInt("WORKER_TERMINAL_LEASE_MAX_SEC", defaultTerminalLeaseMax)
	if terminalLeaseMaxSec < terminalLeaseMinSec {
		terminalLeaseMaxSec = terminalLeaseMinSec
	}
	terminalLeaseDefaultSec := clampInt(
		src.positiveInt("WORKER_TERMINAL_LEASE_DEFAULT_SEC", defaultTerminalLeaseTTL),
		terminalLeaseMinSec,
		terminalLeaseMaxSec,
	)

	e2bAPIKey := strings.TrimSpace(src.getWithEnvAliases("WORKER_E2B_API_KEY", "E2B_API_KEY"))
	e2bAPIURL := strings.TrimRight(strings.TrimSpace(src.getWithEnvAliases("WORKER_E2B_API_URL", "E2B_API_URL")), "/")
	if e2bAPIURL == "" {
		e2bAPIURL = defaultE2BAPIURL
	}
	e2bDomain := strings.TrimSpace(src.getWithEnvAliases("WORKER_E2B_DOMAIN", "E2B_DOMAIN"))
	if e2bDomain == "" {
		e2bDomain = defaultE2BDomain
	}
	pythonTemplate := strings.TrimSpace(src.getWithEnvAliases("WORKER_E2B_PYTHON_TEMPLATE", "E2B_PYTHON_EXEC_TEMPLATE"))
	terminalTemplate := strings.TrimSpace(src.getWithEnvAliases("WORKER_E2B_TERMINAL_TEMPLATE", "E2B_TERMINAL_EXEC_TEMPLATE"))

	return Config{
		ConfigFile:                  src.Path(),
		ConsoleGRPCTarget:           src.stringValue("WORKER_CONSOLE_GRPC_TARGET", defaultConsoleTarget),
		ConsoleTLS:                  !src.boolValue("WORKER_CONSOLE_INSECURE", false),
		WorkerID:                    strings.TrimSpace(src.get("WORKER_ID")),
		WorkerSecret:                strings.TrimSpace(src.get("WORKER_SECRET")),
		HeartbeatInterval:           time.Duration(heartbeatSec) * time.Second,
		HeartbeatJitter:             heartbeatJitter,
		CallTimeout:                 time.Duration(callTimeoutSec) * time.Second,
		NodeName:                    strings.TrimSpace(src.get("WORKER_NODE_NAME")),
		ExecutorKind:                defaultExecutorKind,
		Labels:                      parseLabels(src.get("WORKER_LABELS")),
		E2BAPIKey:                   e2bAPIKey,
		E2BAPIURL:                   e2bAPIURL,
		E2BDomain:                   e2bDomain,
		E2BSandboxURL:               strings.TrimRight(strings.TrimSpace(src.getWithEnvAliases("WORKER_E2B_SANDBOX_URL", "E2B_SANDBOX_URL")), "/"),
		E2BPythonTemplate:           pythonTemplate,
		E2BTerminalTemplate:         terminalTemplate,
		E2BRequestTimeout:           time.Duration(src.positiveInt("WORKER_E2B_REQUEST_TIMEOUT_SEC", defaultE2BRequestTimeout)) * time.Second,
		E2BPythonTimeoutSec:         positiveIntValue(src.getWithEnvAliases("WORKER_E2B_PYTHON_TIMEOUT_SEC", "E2B_SANDBOX_TIMEOUT_SEC"), defaultE2BPythonTimeout),
		TerminalLeaseMinSec:         terminalLeaseMinSec,
		TerminalLeaseMaxSec:         terminalLeaseMaxSec,
		TerminalLeaseDefaultSec:     terminalLeaseDefaultSec,
		TerminalOutputLimitBytes:    src.positiveInt("WORKER_TERMINAL_OUTPUT_LIMIT_BYTES", defaultTerminalOutputMax),
		TerminalExportMaxBytes:      src.nonNegativeInt("WORKER_TERMINAL_EXPORT_MAX_BYTES", 0),
		TerminalSessionMaxInflight:  src.positiveInt("WORKER_TERMINAL_SESSION_MAX_INFLIGHT", defaultTerminalSessionInflight),
		EchoMaxInflight:             src.positiveInt("WORKER_ECHO_MAX_INFLIGHT", defaultMaxInflight),
		PythonExecMaxInflight:       src.positiveInt("WORKER_PYTHON_EXEC_MAX_INFLIGHT", defaultMaxInflight),
		TerminalExecMaxInflight:     src.positiveInt("WORKER_TERMINAL_EXEC_MAX_INFLIGHT", defaultMaxInflight),
		TerminalResourceMaxInflight: src.positiveInt("WORKER_TERMINAL_RESOURCE_MAX_INFLIGHT", defaultMaxInflight),
		LogLevel:                    src.logLevel("WORKER_LOG_LEVEL", defaultLogLevel),
		LogFormat:                   src.logFormat("WORKER_LOG_FORMAT", defaultLogFormat),
		LogAddSource:                src.boolValue("WORKER_LOG_ADD_SOURCE", false),
	}
}

func positiveIntValue(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func (s source) stringValue(key, fallback string) string {
	if value := strings.TrimSpace(s.get(key)); value != "" {
		return value
	}
	return fallback
}

func (s source) positiveInt(key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(s.get(key)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func (s source) nonNegativeInt(key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(s.get(key)))
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func (s source) percent(key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(s.get(key)))
	if err != nil || value < 0 || value > 100 {
		return fallback
	}
	return value
}

func (s source) boolValue(key string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(s.get(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func (s source) logLevel(key, fallback string) string {
	switch value := strings.ToLower(strings.TrimSpace(s.get(key))); value {
	case "debug", "info", "warn", "error":
		return value
	default:
		return fallback
	}
}

func (s source) logFormat(key, fallback string) string {
	switch value := strings.ToLower(strings.TrimSpace(s.get(key))); value {
	case "json", "text":
		return value
	default:
		return fallback
	}
}

func defaultCallTimeoutSec(heartbeatSec int) int {
	if heartbeatSec <= 0 {
		heartbeatSec = defaultHeartbeatInterval
	}
	return (heartbeatSec*5 + 1) / 2
}

func parseLabels(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}
	}
	if strings.HasPrefix(strings.TrimSpace(raw), "{") {
		labels := map[string]string{}
		if json.Unmarshal([]byte(raw), &labels) == nil {
			return normalizeLabels(labels)
		}
	}
	labels := map[string]string{}
	for _, item := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) != "" {
			labels[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return labels
}

func normalizeLabels(raw map[string]string) map[string]string {
	labels := make(map[string]string, len(raw))
	for key, value := range raw {
		if key = strings.TrimSpace(key); key != "" {
			labels[key] = strings.TrimSpace(value)
		}
	}
	return labels
}

func clampInt(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}
