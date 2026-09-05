package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), configFileName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	t.Setenv(configFileEnvKey, path)
	return path
}

func TestLoadReadsConfigFile(t *testing.T) {
	path := writeConfigFile(t, `
http_addr = ":9000"
grpc_addr = ":9051"
db_path = "./db/custom.db"
offline_ttl_sec = 30
enable_registration = true
hidden_tools = ["echo", "pythonExec"]
mcp_token_query_param = "access_token"
log_level = "debug"
`)

	cfg := Load()
	if cfg.ConfigFile != path {
		t.Fatalf("expected config file path %q, got %q", path, cfg.ConfigFile)
	}
	if cfg.HTTPAddr != ":9000" || cfg.GRPCAddr != ":9051" {
		t.Fatalf("unexpected addrs %q/%q", cfg.HTTPAddr, cfg.GRPCAddr)
	}
	if cfg.DBPath != "./db/custom.db" {
		t.Fatalf("unexpected db path %q", cfg.DBPath)
	}
	if cfg.OfflineTTL != 30*time.Second {
		t.Fatalf("unexpected offline ttl %s", cfg.OfflineTTL)
	}
	if !cfg.EnableRegistration {
		t.Fatalf("expected registration enabled")
	}
	if !cfg.HiddenTools["echo"] || !cfg.HiddenTools["pythonexec"] {
		t.Fatalf("unexpected hidden tools %v", cfg.HiddenTools)
	}
	if cfg.MCPTokenQueryParam != "access_token" {
		t.Fatalf("unexpected mcp token query param %q", cfg.MCPTokenQueryParam)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("unexpected log level %q", cfg.LogLevel)
	}
}

func TestEnvOverridesConfigFile(t *testing.T) {
	writeConfigFile(t, `
http_addr = ":9000"
log_level = "debug"
`)
	t.Setenv("CONSOLE_HTTP_ADDR", ":9100")
	t.Setenv("CONSOLE_LOG_LEVEL", "warn")

	cfg := Load()
	if cfg.HTTPAddr != ":9100" {
		t.Fatalf("expected env override, got %q", cfg.HTTPAddr)
	}
	if cfg.LogLevel != "warn" {
		t.Fatalf("expected env override, got %q", cfg.LogLevel)
	}
}

func TestEmptyEnvOverridesConfigFile(t *testing.T) {
	writeConfigFile(t, `
http_addr = ":9000"
dashboard_password = "from-file"
hidden_tools = ["echo"]
`)
	t.Setenv("CONSOLE_HTTP_ADDR", "")
	t.Setenv("CONSOLE_DASHBOARD_PASSWORD", "")
	t.Setenv("CONSOLE_HIDDEN_TOOLS", "")

	cfg := Load()
	if cfg.HTTPAddr != defaultHTTPAddr {
		t.Fatalf("expected empty env to select the default address, got %q", cfg.HTTPAddr)
	}
	if cfg.DashboardPassword != "" {
		t.Fatalf("expected empty env to clear dashboard password, got %q", cfg.DashboardPassword)
	}
	if cfg.HiddenTools != nil {
		t.Fatalf("expected empty env to clear hidden tools, got %v", cfg.HiddenTools)
	}
}

func TestLoadKeepsDefaultsWithoutConfigFile(t *testing.T) {
	t.Setenv(configFileEnvKey, filepath.Join(t.TempDir(), "empty.toml"))
	if err := os.WriteFile(os.Getenv(configFileEnvKey), []byte(""), 0o600); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg := Load()
	if cfg.HTTPAddr != defaultHTTPAddr || cfg.GRPCAddr != defaultGRPCAddr {
		t.Fatalf("unexpected defaults %q/%q", cfg.HTTPAddr, cfg.GRPCAddr)
	}
	if cfg.MCPTokenQueryParam != defaultMCPTokenQueryParam {
		t.Fatalf("unexpected default mcp token query param %q", cfg.MCPTokenQueryParam)
	}
}
