package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultConsoleTarget     = "127.0.0.1:50051"
	defaultHeartbeatInterval = 5
	defaultHeartbeatJitter   = 20
	defaultCallTimeout       = 3
	defaultExecutorKind      = "docker"
	defaultWorkerVersion     = "dev"
)

type Config struct {
	ConsoleGRPCTarget string
	WorkerID          string
	WorkerSecret      string
	HeartbeatInterval time.Duration
	HeartbeatJitter   int
	CallTimeout       time.Duration
	NodeName          string
	ExecutorKind      string
	Version           string
	Labels            map[string]string
}

func Load() Config {
	heartbeatSec := parsePositiveIntEnv("WORKER_HEARTBEAT_INTERVAL_SEC", defaultHeartbeatInterval)
	heartbeatJitter := parsePercentEnv("WORKER_HEARTBEAT_JITTER_PCT", defaultHeartbeatJitter)
	callTimeoutSec := parsePositiveIntEnv("WORKER_CALL_TIMEOUT_SEC", defaultCallTimeout)

	labelsCSV := os.Getenv("WORKER_LABELS")

	return Config{
		ConsoleGRPCTarget: getEnv("WORKER_CONSOLE_GRPC_TARGET", defaultConsoleTarget),
		WorkerID:          strings.TrimSpace(os.Getenv("WORKER_ID")),
		WorkerSecret:      strings.TrimSpace(os.Getenv("WORKER_SECRET")),
		HeartbeatInterval: time.Duration(heartbeatSec) * time.Second,
		HeartbeatJitter:   heartbeatJitter,
		CallTimeout:       time.Duration(callTimeoutSec) * time.Second,
		NodeName:          os.Getenv("WORKER_NODE_NAME"),
		ExecutorKind:      defaultExecutorKind,
		Version:           getEnv("WORKER_VERSION", defaultWorkerVersion),
		Labels:            parseLabels(labelsCSV),
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
