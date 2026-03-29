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
	t.Setenv("CONSOLE_HIDDEN_TOOLS", " pythonexec, readimage , TERMINALEXEC ,, ")

	cfg := Load()

	expected := map[string]bool{
		"pythonexec":   true,
		"readimage":    true,
		"terminalexec": true,
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
