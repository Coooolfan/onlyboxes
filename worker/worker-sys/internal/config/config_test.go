package config

import (
	"reflect"
	"testing"
	"time"
)

func TestLoadUsesComputerUseOutputLimitEnv(t *testing.T) {
	t.Setenv("WORKER_COMPUTER_USE_OUTPUT_LIMIT_BYTES", "2048")

	cfg := Load()
	if cfg.ComputerUseOutputLimitByte != 2048 {
		t.Fatalf("expected output limit 2048, got %d", cfg.ComputerUseOutputLimitByte)
	}
}

func TestLoadUsesDefaultComputerUseWhitelistMode(t *testing.T) {
	t.Setenv("WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE", "")

	cfg := Load()
	if cfg.ComputerUseWhitelistMode != "exact" {
		t.Fatalf("expected default whitelist mode exact, got %q", cfg.ComputerUseWhitelistMode)
	}
}

func TestLoadNormalizesComputerUseWhitelistMode(t *testing.T) {
	t.Setenv("WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE", "  PREFIX ")

	cfg := Load()
	if cfg.ComputerUseWhitelistMode != "prefix" {
		t.Fatalf("expected whitelist mode prefix, got %q", cfg.ComputerUseWhitelistMode)
	}
}

func TestLoadFallsBackComputerUseWhitelistModeOnInvalidValue(t *testing.T) {
	t.Setenv("WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE", "bad_mode")

	cfg := Load()
	if cfg.ComputerUseWhitelistMode != "exact" {
		t.Fatalf("expected fallback whitelist mode exact, got %q", cfg.ComputerUseWhitelistMode)
	}
}

func TestLoadParsesComputerUseWhitelist(t *testing.T) {
	t.Setenv("WORKER_COMPUTER_USE_COMMAND_WHITELIST", `[" echo ","time","","echo"]`)

	cfg := Load()
	want := []string{"echo", "time"}
	if !reflect.DeepEqual(cfg.ComputerUseWhitelist, want) {
		t.Fatalf("unexpected whitelist: want=%v got=%v", want, cfg.ComputerUseWhitelist)
	}
}

func TestLoadUsesEmptyComputerUseWhitelistWhenInvalid(t *testing.T) {
	t.Setenv("WORKER_COMPUTER_USE_COMMAND_WHITELIST", `{"not":"array"}`)

	cfg := Load()
	if len(cfg.ComputerUseWhitelist) != 0 {
		t.Fatalf("expected empty whitelist, got %v", cfg.ComputerUseWhitelist)
	}
}

func TestLoadUsesEmptyReadImageAllowedPathsByDefault(t *testing.T) {
	t.Setenv("WORKER_READ_IMAGE_ALLOWED_PATHS", "")

	cfg := Load()
	if len(cfg.ReadImageAllowedPaths) != 0 {
		t.Fatalf("expected empty readImage allowed paths, got %v", cfg.ReadImageAllowedPaths)
	}
}

func TestLoadParsesReadImageAllowedPaths(t *testing.T) {
	t.Setenv("WORKER_READ_IMAGE_ALLOWED_PATHS", `[" /data/images ","/tmp/a.png","","/tmp/a.png"]`)

	cfg := Load()
	want := []string{"/data/images", "/tmp/a.png"}
	if !reflect.DeepEqual(cfg.ReadImageAllowedPaths, want) {
		t.Fatalf("unexpected readImage allowed paths: want=%v got=%v", want, cfg.ReadImageAllowedPaths)
	}
}

func TestLoadUsesEmptyReadImageAllowedPathsWhenInvalid(t *testing.T) {
	t.Setenv("WORKER_READ_IMAGE_ALLOWED_PATHS", `{"not":"array"}`)

	cfg := Load()
	if len(cfg.ReadImageAllowedPaths) != 0 {
		t.Fatalf("expected empty readImage allowed paths, got %v", cfg.ReadImageAllowedPaths)
	}
}

func TestLoadUsesDynamicCallTimeoutDefault(t *testing.T) {
	t.Setenv("WORKER_HEARTBEAT_INTERVAL_SEC", "5")
	t.Setenv("WORKER_CALL_TIMEOUT_SEC", "")

	cfg := Load()
	if cfg.CallTimeout != 13*time.Second {
		t.Fatalf("expected dynamic default call timeout 13s, got %s", cfg.CallTimeout)
	}
}

func TestLoadUsesDynamicCallTimeoutDefaultFromHeartbeat(t *testing.T) {
	t.Setenv("WORKER_HEARTBEAT_INTERVAL_SEC", "7")
	t.Setenv("WORKER_CALL_TIMEOUT_SEC", "")

	cfg := Load()
	if cfg.CallTimeout != 18*time.Second {
		t.Fatalf("expected dynamic default call timeout 18s, got %s", cfg.CallTimeout)
	}
}

func TestLoadKeepsExplicitCallTimeout(t *testing.T) {
	t.Setenv("WORKER_HEARTBEAT_INTERVAL_SEC", "5")
	t.Setenv("WORKER_CALL_TIMEOUT_SEC", "9")

	cfg := Load()
	if cfg.CallTimeout != 9*time.Second {
		t.Fatalf("expected explicit call timeout 9s, got %s", cfg.CallTimeout)
	}
}

func TestLoadUsesDefaultLogConfig(t *testing.T) {
	t.Setenv("WORKER_LOG_LEVEL", "")
	t.Setenv("WORKER_LOG_FORMAT", "")
	t.Setenv("WORKER_LOG_ADD_SOURCE", "")

	cfg := Load()
	if cfg.LogLevel != defaultLogLevel {
		t.Fatalf("expected default log level %q, got %q", defaultLogLevel, cfg.LogLevel)
	}
	if cfg.LogFormat != defaultLogFormat {
		t.Fatalf("expected default log format %q, got %q", defaultLogFormat, cfg.LogFormat)
	}
	if cfg.LogAddSource != defaultLogAddSource {
		t.Fatalf("expected default log add_source=%t, got %t", defaultLogAddSource, cfg.LogAddSource)
	}
}

func TestLoadSupportsCustomLogConfig(t *testing.T) {
	t.Setenv("WORKER_LOG_LEVEL", "debug")
	t.Setenv("WORKER_LOG_FORMAT", "text")
	t.Setenv("WORKER_LOG_ADD_SOURCE", "true")

	cfg := Load()
	if cfg.LogLevel != "debug" {
		t.Fatalf("expected custom log level debug, got %q", cfg.LogLevel)
	}
	if cfg.LogFormat != "text" {
		t.Fatalf("expected custom log format text, got %q", cfg.LogFormat)
	}
	if !cfg.LogAddSource {
		t.Fatalf("expected custom log add_source=true")
	}
}

func TestLoadFallsBackInvalidLogConfig(t *testing.T) {
	t.Setenv("WORKER_LOG_LEVEL", "verbose")
	t.Setenv("WORKER_LOG_FORMAT", "yaml")
	t.Setenv("WORKER_LOG_ADD_SOURCE", "invalid")

	cfg := Load()
	if cfg.LogLevel != defaultLogLevel {
		t.Fatalf("expected fallback log level %q, got %q", defaultLogLevel, cfg.LogLevel)
	}
	if cfg.LogFormat != defaultLogFormat {
		t.Fatalf("expected fallback log format %q, got %q", defaultLogFormat, cfg.LogFormat)
	}
	if cfg.LogAddSource != defaultLogAddSource {
		t.Fatalf("expected fallback log add_source=%t, got %t", defaultLogAddSource, cfg.LogAddSource)
	}
}

func TestParseLabelsEmpty(t *testing.T) {
	labels := parseLabels("   ")
	if len(labels) != 0 {
		t.Fatalf("expected empty labels, got %v", labels)
	}
}

func TestParseLabelsMixedEntries(t *testing.T) {
	raw := "region=cn, invalid,owner = team-a,=bad, name = value with spaces ,empty=,k=v=extra, ,repeated=old,repeated=new"
	labels := parseLabels(raw)
	want := map[string]string{
		"region":   "cn",
		"owner":    "team-a",
		"name":     "value with spaces",
		"empty":    "",
		"k":        "v=extra",
		"repeated": "new",
	}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("unexpected labels: want=%v got=%v", want, labels)
	}
}
