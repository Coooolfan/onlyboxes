package config

import (
	"os"
	"strconv"
	"time"
)

const (
	defaultHTTPAddr              = ":8089"
	defaultGRPCAddr              = ":50051"
	defaultOfflineTTLSec         = 15
	defaultReplayWindowSec       = 60
	defaultHeartbeatIntervalSec  = 5
	defaultWorkerCredentialsFile = "./worker-credentials.json"
)

type Config struct {
	HTTPAddr              string
	GRPCAddr              string
	OfflineTTL            time.Duration
	ReplayWindow          time.Duration
	HeartbeatIntervalSec  int32
	WorkerMaxCount        int
	WorkerCredentialsFile string
	DashboardUsername     string
	DashboardPassword     string
}

func Load() Config {
	offlineTTLSec := parsePositiveIntEnv("CONSOLE_OFFLINE_TTL_SEC", defaultOfflineTTLSec)
	replayWindowSec := parsePositiveIntEnv("CONSOLE_REPLAY_WINDOW_SEC", defaultReplayWindowSec)
	heartbeatIntervalSec := parsePositiveIntEnv("CONSOLE_HEARTBEAT_INTERVAL_SEC", defaultHeartbeatIntervalSec)
	workerMaxCount := parsePositiveIntEnv("CONSOLE_WORKER_MAX_COUNT", 0)

	return Config{
		HTTPAddr:              getEnv("CONSOLE_HTTP_ADDR", defaultHTTPAddr),
		GRPCAddr:              getEnv("CONSOLE_GRPC_ADDR", defaultGRPCAddr),
		OfflineTTL:            time.Duration(offlineTTLSec) * time.Second,
		ReplayWindow:          time.Duration(replayWindowSec) * time.Second,
		HeartbeatIntervalSec:  int32(heartbeatIntervalSec),
		WorkerMaxCount:        workerMaxCount,
		WorkerCredentialsFile: getEnv("CONSOLE_WORKER_CREDENTIALS_FILE", defaultWorkerCredentialsFile),
		DashboardUsername:     os.Getenv("CONSOLE_DASHBOARD_USERNAME"),
		DashboardPassword:     os.Getenv("CONSOLE_DASHBOARD_PASSWORD"),
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
