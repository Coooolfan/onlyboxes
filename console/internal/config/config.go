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
	defaultExportUploadTTLSec   = 15 * 60
	defaultExportDownloadTTLSec = 60 * 60
	defaultLogLevel             = "info"
	defaultLogFormat            = "json"
	defaultLogAddSource         = false
)

type Config struct {
	HTTPAddr              string
	GRPCAddr              string
	OfflineTTL            time.Duration
	ReplayWindow          time.Duration
	HeartbeatIntervalSec  int32
	DashboardUsername     string
	DashboardPassword     string
	InitialAdminAPIKey    string
	JITSigningKey         string
	DBPath                string
	DBBusyTimeoutMS       int
	HashKey               string
	TaskRetentionDays     int
	ExportFileEndpoint    string
	ExportFileRegion      string
	ExportFileBucketName  string
	ExportFilePrefix      string
	ExportFileAK          string
	ExportFileSK          string
	ExportFileUploadTTL   time.Duration
	ExportFileDownloadTTL time.Duration
	EnableRegistration    bool
	HiddenTools           map[string]bool
	LogLevel              string
	LogFormat             string
	LogAddSource          bool
}

func Load() Config {
	offlineTTLSec := parsePositiveIntEnv("CONSOLE_OFFLINE_TTL_SEC", defaultOfflineTTLSec)
	replayWindowSec := parsePositiveIntEnv("CONSOLE_REPLAY_WINDOW_SEC", defaultReplayWindowSec)
	heartbeatIntervalSec := parsePositiveIntEnv("CONSOLE_HEARTBEAT_INTERVAL_SEC", defaultHeartbeatIntervalSec)
	dbBusyTimeoutMS := parsePositiveIntEnv("CONSOLE_DB_BUSY_TIMEOUT_MS", defaultDBBusyTimeoutMS)
	taskRetentionDays := parsePositiveIntEnv("CONSOLE_TASK_RETENTION_DAYS", defaultTaskRetentionDays)
	exportUploadTTLSec := parsePositiveIntEnv("CONSOLE_EXPORT_FILE_UPLOAD_PRESIGN_TTL_SEC", defaultExportUploadTTLSec)
	exportDownloadTTLSec := parsePositiveIntEnv("CONSOLE_EXPORT_FILE_DOWNLOAD_PRESIGN_TTL_SEC", defaultExportDownloadTTLSec)

	return Config{
		HTTPAddr:              getEnv("CONSOLE_HTTP_ADDR", defaultHTTPAddr),
		GRPCAddr:              getEnv("CONSOLE_GRPC_ADDR", defaultGRPCAddr),
		OfflineTTL:            time.Duration(offlineTTLSec) * time.Second,
		ReplayWindow:          time.Duration(replayWindowSec) * time.Second,
		HeartbeatIntervalSec:  int32(heartbeatIntervalSec),
		DashboardUsername:     os.Getenv("CONSOLE_DASHBOARD_USERNAME"),
		DashboardPassword:     os.Getenv("CONSOLE_DASHBOARD_PASSWORD"),
		InitialAdminAPIKey:    os.Getenv("CONSOLE_INITIAL_ADMIN_API_KEY"),
		JITSigningKey:         os.Getenv("CONSOLE_JIT_SIGNING_KEY"),
		DBPath:                getEnv("CONSOLE_DB_PATH", defaultDBPath),
		DBBusyTimeoutMS:       dbBusyTimeoutMS,
		HashKey:               os.Getenv("CONSOLE_HASH_KEY"),
		TaskRetentionDays:     taskRetentionDays,
		ExportFileEndpoint:    strings.TrimSpace(os.Getenv("CONSOLE_EXPORT_FILE_ENDPOINT")),
		ExportFileRegion:      strings.TrimSpace(os.Getenv("CONSOLE_EXPORT_FILE_REGION")),
		ExportFileBucketName:  strings.TrimSpace(os.Getenv("CONSOLE_EXPORT_FILE_BUCKET_NAME")),
		ExportFilePrefix:      strings.TrimSpace(os.Getenv("CONSOLE_EXPORT_FILE_EXPORT_PREFIX")),
		ExportFileAK:          strings.TrimSpace(os.Getenv("CONSOLE_EXPORT_FILE_AK")),
		ExportFileSK:          strings.TrimSpace(os.Getenv("CONSOLE_EXPORT_FILE_SK")),
		ExportFileUploadTTL:   time.Duration(exportUploadTTLSec) * time.Second,
		ExportFileDownloadTTL: time.Duration(exportDownloadTTLSec) * time.Second,
		EnableRegistration:    parseBoolEnv("CONSOLE_ENABLE_REGISTRATION", false),
		HiddenTools:           parseStringSetEnv("CONSOLE_HIDDEN_TOOLS"),
		LogLevel:              parseLogLevelEnv("CONSOLE_LOG_LEVEL", defaultLogLevel),
		LogFormat:             parseLogFormatEnv("CONSOLE_LOG_FORMAT", defaultLogFormat),
		LogAddSource:          parseBoolEnv("CONSOLE_LOG_ADD_SOURCE", defaultLogAddSource),
	}
}

func (c Config) ExportFileEnabled() bool {
	return c.ExportFileEndpoint != "" &&
		c.ExportFileRegion != "" &&
		c.ExportFileBucketName != "" &&
		c.ExportFilePrefix != "" &&
		c.ExportFileAK != "" &&
		c.ExportFileSK != ""
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

func parseStringSetEnv(key string) map[string]bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	result := make(map[string]bool)
	for _, item := range strings.Split(raw, ",") {
		item = normalizeToolKey(item)
		if item != "" {
			result[item] = true
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func normalizeToolKey(value string) string {
	return strings.TrimSpace(strings.ToLower(value))
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
