package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPAddr             = ":8089"
	defaultGRPCAddr             = ":50051"
	defaultOfflineTTLSec        = 15
	defaultReplayWindowSec      = 60
	defaultHeartbeatIntervalSec = 5
	defaultDBPath               = "./db/onlyboxes-console.db"
	defaultDBBusyTimeoutMS      = 5000
	defaultTaskRetentionDays    = 30
)

type Config struct {
	HTTPAddr             string
	GRPCAddr             string
	OfflineTTL           time.Duration
	ReplayWindow         time.Duration
	HeartbeatIntervalSec int32
	DashboardUsername    string
	DashboardPassword    string
	DBPath               string
	DBBusyTimeoutMS      int
	HashKey              string
	TaskRetentionDays    int
	EnableRegistration   bool
}

func Load() Config {
	offlineTTLSec := parsePositiveIntEnv("CONSOLE_OFFLINE_TTL_SEC", defaultOfflineTTLSec)
	replayWindowSec := parsePositiveIntEnv("CONSOLE_REPLAY_WINDOW_SEC", defaultReplayWindowSec)
	heartbeatIntervalSec := parsePositiveIntEnv("CONSOLE_HEARTBEAT_INTERVAL_SEC", defaultHeartbeatIntervalSec)
	dbBusyTimeoutMS := parsePositiveIntEnv("CONSOLE_DB_BUSY_TIMEOUT_MS", defaultDBBusyTimeoutMS)
	taskRetentionDays := parsePositiveIntEnv("CONSOLE_TASK_RETENTION_DAYS", defaultTaskRetentionDays)

	return Config{
		HTTPAddr:             getEnv("CONSOLE_HTTP_ADDR", defaultHTTPAddr),
		GRPCAddr:             getEnv("CONSOLE_GRPC_ADDR", defaultGRPCAddr),
		OfflineTTL:           time.Duration(offlineTTLSec) * time.Second,
		ReplayWindow:         time.Duration(replayWindowSec) * time.Second,
		HeartbeatIntervalSec: int32(heartbeatIntervalSec),
		DashboardUsername:    os.Getenv("CONSOLE_DASHBOARD_USERNAME"),
		DashboardPassword:    os.Getenv("CONSOLE_DASHBOARD_PASSWORD"),
		DBPath:               getEnv("CONSOLE_DB_PATH", defaultDBPath),
		DBBusyTimeoutMS:      dbBusyTimeoutMS,
		HashKey:              os.Getenv("CONSOLE_HASH_KEY"),
		TaskRetentionDays:    taskRetentionDays,
		EnableRegistration:   parseBoolEnv("CONSOLE_ENABLE_REGISTRATION", false),
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
