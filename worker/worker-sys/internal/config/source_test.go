package config

import (
	"os"
	"path/filepath"
	"reflect"
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
console_grpc_target = "10.0.0.1:50051"
console_insecure = true
id = "worker-1"
secret = "s3cret"
heartbeat_interval_sec = 7
computer_use_output_limit_bytes = 4096
computer_use_command_whitelist_mode = "prefix"
computer_use_command_whitelist = ["ls", "cat"]
read_image_allowed_paths = ["/tmp"]
log_level = "debug"
log_add_source = true

[labels]
region = "cn"
description = "gpu,shared"
`)

	cfg := Load()
	if cfg.ConfigFile != path {
		t.Fatalf("expected config file path %q, got %q", path, cfg.ConfigFile)
	}
	if cfg.ConsoleGRPCTarget != "10.0.0.1:50051" {
		t.Fatalf("unexpected console target %q", cfg.ConsoleGRPCTarget)
	}
	if cfg.ConsoleTLS {
		t.Fatalf("expected console TLS disabled by console_insecure=true")
	}
	if cfg.WorkerID != "worker-1" || cfg.WorkerSecret != "s3cret" {
		t.Fatalf("unexpected identity %q/%q", cfg.WorkerID, cfg.WorkerSecret)
	}
	if cfg.HeartbeatInterval != 7*time.Second || cfg.CallTimeout != 18*time.Second {
		t.Fatalf("unexpected timings %s/%s", cfg.HeartbeatInterval, cfg.CallTimeout)
	}
	if cfg.ComputerUseOutputLimitByte != 4096 {
		t.Fatalf("unexpected output limit %d", cfg.ComputerUseOutputLimitByte)
	}
	if cfg.ComputerUseWhitelistMode != computerUseWhitelistModePrefix {
		t.Fatalf("unexpected whitelist mode %q", cfg.ComputerUseWhitelistMode)
	}
	if !reflect.DeepEqual(cfg.ComputerUseWhitelist, []string{"ls", "cat"}) {
		t.Fatalf("unexpected whitelist %v", cfg.ComputerUseWhitelist)
	}
	if !reflect.DeepEqual(cfg.ReadImageAllowedPaths, []string{"/tmp"}) {
		t.Fatalf("unexpected read image paths %v", cfg.ReadImageAllowedPaths)
	}
	if cfg.LogLevel != "debug" || !cfg.LogAddSource {
		t.Fatalf("unexpected log config %q/%t", cfg.LogLevel, cfg.LogAddSource)
	}
	if cfg.Labels["region"] != "cn" || cfg.Labels["description"] != "gpu,shared" {
		t.Fatalf("unexpected labels %v", cfg.Labels)
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
