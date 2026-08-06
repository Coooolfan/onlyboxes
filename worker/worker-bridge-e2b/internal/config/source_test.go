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
	writeConfigFile(t, `
console_grpc_target = "10.0.0.1:50051"
console_insecure = true
id = "worker-1"
secret = "s3cret"
heartbeat_interval_sec = 7
e2b_api_key = "test-api-key"
e2b_python_template = "python-template"
e2b_terminal_template = "terminal-template"
e2b_request_timeout_sec = 12
terminal_max_active_sessions = 7
terminal_export_mode = "sandbox"
log_level = "debug"
log_add_source = true

[labels]
region = "cn"
owner = "team-a"
description = "gpu,shared"
`)

	cfg := Load()
	if cfg.ConsoleGRPCTarget != "10.0.0.1:50051" {
		t.Fatalf("unexpected console target %q", cfg.ConsoleGRPCTarget)
	}
	if cfg.ConsoleTLS {
		t.Fatalf("expected console TLS disabled by console_insecure=true")
	}
	if cfg.WorkerID != "worker-1" || cfg.WorkerSecret != "s3cret" {
		t.Fatalf("unexpected identity %q/%q", cfg.WorkerID, cfg.WorkerSecret)
	}
	if cfg.HeartbeatInterval != 7*time.Second {
		t.Fatalf("unexpected heartbeat interval %s", cfg.HeartbeatInterval)
	}
	if cfg.CallTimeout != 18*time.Second {
		t.Fatalf("expected dynamic call timeout 18s, got %s", cfg.CallTimeout)
	}
	if cfg.E2BAPIKey != "test-api-key" {
		t.Fatalf("unexpected E2B API key")
	}
	if cfg.E2BPythonTemplate != "python-template" || cfg.E2BTerminalTemplate != "terminal-template" {
		t.Fatalf("unexpected E2B templates %q/%q", cfg.E2BPythonTemplate, cfg.E2BTerminalTemplate)
	}
	if cfg.E2BRequestTimeout != 12*time.Second {
		t.Fatalf("unexpected E2B request timeout %s", cfg.E2BRequestTimeout)
	}
	if cfg.TerminalExportMode != "sandbox" {
		t.Fatalf("unexpected terminal export mode %q", cfg.TerminalExportMode)
	}
	if cfg.TerminalMaxActiveSessions != 7 {
		t.Fatalf("unexpected terminal max active sessions %d", cfg.TerminalMaxActiveSessions)
	}
	if cfg.LogLevel != "debug" || !cfg.LogAddSource {
		t.Fatalf("unexpected log config %q/%t", cfg.LogLevel, cfg.LogAddSource)
	}
	if cfg.Labels["region"] != "cn" || cfg.Labels["owner"] != "team-a" || cfg.Labels["description"] != "gpu,shared" {
		t.Fatalf("unexpected labels %v", cfg.Labels)
	}
}

func TestCloudDefaultsAndTerminalExportMode(t *testing.T) {
	writeConfigFile(t, `terminal_export_mode = "invalid"`)

	cfg := Load()
	if got := cfg.TerminalExportMode; got != defaultTerminalExportMode {
		t.Fatalf("expected default terminal export mode %q, got %q", defaultTerminalExportMode, got)
	}
	if got := cfg.TerminalSessionMaxInflight; got != defaultTerminalSessionInflight {
		t.Fatalf("expected default terminal session max inflight %d, got %d", defaultTerminalSessionInflight, got)
	}
	if got := cfg.TerminalMaxActiveSessions; got != defaultTerminalMaxActiveSessions {
		t.Fatalf("expected default terminal max active sessions %d, got %d", defaultTerminalMaxActiveSessions, got)
	}
	if cfg.TerminalLeaseMaxSec != defaultTerminalLeaseMax ||
		cfg.TerminalLeaseDefaultSec != defaultTerminalLeaseTTL ||
		cfg.TerminalOutputLimitBytes != defaultTerminalOutputMax {
		t.Fatalf(
			"unexpected terminal defaults: max_lease=%d default_lease=%d output_limit=%d",
			cfg.TerminalLeaseMaxSec,
			cfg.TerminalLeaseDefaultSec,
			cfg.TerminalOutputLimitBytes,
		)
	}
	if cfg.EchoMaxInflight != defaultEchoMaxInflight ||
		cfg.PythonExecMaxInflight != defaultPythonExecMaxInflight ||
		cfg.TerminalExecMaxInflight != defaultTerminalExecMaxInflight ||
		cfg.TerminalResourceMaxInflight != defaultResourceMaxInflight {
		t.Fatalf(
			"unexpected capability defaults: echo=%d python=%d terminal=%d resource=%d",
			cfg.EchoMaxInflight,
			cfg.PythonExecMaxInflight,
			cfg.TerminalExecMaxInflight,
			cfg.TerminalResourceMaxInflight,
		)
	}

	t.Setenv("WORKER_TERMINAL_EXPORT_MODE", "WORKER")
	if got := Load().TerminalExportMode; got != "worker" {
		t.Fatalf("expected case-insensitive worker mode, got %q", got)
	}
}

func TestLoadParsesTerminalMaxActiveSessions(t *testing.T) {
	t.Setenv("WORKER_TERMINAL_MAX_ACTIVE_SESSIONS", "0")
	if got := Load().TerminalMaxActiveSessions; got != 0 {
		t.Fatalf("expected zero to mean unlimited, got %d", got)
	}
	t.Setenv("WORKER_TERMINAL_MAX_ACTIVE_SESSIONS", "12")
	if got := Load().TerminalMaxActiveSessions; got != 12 {
		t.Fatalf("expected finite max active sessions 12, got %d", got)
	}
	t.Setenv("WORKER_TERMINAL_MAX_ACTIVE_SESSIONS", "-1")
	if got := Load().TerminalMaxActiveSessions; got != defaultTerminalMaxActiveSessions {
		t.Fatalf("expected invalid value to fall back to %d, got %d", defaultTerminalMaxActiveSessions, got)
	}
}

func TestEnvOverridesConfigFile(t *testing.T) {
	writeConfigFile(t, `
console_grpc_target = "10.0.0.1:50051"
log_level = "debug"
`)
	t.Setenv("WORKER_CONSOLE_GRPC_TARGET", "127.0.0.1:60051")
	t.Setenv("WORKER_LOG_LEVEL", "warn")

	cfg := Load()
	if cfg.ConsoleGRPCTarget != "127.0.0.1:60051" {
		t.Fatalf("expected env override, got %q", cfg.ConsoleGRPCTarget)
	}
	if cfg.LogLevel != "warn" {
		t.Fatalf("expected env override, got %q", cfg.LogLevel)
	}
}

func TestEmptyEnvOverridesConfigFile(t *testing.T) {
	writeConfigFile(t, `
console_grpc_target = "10.0.0.1:50051"
secret = "from-file"

[labels]
region = "cn"
`)
	t.Setenv("WORKER_CONSOLE_GRPC_TARGET", "")
	t.Setenv("WORKER_SECRET", "")
	t.Setenv("WORKER_LABELS", "")

	cfg := Load()
	if cfg.ConsoleGRPCTarget != defaultConsoleTarget {
		t.Fatalf("expected empty env to select the default target, got %q", cfg.ConsoleGRPCTarget)
	}
	if cfg.WorkerSecret != "" {
		t.Fatalf("expected empty env to clear worker secret, got %q", cfg.WorkerSecret)
	}
	if len(cfg.Labels) != 0 {
		t.Fatalf("expected empty env to clear labels, got %v", cfg.Labels)
	}
}

func TestLoadReportsConfigFilePath(t *testing.T) {
	path := writeConfigFile(t, "log_format = \"text\"\n")

	cfg := Load()
	if cfg.ConfigFile != path {
		t.Fatalf("expected config file path %q, got %q", path, cfg.ConfigFile)
	}
	if cfg.LogFormat != "text" {
		t.Fatalf("unexpected log format %q", cfg.LogFormat)
	}
}

func TestE2BStandardEnvironmentAliasesOverrideConfigFile(t *testing.T) {
	writeConfigFile(t, `
e2b_api_key = "from-file"
e2b_python_template = "python-from-file"
e2b_terminal_template = "terminal-from-file"
`)
	t.Setenv("E2B_API_KEY", "from-env")
	t.Setenv("E2B_PYTHON_EXEC_TEMPLATE", "python-from-env")
	t.Setenv("E2B_TERMINAL_EXEC_TEMPLATE", "terminal-from-env")
	t.Setenv("E2B_SANDBOX_TIMEOUT_SEC", "420")

	cfg := Load()
	if cfg.E2BAPIKey != "from-env" {
		t.Fatalf("expected standard E2B API key alias")
	}
	if cfg.E2BPythonTemplate != "python-from-env" || cfg.E2BTerminalTemplate != "terminal-from-env" {
		t.Fatalf("unexpected aliased templates %q/%q", cfg.E2BPythonTemplate, cfg.E2BTerminalTemplate)
	}
	if cfg.E2BPythonTimeoutSec != 420 {
		t.Fatalf("unexpected aliased sandbox timeout %d", cfg.E2BPythonTimeoutSec)
	}
}

func TestCanonicalWorkerE2BEnvironmentWinsOverAlias(t *testing.T) {
	writeConfigFile(t, `e2b_api_key = "from-file"`)
	t.Setenv("E2B_API_KEY", "from-alias")
	t.Setenv("WORKER_E2B_API_KEY", "from-worker")

	if got := Load().E2BAPIKey; got != "from-worker" {
		t.Fatalf("expected canonical worker variable, got %q", got)
	}
}
