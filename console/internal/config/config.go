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
	ExportReturnSchema    string
	EnableRegistration    bool
	HiddenTools           map[string]bool
	MCPToolOverrides      map[string]MCPToolOverride
	LogLevel              string
	LogFormat             string
	LogAddSource          bool
}

// MCPToolOverride holds optional env-driven overrides for a single MCP tool's
// Title, Description, and per-parameter descriptions.
//
// Pointer semantics:
//   - nil       → env not set, use built-in default.
//   - non-nil   → env set. For Title/Description: empty string is treated as
//                invalid (fallback + warn). For ParamDescriptions: empty
//                string means "hide this parameter from tools/list inputSchema".
type MCPToolOverride struct {
	Title             *string
	Description       *string
	ParamDescriptions map[string]*string
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
		ExportReturnSchema:    parseExportReturnSchemaEnv("CONSOLE_EXPORT_RETURN_SCHEMA"),
		EnableRegistration:    parseBoolEnv("CONSOLE_ENABLE_REGISTRATION", false),
		HiddenTools:           parseStringSetEnv("CONSOLE_HIDDEN_TOOLS"),
		MCPToolOverrides:      loadMCPToolOverrides(),
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

func parseExportReturnSchemaEnv(key string) string {
	value := strings.TrimSpace(strings.ToUpper(os.Getenv(key)))
	switch value {
	case "ALL", "SIGNED_URL", "OBJECTKEY":
		return value
	default:
		return "ALL"
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

// mcpToolParamCatalog enumerates every (toolName, paramName) pair that participates
// in env-driven override. Tool names match those registered in NewMCPHandler;
// param names match the snake_case JSON field keys in each input schema.
//
// Keeping this catalog in config (rather than httpapi) avoids an import cycle
// and localizes the mapping used to derive env var names.
var mcpToolParamCatalog = []struct {
	ToolName string
	Params   []string
}{
	{"echo", []string{"message", "timeout_ms"}},
	{"pythonExec", []string{"code", "timeout_ms"}},
	{"terminalExec", []string{"command", "session_id", "create_if_missing", "lease_ttl_sec", "timeout_ms"}},
	{"computerUse", []string{"command", "timeout_ms", "request_id"}},
	{"readImage", []string{"session_id", "file_path", "timeout_ms"}},
	{"exportFile", []string{"session_id", "file_path", "timeout_ms"}},
}

// loadMCPToolOverrides reads env vars of the form:
//   CONSOLE_MCP_TOOL_<TOOL>_TITLE
//   CONSOLE_MCP_TOOL_<TOOL>_DESCRIPTION
//   CONSOLE_MCP_TOOL_<TOOL>_PARAM_<PARAM>_DESCRIPTION
// where <TOOL> is the camelCase tool name translated to UPPER_SNAKE (e.g.
// pythonExec → PYTHON_EXEC) and <PARAM> is the snake_case param name uppercased
// (e.g. session_id → SESSION_ID).
//
// It uses os.LookupEnv so that an explicitly empty string is distinguishable
// from an unset variable.
func loadMCPToolOverrides() map[string]MCPToolOverride {
	result := make(map[string]MCPToolOverride)
	for _, entry := range mcpToolParamCatalog {
		toolEnv := toolNameToEnvSegment(entry.ToolName)
		override := MCPToolOverride{}
		if v, ok := os.LookupEnv("CONSOLE_MCP_TOOL_" + toolEnv + "_TITLE"); ok {
			s := v
			override.Title = &s
		}
		if v, ok := os.LookupEnv("CONSOLE_MCP_TOOL_" + toolEnv + "_DESCRIPTION"); ok {
			s := v
			override.Description = &s
		}
		params := make(map[string]*string)
		for _, param := range entry.Params {
			paramEnv := paramNameToEnvSegment(param)
			if v, ok := os.LookupEnv("CONSOLE_MCP_TOOL_" + toolEnv + "_PARAM_" + paramEnv + "_DESCRIPTION"); ok {
				s := v
				params[param] = &s
			}
		}
		if len(params) > 0 {
			override.ParamDescriptions = params
		}
		if override.Title != nil || override.Description != nil || override.ParamDescriptions != nil {
			result[entry.ToolName] = override
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// toolNameToEnvSegment converts a camelCase tool name (e.g. "pythonExec") to
// the UPPER_SNAKE_CASE segment used in env variable names ("PYTHON_EXEC").
func toolNameToEnvSegment(name string) string {
	var b strings.Builder
	for i, r := range name {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r - 32)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// paramNameToEnvSegment converts a snake_case param name (e.g. "session_id")
// to the UPPER_SNAKE_CASE segment used in env variable names ("SESSION_ID").
func paramNameToEnvSegment(name string) string {
	return strings.ToUpper(name)
}
