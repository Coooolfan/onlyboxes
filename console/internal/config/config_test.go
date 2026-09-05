package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("CONSOLE_HTTP_ADDR", "")
	t.Setenv("CONSOLE_GRPC_ADDR", "")
	t.Setenv("CONSOLE_OFFLINE_TTL_SEC", "")
	t.Setenv("CONSOLE_HEARTBEAT_INTERVAL_SEC", "")
	t.Setenv("CONSOLE_DB_PATH", "")
	t.Setenv("CONSOLE_DASHBOARD_USERNAME", "")
	t.Setenv("CONSOLE_DASHBOARD_PASSWORD", "")
	t.Setenv("CONSOLE_INITIAL_ADMIN_API_KEY", "")
	t.Setenv("CONSOLE_JIT_SIGNING_KEY", "")
	t.Setenv("CONSOLE_EXPORT_FILE_ENDPOINT", "")
	t.Setenv("CONSOLE_EXPORT_FILE_REGION", "")
	t.Setenv("CONSOLE_EXPORT_FILE_BUCKET_NAME", "")
	t.Setenv("CONSOLE_EXPORT_FILE_EXPORT_PREFIX", "")
	t.Setenv("CONSOLE_EXPORT_FILE_AK", "")
	t.Setenv("CONSOLE_EXPORT_FILE_SK", "")
	t.Setenv("CONSOLE_EXPORT_FILE_UPLOAD_PRESIGN_TTL_SEC", "")
	t.Setenv("CONSOLE_EXPORT_FILE_DOWNLOAD_PRESIGN_TTL_SEC", "")
	t.Setenv("CONSOLE_ENABLE_REGISTRATION", "")
	t.Setenv("CONSOLE_MCP_TOKEN_QUERY_PARAM", "")
	t.Setenv("CONSOLE_PROXY_ENABLED", "")
	t.Setenv("CONSOLE_PROXY_PUBLIC_BASE_DOMAIN", "")
	t.Setenv("CONSOLE_PROXY_PUBLIC_SCHEME", "")
	t.Setenv("CONSOLE_PROXY_INTERNAL_AUTH_TOKEN", "")
	t.Setenv("CONSOLE_PROXY_ALLOWED_WORKER_CIDRS", "")
	t.Setenv("CONSOLE_PROXY_ALLOWED_WORKER_PORTS", "")
	t.Setenv("CONSOLE_PROXY_ROUTE_TTL_SEC", "")
	t.Setenv("CONSOLE_PROXY_ROUTE_KEY_LENGTH", "")
	t.Setenv("CONSOLE_PROXY_ROUTE_MAX_PER_ACCOUNT", "")
	t.Setenv("CONSOLE_PROXY_ROUTE_MAX_PER_SESSION", "")
	t.Setenv("CONSOLE_LOG_LEVEL", "")
	t.Setenv("CONSOLE_LOG_FORMAT", "")
	t.Setenv("CONSOLE_LOG_ADD_SOURCE", "")

	cfg := Load()
	if cfg.HTTPAddr != defaultHTTPAddr {
		t.Fatalf("expected HTTPAddr=%q, got %q", defaultHTTPAddr, cfg.HTTPAddr)
	}
	if cfg.GRPCAddr != defaultGRPCAddr {
		t.Fatalf("expected GRPCAddr=%q, got %q", defaultGRPCAddr, cfg.GRPCAddr)
	}
	if cfg.OfflineTTL != time.Duration(defaultOfflineTTLSec)*time.Second {
		t.Fatalf("unexpected OfflineTTL: %s", cfg.OfflineTTL)
	}
	if cfg.HeartbeatIntervalSec != int32(defaultHeartbeatIntervalSec) {
		t.Fatalf("unexpected HeartbeatIntervalSec: %d", cfg.HeartbeatIntervalSec)
	}
	if cfg.DBPath != defaultDBPath {
		t.Fatalf("expected DBPath=%q, got %q", defaultDBPath, cfg.DBPath)
	}
	if cfg.DashboardUsername != "" || cfg.DashboardPassword != "" {
		t.Fatalf("expected empty dashboard credentials, got username=%q password=%q", cfg.DashboardUsername, cfg.DashboardPassword)
	}
	if cfg.InitialAdminAPIKey != "" {
		t.Fatalf("expected empty initial admin api key by default, got %q", cfg.InitialAdminAPIKey)
	}
	if cfg.JITSigningKey != "" {
		t.Fatalf("expected empty jit signing key by default, got %q", cfg.JITSigningKey)
	}
	if cfg.ExportFileEnabled() {
		t.Fatalf("expected exportFile disabled by default")
	}
	if cfg.ExportFileUploadTTL != time.Duration(defaultExportUploadTTLSec)*time.Second {
		t.Fatalf("expected default export upload ttl, got %s", cfg.ExportFileUploadTTL)
	}
	if cfg.ExportFileDownloadTTL != time.Duration(defaultExportDownloadTTLSec)*time.Second {
		t.Fatalf("expected default export download ttl, got %s", cfg.ExportFileDownloadTTL)
	}
	if cfg.EnableRegistration {
		t.Fatalf("expected registration disabled by default")
	}
	if cfg.MCPTokenQueryParam != defaultMCPTokenQueryParam {
		t.Fatalf("expected MCPTokenQueryParam=%q, got %q", defaultMCPTokenQueryParam, cfg.MCPTokenQueryParam)
	}
	if cfg.ProxyEnabled || cfg.ProxyPublicBaseDomain != "" || cfg.ProxyInternalAuthToken != "" || len(cfg.ProxyAllowedWorkerCIDRs) != 0 {
		t.Fatalf("expected proxy disabled by default, got %#v", cfg)
	}
	if cfg.ProxyPublicScheme != defaultProxyPublicScheme {
		t.Fatalf("expected default proxy public scheme %q, got %q", defaultProxyPublicScheme, cfg.ProxyPublicScheme)
	}
	if len(cfg.ProxyAllowedWorkerPorts) != 1 || cfg.ProxyAllowedWorkerPorts[0] != defaultProxyWorkerPort {
		t.Fatalf("unexpected default proxy worker ports: %#v", cfg.ProxyAllowedWorkerPorts)
	}
	if len(cfg.ProxyAllowedDirectDomains) != 1 || cfg.ProxyAllowedDirectDomains[0] != defaultProxyDirectDomain {
		t.Fatalf("unexpected default direct proxy domains: %#v", cfg.ProxyAllowedDirectDomains)
	}
	if cfg.ProxyRouteTTL != time.Duration(defaultProxyRouteTTLSec)*time.Second {
		t.Fatalf("unexpected proxy route TTL: %s", cfg.ProxyRouteTTL)
	}
	if cfg.ProxyRouteKeyLength != defaultProxyRouteKeyLength {
		t.Fatalf("unexpected default proxy route key length: %d", cfg.ProxyRouteKeyLength)
	}
	if cfg.ProxyRouteMaxPerAccount != defaultProxyRouteMaxPerAccount || cfg.ProxyRouteMaxPerSession != defaultProxyRouteMaxPerSession {
		t.Fatalf("unexpected proxy route limits: account=%d session=%d", cfg.ProxyRouteMaxPerAccount, cfg.ProxyRouteMaxPerSession)
	}
	if cfg.LogLevel != defaultLogLevel {
		t.Fatalf("expected LogLevel=%q, got %q", defaultLogLevel, cfg.LogLevel)
	}
	if cfg.LogFormat != defaultLogFormat {
		t.Fatalf("expected LogFormat=%q, got %q", defaultLogFormat, cfg.LogFormat)
	}
	if cfg.LogAddSource != defaultLogAddSource {
		t.Fatalf("expected LogAddSource=%t, got %t", defaultLogAddSource, cfg.LogAddSource)
	}
}

func TestLoadReadsDashboardCredentialsAndDurations(t *testing.T) {
	t.Setenv("CONSOLE_DASHBOARD_USERNAME", "admin")
	t.Setenv("CONSOLE_DASHBOARD_PASSWORD", "secret")
	t.Setenv("CONSOLE_INITIAL_ADMIN_API_KEY", "obxk_testkey123")
	t.Setenv("CONSOLE_JIT_SIGNING_KEY", "jit-signing-secret")
	t.Setenv("CONSOLE_OFFLINE_TTL_SEC", "30")
	t.Setenv("CONSOLE_HEARTBEAT_INTERVAL_SEC", "10")
	t.Setenv("CONSOLE_DB_PATH", "/var/lib/onlyboxes/console.db")
	t.Setenv("CONSOLE_EXPORT_FILE_ENDPOINT", "https://minio.example.com")
	t.Setenv("CONSOLE_EXPORT_FILE_REGION", "cn-test-1")
	t.Setenv("CONSOLE_EXPORT_FILE_BUCKET_NAME", "exports")
	t.Setenv("CONSOLE_EXPORT_FILE_EXPORT_PREFIX", "prefix")
	t.Setenv("CONSOLE_EXPORT_FILE_AK", "ak-test")
	t.Setenv("CONSOLE_EXPORT_FILE_SK", "sk-test")
	t.Setenv("CONSOLE_EXPORT_FILE_UPLOAD_PRESIGN_TTL_SEC", "120")
	t.Setenv("CONSOLE_EXPORT_FILE_DOWNLOAD_PRESIGN_TTL_SEC", "900")
	t.Setenv("CONSOLE_ENABLE_REGISTRATION", "true")
	t.Setenv("CONSOLE_MCP_TOKEN_QUERY_PARAM", "access_token")
	t.Setenv("CONSOLE_PROXY_ENABLED", "true")
	t.Setenv("CONSOLE_PROXY_PUBLIC_BASE_DOMAIN", "Public-Preview.Example.COM")
	t.Setenv("CONSOLE_PROXY_PUBLIC_SCHEME", "HTTP")
	t.Setenv("CONSOLE_PROXY_INTERNAL_AUTH_TOKEN", "internal-proxy-secret")
	t.Setenv("CONSOLE_PROXY_ALLOWED_WORKER_CIDRS", "10.0.0.0/8, 2001:db8::/32, invalid,10.0.0.0/8")
	t.Setenv("CONSOLE_PROXY_ALLOWED_WORKER_PORTS", "8091, 18091,invalid,8091")
	t.Setenv("CONSOLE_PROXY_ALLOWED_DIRECT_DOMAINS", "e2b.app, Sandbox.Example.COM.,invalid/path,e2b.app")
	t.Setenv("CONSOLE_PROXY_ROUTE_TTL_SEC", "3600")
	t.Setenv("CONSOLE_PROXY_ROUTE_KEY_LENGTH", "8")
	t.Setenv("CONSOLE_PROXY_ROUTE_MAX_PER_ACCOUNT", "24")
	t.Setenv("CONSOLE_PROXY_ROUTE_MAX_PER_SESSION", "3")
	t.Setenv("CONSOLE_LOG_LEVEL", "debug")
	t.Setenv("CONSOLE_LOG_FORMAT", "text")
	t.Setenv("CONSOLE_LOG_ADD_SOURCE", "true")

	cfg := Load()
	if cfg.DashboardUsername != "admin" {
		t.Fatalf("expected username admin, got %q", cfg.DashboardUsername)
	}
	if cfg.DashboardPassword != "secret" {
		t.Fatalf("expected password secret, got %q", cfg.DashboardPassword)
	}
	if cfg.InitialAdminAPIKey != "obxk_testkey123" {
		t.Fatalf("expected initial admin api key %q, got %q", "obxk_testkey123", cfg.InitialAdminAPIKey)
	}
	if cfg.JITSigningKey != "jit-signing-secret" {
		t.Fatalf("expected JIT signing key %q, got %q", "jit-signing-secret", cfg.JITSigningKey)
	}
	if cfg.OfflineTTL != 30*time.Second {
		t.Fatalf("expected OfflineTTL=30s, got %s", cfg.OfflineTTL)
	}
	if cfg.HeartbeatIntervalSec != 10 {
		t.Fatalf("expected HeartbeatIntervalSec=10, got %d", cfg.HeartbeatIntervalSec)
	}
	if cfg.DBPath != "/var/lib/onlyboxes/console.db" {
		t.Fatalf("expected DBPath override to be used, got %q", cfg.DBPath)
	}
	if cfg.ExportFileEndpoint != "https://minio.example.com" {
		t.Fatalf("expected export endpoint override, got %q", cfg.ExportFileEndpoint)
	}
	if cfg.ExportFileRegion != "cn-test-1" {
		t.Fatalf("expected export region override, got %q", cfg.ExportFileRegion)
	}
	if cfg.ExportFileBucketName != "exports" {
		t.Fatalf("expected export bucket override, got %q", cfg.ExportFileBucketName)
	}
	if cfg.ExportFilePrefix != "prefix" {
		t.Fatalf("expected export prefix override, got %q", cfg.ExportFilePrefix)
	}
	if cfg.ExportFileAK != "ak-test" || cfg.ExportFileSK != "sk-test" {
		t.Fatalf("expected export credentials override, got ak=%q sk=%q", cfg.ExportFileAK, cfg.ExportFileSK)
	}
	if cfg.ExportFileUploadTTL != 120*time.Second {
		t.Fatalf("expected export upload ttl override, got %s", cfg.ExportFileUploadTTL)
	}
	if cfg.ExportFileDownloadTTL != 900*time.Second {
		t.Fatalf("expected export download ttl override, got %s", cfg.ExportFileDownloadTTL)
	}
	if !cfg.ExportFileEnabled() {
		t.Fatalf("expected exportFile enabled when all env vars are present")
	}
	if !cfg.EnableRegistration {
		t.Fatalf("expected registration enabled")
	}
	if cfg.MCPTokenQueryParam != "access_token" {
		t.Fatalf("expected MCPTokenQueryParam access_token, got %q", cfg.MCPTokenQueryParam)
	}
	if !cfg.ProxyEnabled || cfg.ProxyPublicBaseDomain != "public-preview.example.com" || cfg.ProxyPublicScheme != "http" || cfg.ProxyInternalAuthToken != "internal-proxy-secret" {
		t.Fatalf("unexpected proxy config: %#v", cfg)
	}
	if len(cfg.ProxyAllowedWorkerCIDRs) != 2 || cfg.ProxyAllowedWorkerCIDRs[0].String() != "10.0.0.0/8" || cfg.ProxyAllowedWorkerCIDRs[1].String() != "2001:db8::/32" {
		t.Fatalf("unexpected proxy worker CIDRs: %#v", cfg.ProxyAllowedWorkerCIDRs)
	}
	if len(cfg.ProxyAllowedWorkerPorts) != 2 || cfg.ProxyAllowedWorkerPorts[0] != 8091 || cfg.ProxyAllowedWorkerPorts[1] != 18091 {
		t.Fatalf("unexpected proxy worker ports: %#v", cfg.ProxyAllowedWorkerPorts)
	}
	if len(cfg.ProxyAllowedDirectDomains) != 2 || cfg.ProxyAllowedDirectDomains[0] != "e2b.app" || cfg.ProxyAllowedDirectDomains[1] != "sandbox.example.com" {
		t.Fatalf("unexpected direct proxy domains: %#v", cfg.ProxyAllowedDirectDomains)
	}
	if cfg.ProxyRouteTTL != time.Hour {
		t.Fatalf("expected proxy route TTL 1h, got %s", cfg.ProxyRouteTTL)
	}
	if cfg.ProxyRouteKeyLength != 8 {
		t.Fatalf("expected proxy route key length 8, got %d", cfg.ProxyRouteKeyLength)
	}
	if cfg.ProxyRouteMaxPerAccount != 24 || cfg.ProxyRouteMaxPerSession != 3 {
		t.Fatalf("unexpected proxy route limit overrides: account=%d session=%d", cfg.ProxyRouteMaxPerAccount, cfg.ProxyRouteMaxPerSession)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("expected LogLevel=debug, got %q", cfg.LogLevel)
	}
	if cfg.LogFormat != "text" {
		t.Fatalf("expected LogFormat=text, got %q", cfg.LogFormat)
	}
	if !cfg.LogAddSource {
		t.Fatalf("expected LogAddSource=true")
	}
}

func TestLoadFallsBackForInvalidNumericEnv(t *testing.T) {
	t.Setenv("CONSOLE_OFFLINE_TTL_SEC", "-1")
	t.Setenv("CONSOLE_HEARTBEAT_INTERVAL_SEC", "0")
	t.Setenv("CONSOLE_EXPORT_FILE_UPLOAD_PRESIGN_TTL_SEC", "0")
	t.Setenv("CONSOLE_EXPORT_FILE_DOWNLOAD_PRESIGN_TTL_SEC", "not-a-number")
	t.Setenv("CONSOLE_PROXY_ROUTE_TTL_SEC", "0")
	t.Setenv("CONSOLE_PROXY_ROUTE_KEY_LENGTH", "7")
	t.Setenv("CONSOLE_PROXY_ROUTE_MAX_PER_ACCOUNT", "-1")
	t.Setenv("CONSOLE_PROXY_ROUTE_MAX_PER_SESSION", "not-a-number")

	cfg := Load()
	if cfg.OfflineTTL != time.Duration(defaultOfflineTTLSec)*time.Second {
		t.Fatalf("expected default offline ttl, got %s", cfg.OfflineTTL)
	}
	if cfg.HeartbeatIntervalSec != int32(defaultHeartbeatIntervalSec) {
		t.Fatalf("expected default heartbeat interval, got %d", cfg.HeartbeatIntervalSec)
	}
	if cfg.ExportFileUploadTTL != time.Duration(defaultExportUploadTTLSec)*time.Second {
		t.Fatalf("expected default export upload ttl, got %s", cfg.ExportFileUploadTTL)
	}
	if cfg.ExportFileDownloadTTL != time.Duration(defaultExportDownloadTTLSec)*time.Second {
		t.Fatalf("expected default export download ttl, got %s", cfg.ExportFileDownloadTTL)
	}
	if cfg.ProxyRouteTTL != time.Duration(defaultProxyRouteTTLSec)*time.Second {
		t.Fatalf("expected default proxy route TTL, got %s", cfg.ProxyRouteTTL)
	}
	if cfg.ProxyRouteKeyLength != defaultProxyRouteKeyLength {
		t.Fatalf("expected default proxy route key length, got %d", cfg.ProxyRouteKeyLength)
	}
	if cfg.ProxyRouteMaxPerAccount != defaultProxyRouteMaxPerAccount || cfg.ProxyRouteMaxPerSession != defaultProxyRouteMaxPerSession {
		t.Fatalf("expected default proxy route limits, got account=%d session=%d", cfg.ProxyRouteMaxPerAccount, cfg.ProxyRouteMaxPerSession)
	}
}

func TestParseProxyWorkerPortsFailsClosedForExplicitInvalidList(t *testing.T) {
	for _, raw := range []string{"invalid,0,65536", `["8091"]`, "[]"} {
		if ports := parsePortList(raw); len(ports) != 0 {
			t.Fatalf("expected invalid explicit port list %q to yield no allowed ports, got %#v", raw, ports)
		}
	}
}

func TestLoadRegistrationFlagFallback(t *testing.T) {
	t.Setenv("CONSOLE_ENABLE_REGISTRATION", "not-a-bool")
	cfg := Load()
	if cfg.EnableRegistration {
		t.Fatalf("expected invalid bool value to fallback to false")
	}
}

func TestLoadLogConfigFallback(t *testing.T) {
	t.Setenv("CONSOLE_LOG_LEVEL", "verbose")
	t.Setenv("CONSOLE_LOG_FORMAT", "yaml")
	t.Setenv("CONSOLE_LOG_ADD_SOURCE", "not-a-bool")

	cfg := Load()
	if cfg.LogLevel != defaultLogLevel {
		t.Fatalf("expected LogLevel fallback=%q, got %q", defaultLogLevel, cfg.LogLevel)
	}
	if cfg.LogFormat != defaultLogFormat {
		t.Fatalf("expected LogFormat fallback=%q, got %q", defaultLogFormat, cfg.LogFormat)
	}
	if cfg.LogAddSource != defaultLogAddSource {
		t.Fatalf("expected LogAddSource fallback=%t, got %t", defaultLogAddSource, cfg.LogAddSource)
	}
}

func TestLoadNormalizesHiddenTools(t *testing.T) {
	t.Setenv("CONSOLE_HIDDEN_TOOLS", " pythonexec, readimage , TERMINALEXEC , exportfile ,, ")

	cfg := Load()

	expected := map[string]bool{
		"pythonexec":   true,
		"readimage":    true,
		"terminalexec": true,
		"exportfile":   true,
	}
	if len(cfg.HiddenTools) != len(expected) {
		t.Fatalf("expected %d hidden tools, got %d: %#v", len(expected), len(cfg.HiddenTools), cfg.HiddenTools)
	}
	for key := range expected {
		if !cfg.HiddenTools[key] {
			t.Fatalf("expected hidden tool key %q to be present in %#v", key, cfg.HiddenTools)
		}
	}
}

func TestExportFileEnabledRequiresAllFields(t *testing.T) {
	t.Setenv("CONSOLE_EXPORT_FILE_ENDPOINT", "https://minio.example.com")
	t.Setenv("CONSOLE_EXPORT_FILE_REGION", "cn-test-1")
	t.Setenv("CONSOLE_EXPORT_FILE_BUCKET_NAME", "exports")
	t.Setenv("CONSOLE_EXPORT_FILE_EXPORT_PREFIX", "prefix")
	t.Setenv("CONSOLE_EXPORT_FILE_AK", "ak-test")
	t.Setenv("CONSOLE_EXPORT_FILE_SK", "")

	cfg := Load()
	if cfg.ExportFileEnabled() {
		t.Fatalf("expected exportFile disabled when secret key is missing")
	}
}
