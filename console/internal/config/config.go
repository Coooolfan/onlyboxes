package config

import (
	"encoding/json"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPAddr                = ":8089"
	defaultGRPCAddr                = ":50051"
	defaultOfflineTTLSec           = 15
	defaultHeartbeatIntervalSec    = 5
	defaultDBPath                  = "./db/onlyboxes-console.db"
	defaultDBBusyTimeoutMS         = 5000
	defaultTaskRetentionDays       = 30
	defaultExportUploadTTLSec      = 15 * 60
	defaultExportDownloadTTLSec    = 60 * 60
	defaultMCPTokenQueryParam      = "token"
	defaultProxyRouteTTLSec        = 24 * 60 * 60
	defaultProxyRouteKeyLength     = 26
	minProxyRouteKeyLength         = 8
	maxProxyRouteKeyLength         = 26
	defaultProxyRouteMaxPerAccount = 16
	defaultProxyRouteMaxPerSession = 2
	defaultProxyWorkerPort         = 8091
	defaultProxyPublicScheme       = "https"
	defaultProxyDirectDomain       = "e2b.app"
	defaultLogLevel                = "info"
	defaultLogFormat               = "json"
	defaultLogAddSource            = false
)

type Config struct {
	ConfigFile                string
	HTTPAddr                  string
	GRPCAddr                  string
	OfflineTTL                time.Duration
	HeartbeatIntervalSec      int32
	DashboardUsername         string
	DashboardPassword         string
	InitialAdminAPIKey        string
	JITSigningKey             string
	DashboardJITSigningKey    string
	DBPath                    string
	DBBusyTimeoutMS           int
	HashKey                   string
	TaskRetentionDays         int
	ExportFileEndpoint        string
	ExportFileRegion          string
	ExportFileBucketName      string
	ExportFilePrefix          string
	ExportFileAK              string
	ExportFileSK              string
	ExportFileUploadTTL       time.Duration
	ExportFileDownloadTTL     time.Duration
	ExportReturnSchema        string
	EnableRegistration        bool
	HiddenTools               map[string]bool
	MCPTokenQueryParam        string
	ProxyEnabled              bool
	ProxyPublicBaseDomain     string
	ProxyPublicScheme         string
	ProxyInternalAuthToken    string
	ProxyAllowedWorkerCIDRs   []netip.Prefix
	ProxyAllowedWorkerPorts   []uint16
	ProxyAllowedDirectDomains []string
	ProxyRouteTTL             time.Duration
	ProxyRouteKeyLength       int
	ProxyRouteMaxPerAccount   int
	ProxyRouteMaxPerSession   int
	LogLevel                  string
	LogFormat                 string
	LogAddSource              bool
}

func Load() Config {
	src := newSource()

	offlineTTLSec := src.positiveInt("CONSOLE_OFFLINE_TTL_SEC", defaultOfflineTTLSec)
	heartbeatIntervalSec := src.positiveInt("CONSOLE_HEARTBEAT_INTERVAL_SEC", defaultHeartbeatIntervalSec)
	dbBusyTimeoutMS := src.positiveInt("CONSOLE_DB_BUSY_TIMEOUT_MS", defaultDBBusyTimeoutMS)
	taskRetentionDays := src.positiveInt("CONSOLE_TASK_RETENTION_DAYS", defaultTaskRetentionDays)
	exportUploadTTLSec := src.positiveInt("CONSOLE_EXPORT_FILE_UPLOAD_PRESIGN_TTL_SEC", defaultExportUploadTTLSec)
	exportDownloadTTLSec := src.positiveInt("CONSOLE_EXPORT_FILE_DOWNLOAD_PRESIGN_TTL_SEC", defaultExportDownloadTTLSec)
	proxyRouteTTLSec := src.positiveInt("CONSOLE_PROXY_ROUTE_TTL_SEC", defaultProxyRouteTTLSec)
	proxyRouteKeyLength := src.boundedInt("CONSOLE_PROXY_ROUTE_KEY_LENGTH", defaultProxyRouteKeyLength, minProxyRouteKeyLength, maxProxyRouteKeyLength)
	proxyRouteMaxPerAccount := src.positiveInt("CONSOLE_PROXY_ROUTE_MAX_PER_ACCOUNT", defaultProxyRouteMaxPerAccount)
	proxyRouteMaxPerSession := src.positiveInt("CONSOLE_PROXY_ROUTE_MAX_PER_SESSION", defaultProxyRouteMaxPerSession)

	return Config{
		ConfigFile:                src.Path(),
		HTTPAddr:                  src.stringValue("CONSOLE_HTTP_ADDR", defaultHTTPAddr),
		GRPCAddr:                  src.stringValue("CONSOLE_GRPC_ADDR", defaultGRPCAddr),
		OfflineTTL:                time.Duration(offlineTTLSec) * time.Second,
		HeartbeatIntervalSec:      int32(heartbeatIntervalSec),
		DashboardUsername:         src.get("CONSOLE_DASHBOARD_USERNAME"),
		DashboardPassword:         src.get("CONSOLE_DASHBOARD_PASSWORD"),
		InitialAdminAPIKey:        src.get("CONSOLE_INITIAL_ADMIN_API_KEY"),
		JITSigningKey:             src.get("CONSOLE_JIT_SIGNING_KEY"),
		DashboardJITSigningKey:    src.get("CONSOLE_DASHBOARD_JIT_SIGNING_KEY"),
		DBPath:                    src.stringValue("CONSOLE_DB_PATH", defaultDBPath),
		DBBusyTimeoutMS:           dbBusyTimeoutMS,
		HashKey:                   src.get("CONSOLE_HASH_KEY"),
		TaskRetentionDays:         taskRetentionDays,
		ExportFileEndpoint:        strings.TrimSpace(src.get("CONSOLE_EXPORT_FILE_ENDPOINT")),
		ExportFileRegion:          strings.TrimSpace(src.get("CONSOLE_EXPORT_FILE_REGION")),
		ExportFileBucketName:      strings.TrimSpace(src.get("CONSOLE_EXPORT_FILE_BUCKET_NAME")),
		ExportFilePrefix:          strings.TrimSpace(src.get("CONSOLE_EXPORT_FILE_EXPORT_PREFIX")),
		ExportFileAK:              strings.TrimSpace(src.get("CONSOLE_EXPORT_FILE_AK")),
		ExportFileSK:              strings.TrimSpace(src.get("CONSOLE_EXPORT_FILE_SK")),
		ExportFileUploadTTL:       time.Duration(exportUploadTTLSec) * time.Second,
		ExportFileDownloadTTL:     time.Duration(exportDownloadTTLSec) * time.Second,
		ExportReturnSchema:        src.exportReturnSchema("CONSOLE_EXPORT_RETURN_SCHEMA"),
		EnableRegistration:        src.boolValue("CONSOLE_ENABLE_REGISTRATION", false),
		HiddenTools:               src.stringSet("CONSOLE_HIDDEN_TOOLS"),
		MCPTokenQueryParam:        src.trimmedStringValue("CONSOLE_MCP_TOKEN_QUERY_PARAM", defaultMCPTokenQueryParam),
		ProxyEnabled:              src.boolValue("CONSOLE_PROXY_ENABLED", false),
		ProxyPublicBaseDomain:     strings.TrimSpace(strings.ToLower(src.get("CONSOLE_PROXY_PUBLIC_BASE_DOMAIN"))),
		ProxyPublicScheme:         strings.ToLower(src.trimmedStringValue("CONSOLE_PROXY_PUBLIC_SCHEME", defaultProxyPublicScheme)),
		ProxyInternalAuthToken:    strings.TrimSpace(src.get("CONSOLE_PROXY_INTERNAL_AUTH_TOKEN")),
		ProxyAllowedWorkerCIDRs:   parseCIDRList(src.get("CONSOLE_PROXY_ALLOWED_WORKER_CIDRS")),
		ProxyAllowedWorkerPorts:   parsePortList(src.get("CONSOLE_PROXY_ALLOWED_WORKER_PORTS")),
		ProxyAllowedDirectDomains: parseDomainList(src.trimmedStringValue("CONSOLE_PROXY_ALLOWED_DIRECT_DOMAINS", defaultProxyDirectDomain)),
		ProxyRouteTTL:             time.Duration(proxyRouteTTLSec) * time.Second,
		ProxyRouteKeyLength:       proxyRouteKeyLength,
		ProxyRouteMaxPerAccount:   proxyRouteMaxPerAccount,
		ProxyRouteMaxPerSession:   proxyRouteMaxPerSession,
		LogLevel:                  src.logLevel("CONSOLE_LOG_LEVEL", defaultLogLevel),
		LogFormat:                 src.logFormat("CONSOLE_LOG_FORMAT", defaultLogFormat),
		LogAddSource:              src.boolValue("CONSOLE_LOG_ADD_SOURCE", defaultLogAddSource),
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

func (s source) stringValue(key string, defaultValue string) string {
	value := s.get(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func (s source) trimmedStringValue(key string, defaultValue string) string {
	value := strings.TrimSpace(s.get(key))
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

func (s source) boundedInt(key string, defaultValue int, minValue int, maxValue int) int {
	value := strings.TrimSpace(s.get(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minValue || parsed > maxValue {
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

// stringSet accepts either the comma separated form used by environment
// variables or the JSON array produced by a TOML array.
func (s source) stringSet(key string) map[string]bool {
	raw := strings.TrimSpace(s.get(key))
	if raw == "" {
		return nil
	}

	items := []string{}
	if strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &items); err != nil {
			return nil
		}
	} else {
		items = strings.Split(raw, ",")
	}

	result := make(map[string]bool)
	for _, item := range items {
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

func parseCIDRList(raw string) []netip.Prefix {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}

	items := []string{}
	if strings.HasPrefix(value, "[") {
		if err := json.Unmarshal([]byte(value), &items); err != nil {
			return nil
		}
	} else {
		items = strings.Split(value, ",")
	}

	prefixes := make([]netip.Prefix, 0, len(items))
	seen := make(map[netip.Prefix]struct{}, len(items))
	for _, item := range items {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(item))
		if err != nil {
			continue
		}
		prefix = prefix.Masked()
		if _, exists := seen[prefix]; exists {
			continue
		}
		seen[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

func parsePortList(raw string) []uint16 {
	value := strings.TrimSpace(raw)
	if value == "" {
		return []uint16{defaultProxyWorkerPort}
	}

	items := []int{}
	if strings.HasPrefix(value, "[") {
		if err := json.Unmarshal([]byte(value), &items); err != nil {
			return nil
		}
	} else {
		parts := strings.Split(value, ",")
		items = make([]int, 0, len(parts))
		for _, part := range parts {
			port, err := strconv.Atoi(strings.TrimSpace(part))
			if err == nil {
				items = append(items, port)
			}
		}
	}

	ports := make([]uint16, 0, len(items))
	seen := make(map[uint16]struct{}, len(items))
	for _, item := range items {
		if item < 1 || item > 65535 {
			continue
		}
		port := uint16(item)
		if _, exists := seen[port]; exists {
			continue
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
	}
	if len(ports) == 0 {
		return nil
	}
	return ports
}

func parseDomainList(raw string) []string {
	value := strings.TrimSpace(raw)
	items := strings.Split(value, ",")
	if strings.HasPrefix(value, "[") {
		if err := json.Unmarshal([]byte(value), &items); err != nil {
			return nil
		}
	}
	result := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(item)), ".")
		if domain == "" || strings.ContainsAny(domain, "/:@?#*") {
			continue
		}
		if _, exists := seen[domain]; exists {
			continue
		}
		seen[domain] = struct{}{}
		result = append(result, domain)
	}
	return result
}

func normalizeToolKey(value string) string {
	return strings.TrimSpace(strings.ToLower(value))
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

func (s source) exportReturnSchema(key string) string {
	value := strings.TrimSpace(strings.ToUpper(s.get(key)))
	switch value {
	case "ALL", "SIGNED_URL", "OBJECTKEY":
		return value
	default:
		return "ALL"
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
