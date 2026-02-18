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
	t.Setenv("CONSOLE_DASHBOARD_USERNAME", "")
	t.Setenv("CONSOLE_DASHBOARD_PASSWORD", "")

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
	if cfg.DashboardUsername != "" || cfg.DashboardPassword != "" {
		t.Fatalf("expected empty dashboard credentials, got username=%q password=%q", cfg.DashboardUsername, cfg.DashboardPassword)
	}
}

func TestLoadReadsDashboardCredentialsAndDurations(t *testing.T) {
	t.Setenv("CONSOLE_DASHBOARD_USERNAME", "admin")
	t.Setenv("CONSOLE_DASHBOARD_PASSWORD", "secret")
	t.Setenv("CONSOLE_OFFLINE_TTL_SEC", "30")
	t.Setenv("CONSOLE_REPLAY_WINDOW_SEC", "120")
	t.Setenv("CONSOLE_HEARTBEAT_INTERVAL_SEC", "10")

	cfg := Load()
	if cfg.DashboardUsername != "admin" {
		t.Fatalf("expected username admin, got %q", cfg.DashboardUsername)
	}
	if cfg.DashboardPassword != "secret" {
		t.Fatalf("expected password secret, got %q", cfg.DashboardPassword)
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
