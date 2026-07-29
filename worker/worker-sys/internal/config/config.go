package config

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	defaultConsoleTarget             = "127.0.0.1:50051"
	defaultHeartbeatIntervalSec      = 5
	defaultHeartbeatJitterPct        = 20
	defaultExecutorKind              = "sys"
	defaultComputerUseOutputMaxByte  = 1024 * 1024
	computerUseWhitelistModePrefix   = "prefix"
	computerUseWhitelistModeExact    = "exact"
	computerUseWhitelistModeAllowAll = "allow_all"
	defaultLogLevel                  = "info"
	defaultLogFormat                 = "json"
	defaultLogAddSource              = false
	// worker-sys executes on the host without a container sandbox, so command
	// concurrency stays at one unless the operator opts in.
	defaultMaxInflight = 1
)

type Config struct {
	ConfigFile                 string
	ConsoleGRPCTarget          string
	ConsoleTLS                 bool
	WorkerID                   string
	WorkerSecret               string
	HeartbeatInterval          time.Duration
	HeartbeatJitter            int
	CallTimeout                time.Duration
	NodeName                   string
	ExecutorKind               string
	Labels                     map[string]string
	ComputerUseOutputLimitByte int
	ComputerUseWhitelistMode   string
	ComputerUseWhitelist       []string
	ReadImageAllowedPaths      []string
	ComputerUseMaxInflight     int
	ReadImageMaxInflight       int
	LogLevel                   string
	LogFormat                  string
	LogAddSource               bool
}

func Load() Config {
	src := newSource()

	heartbeatSec := src.positiveInt("WORKER_HEARTBEAT_INTERVAL_SEC", defaultHeartbeatIntervalSec)
	heartbeatJitter := src.percent("WORKER_HEARTBEAT_JITTER_PCT", defaultHeartbeatJitterPct)
	callTimeoutSec := src.positiveInt("WORKER_CALL_TIMEOUT_SEC", defaultCallTimeoutSec(heartbeatSec))
	outputLimit := src.positiveInt("WORKER_COMPUTER_USE_OUTPUT_LIMIT_BYTES", defaultComputerUseOutputMaxByte)
	whitelistMode := parseComputerUseWhitelistMode(src.get("WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE"))
	whitelist := parseComputerUseWhitelist(src.get("WORKER_COMPUTER_USE_COMMAND_WHITELIST"))
	readImageAllowedPaths := parseReadImageAllowedPaths(src.get("WORKER_READ_IMAGE_ALLOWED_PATHS"))

	return Config{
		ConfigFile:                 src.Path(),
		ConsoleGRPCTarget:          src.stringValue("WORKER_CONSOLE_GRPC_TARGET", defaultConsoleTarget),
		ConsoleTLS:                 src.get("WORKER_CONSOLE_INSECURE") != "true",
		WorkerID:                   strings.TrimSpace(src.get("WORKER_ID")),
		WorkerSecret:               strings.TrimSpace(src.get("WORKER_SECRET")),
		HeartbeatInterval:          time.Duration(heartbeatSec) * time.Second,
		HeartbeatJitter:            heartbeatJitter,
		CallTimeout:                time.Duration(callTimeoutSec) * time.Second,
		NodeName:                   src.get("WORKER_NODE_NAME"),
		ExecutorKind:               defaultExecutorKind,
		Labels:                     parseLabels(src.get("WORKER_LABELS")),
		ComputerUseOutputLimitByte: outputLimit,
		ComputerUseWhitelistMode:   whitelistMode,
		ComputerUseWhitelist:       whitelist,
		ReadImageAllowedPaths:      readImageAllowedPaths,
		ComputerUseMaxInflight:     src.positiveInt("WORKER_COMPUTER_USE_MAX_INFLIGHT", defaultMaxInflight),
		ReadImageMaxInflight:       src.positiveInt("WORKER_READ_IMAGE_MAX_INFLIGHT", defaultMaxInflight),
		LogLevel:                   src.logLevel("WORKER_LOG_LEVEL", defaultLogLevel),
		LogFormat:                  src.logFormat("WORKER_LOG_FORMAT", defaultLogFormat),
		LogAddSource:               src.boolValue("WORKER_LOG_ADD_SOURCE", defaultLogAddSource),
	}
}

func (s source) stringValue(key string, defaultValue string) string {
	value := s.get(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func (s source) positiveInt(key string, defaultValue int) int {
	value := strings.TrimSpace(s.get(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return defaultValue
	}
	return parsed
}

func (s source) percent(key string, defaultValue int) int {
	value := strings.TrimSpace(s.get(key))
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
	switch strings.TrimSpace(strings.ToLower(s.get(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return defaultValue
	}
}

func (s source) logLevel(key string, defaultValue string) string {
	value := strings.TrimSpace(strings.ToLower(s.get(key)))
	switch value {
	case "debug", "info", "warn", "error":
		return value
	default:
		return defaultValue
	}
}

func (s source) logFormat(key string, defaultValue string) string {
	value := strings.TrimSpace(strings.ToLower(s.get(key)))
	switch value {
	case "json", "text":
		return value
	default:
		return defaultValue
	}
}

func defaultCallTimeoutSec(heartbeatSec int) int {
	if heartbeatSec <= 0 {
		heartbeatSec = defaultHeartbeatIntervalSec
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

func parseComputerUseWhitelistMode(raw string) string {
	mode := strings.ToLower(strings.TrimSpace(raw))
	switch mode {
	case computerUseWhitelistModePrefix, computerUseWhitelistModeExact, computerUseWhitelistModeAllowAll:
		return mode
	default:
		return computerUseWhitelistModeExact
	}
}

func parseComputerUseWhitelist(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}

	decoded := []string{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return []string{}
	}

	result := make([]string, 0, len(decoded))
	seen := make(map[string]struct{}, len(decoded))
	for _, entry := range decoded {
		value := strings.TrimSpace(entry)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}

	return result
}

func parseReadImageAllowedPaths(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}

	decoded := []string{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return []string{}
	}

	result := make([]string, 0, len(decoded))
	seen := make(map[string]struct{}, len(decoded))
	for _, entry := range decoded {
		value := strings.TrimSpace(entry)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}

	return result
}
