package config

import (
	"reflect"
	"testing"
	"time"
)

func TestLoadUsesDefaultDockerImages(t *testing.T) {
	t.Setenv("WORKER_PYTHON_EXEC_DOCKER_IMAGE", "")
	t.Setenv("WORKER_TERMINAL_EXEC_DOCKER_IMAGE", "")

	cfg := Load()
	if cfg.PythonExecDockerImage != defaultPythonExecImage {
		t.Fatalf("expected default pythonExec image %q, got %q", defaultPythonExecImage, cfg.PythonExecDockerImage)
	}
	if cfg.TerminalExecDockerImage != defaultTerminalExecImage {
		t.Fatalf("expected default terminalExec image %q, got %q", defaultTerminalExecImage, cfg.TerminalExecDockerImage)
	}
}

func TestLoadSupportsCustomDockerImages(t *testing.T) {
	t.Setenv("WORKER_PYTHON_EXEC_DOCKER_IMAGE", "python:3.12-alpine")
	t.Setenv("WORKER_TERMINAL_EXEC_DOCKER_IMAGE", "debian:bookworm-slim")

	cfg := Load()
	if cfg.PythonExecDockerImage != "python:3.12-alpine" {
		t.Fatalf("expected custom pythonExec image, got %q", cfg.PythonExecDockerImage)
	}
	if cfg.TerminalExecDockerImage != "debian:bookworm-slim" {
		t.Fatalf("expected custom terminalExec image, got %q", cfg.TerminalExecDockerImage)
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

func TestLoadTerminalMaxActiveSessions(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "default", value: "", want: 0},
		{name: "unlimited", value: "0", want: 0},
		{name: "finite", value: "12", want: 12},
		{name: "negative_falls_back", value: "-1", want: 0},
		{name: "invalid_falls_back", value: "many", want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WORKER_TERMINAL_MAX_ACTIVE_SESSIONS", tc.value)
			if got := Load().TerminalMaxActiveSessions; got != tc.want {
				t.Fatalf("expected terminal max active sessions %d, got %d", tc.want, got)
			}
		})
	}
}

func TestLoadProxyConfig(t *testing.T) {
	t.Setenv("WORKER_PROXY_ENABLED", "true")
	t.Setenv("WORKER_PROXY_LISTEN_ADDR", "127.0.0.1:18091")
	t.Setenv("WORKER_PROXY_ADVERTISE_ADDR", "10.0.0.5:18091")

	cfg := Load()
	if !cfg.ProxyEnabled {
		t.Fatalf("expected proxy enabled")
	}
	if cfg.ProxyListenAddr != "127.0.0.1:18091" || cfg.ProxyAdvertiseAddr != "10.0.0.5:18091" {
		t.Fatalf("unexpected proxy config: listen=%q advertise=%q", cfg.ProxyListenAddr, cfg.ProxyAdvertiseAddr)
	}
}

func TestLoadProxyDefaults(t *testing.T) {
	t.Setenv("WORKER_PROXY_ENABLED", "")
	t.Setenv("WORKER_PROXY_LISTEN_ADDR", "")
	t.Setenv("WORKER_PROXY_ADVERTISE_ADDR", "")

	cfg := Load()
	if cfg.ProxyEnabled {
		t.Fatalf("expected proxy disabled by default")
	}
	if cfg.ProxyListenAddr != defaultProxyListenAddr || cfg.ProxyAdvertiseAddr != "" {
		t.Fatalf("unexpected proxy defaults: listen=%q advertise=%q", cfg.ProxyListenAddr, cfg.ProxyAdvertiseAddr)
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
