package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("CONSOLE_HTTP_ADDR", "")
	t.Setenv("CONSOLE_GRPC_ADDR", "")
	t.Setenv("CONSOLE_OFFLINE_TTL_SEC", "")
	t.Setenv("CONSOLE_REPLAY_WINDOW_SEC", "")
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
	if cfg.ReplayWindow != time.Duration(defaultReplayWindowSec)*time.Second {
		t.Fatalf("unexpected ReplayWindow: %s", cfg.ReplayWindow)
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
	t.Setenv("CONSOLE_REPLAY_WINDOW_SEC", "120")
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
	if cfg.ReplayWindow != 120*time.Second {
		t.Fatalf("expected ReplayWindow=120s, got %s", cfg.ReplayWindow)
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
	t.Setenv("CONSOLE_REPLAY_WINDOW_SEC", "not-a-number")
	t.Setenv("CONSOLE_HEARTBEAT_INTERVAL_SEC", "0")
	t.Setenv("CONSOLE_EXPORT_FILE_UPLOAD_PRESIGN_TTL_SEC", "0")
	t.Setenv("CONSOLE_EXPORT_FILE_DOWNLOAD_PRESIGN_TTL_SEC", "not-a-number")

	cfg := Load()
	if cfg.OfflineTTL != time.Duration(defaultOfflineTTLSec)*time.Second {
		t.Fatalf("expected default offline ttl, got %s", cfg.OfflineTTL)
	}
	if cfg.ReplayWindow != time.Duration(defaultReplayWindowSec)*time.Second {
		t.Fatalf("expected default replay window, got %s", cfg.ReplayWindow)
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

func TestLoadMCPToolOverrides_NoEnv_Nil(t *testing.T) {
	cfg := Load()
	if cfg.MCPToolOverrides != nil {
		t.Fatalf("expected nil MCPToolOverrides when no env set, got %#v", cfg.MCPToolOverrides)
	}
}

func TestLoadMCPToolOverrides_Description_Override(t *testing.T) {
	t.Setenv("CONSOLE_MCP_TOOL_ECHO_DESCRIPTION", "custom echo")
	cfg := Load()
	o, ok := cfg.MCPToolOverrides["echo"]
	if !ok {
		t.Fatalf("expected echo override, got %#v", cfg.MCPToolOverrides)
	}
	if o.Description == nil || *o.Description != "custom echo" {
		t.Fatalf("expected description=custom echo, got %#v", o.Description)
	}
	if o.Title != nil {
		t.Fatalf("expected nil Title, got %#v", o.Title)
	}
}

func TestLoadMCPToolOverrides_Title_Override(t *testing.T) {
	t.Setenv("CONSOLE_MCP_TOOL_PYTHON_EXEC_TITLE", "Py Runner")
	cfg := Load()
	o := cfg.MCPToolOverrides["pythonExec"]
	if o.Title == nil || *o.Title != "Py Runner" {
		t.Fatalf("expected Title=Py Runner, got %#v", o.Title)
	}
}

func TestLoadMCPToolOverrides_ParamDescription_Override(t *testing.T) {
	t.Setenv("CONSOLE_MCP_TOOL_TERMINAL_EXEC_PARAM_SESSION_ID_DESCRIPTION", "session token")
	cfg := Load()
	o := cfg.MCPToolOverrides["terminalExec"]
	got, ok := o.ParamDescriptions["session_id"]
	if !ok {
		t.Fatalf("expected session_id param override, got %#v", o.ParamDescriptions)
	}
	if got == nil || *got != "session token" {
		t.Fatalf("expected session_id=session token, got %#v", got)
	}
}

func TestLoadMCPToolOverrides_ParamHidden_EmptyString(t *testing.T) {
	t.Setenv("CONSOLE_MCP_TOOL_TERMINAL_EXEC_PARAM_SESSION_ID_DESCRIPTION", "")
	cfg := Load()
	o := cfg.MCPToolOverrides["terminalExec"]
	got, ok := o.ParamDescriptions["session_id"]
	if !ok {
		t.Fatalf("expected session_id override present, got %#v", o.ParamDescriptions)
	}
	if got == nil {
		t.Fatalf("expected non-nil pointer, got nil (means env not detected)")
	}
	if *got != "" {
		t.Fatalf("expected empty string for hidden, got %q", *got)
	}
}

func TestLoadMCPToolOverrides_Name_Override(t *testing.T) {
	t.Setenv("CONSOLE_MCP_TOOL_PYTHON_EXEC_NAME", "py")
	cfg := Load()
	o, ok := cfg.MCPToolOverrides["pythonExec"]
	if !ok {
		t.Fatalf("expected pythonExec override, got %#v", cfg.MCPToolOverrides)
	}
	if o.Name == nil || *o.Name != "py" {
		t.Fatalf("expected Name=py, got %#v", o.Name)
	}
	if o.Title != nil || o.Description != nil {
		t.Fatalf("expected only Name to be set, got %#v", o)
	}
}

func TestLoadMCPToolOverrides_NameEmptyString_Preserved(t *testing.T) {
	// Empty string is preserved in config; the handler layer decides the
	// fallback+warn behavior.
	t.Setenv("CONSOLE_MCP_TOOL_ECHO_NAME", "")
	cfg := Load()
	o := cfg.MCPToolOverrides["echo"]
	if o.Name == nil {
		t.Fatalf("expected non-nil Name pointer (env set to empty)")
	}
	if *o.Name != "" {
		t.Fatalf("expected empty string, got %q", *o.Name)
	}
}

func TestLoadMCPToolOverrides_DescriptionEmptyString_Preserved(t *testing.T) {
	// Empty string is preserved in config; the handler layer decides the
	// fallback+warn for Title/Description.
	t.Setenv("CONSOLE_MCP_TOOL_ECHO_DESCRIPTION", "")
	cfg := Load()
	o := cfg.MCPToolOverrides["echo"]
	if o.Description == nil {
		t.Fatalf("expected non-nil Description pointer (env set to empty)")
	}
	if *o.Description != "" {
		t.Fatalf("expected empty string, got %q", *o.Description)
	}
}

func TestToolNameToEnvSegment(t *testing.T) {
	cases := map[string]string{
		"echo":         "ECHO",
		"pythonExec":   "PYTHON_EXEC",
		"terminalExec": "TERMINAL_EXEC",
		"computerUse":  "COMPUTER_USE",
		"readImage":    "READ_IMAGE",
		"exportFile":   "EXPORT_FILE",
	}
	for in, want := range cases {
		if got := toolNameToEnvSegment(in); got != want {
			t.Errorf("toolNameToEnvSegment(%q)=%q want %q", in, got, want)
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
