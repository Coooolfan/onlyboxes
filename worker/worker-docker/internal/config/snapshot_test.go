package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSnapshotConfigurationStrictLimitsAndPrecedence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(file, []byte(`persistence_enabled = true
[snapshot]
tmp_dir = "/var/lib/onlyboxes/snapshots"
docker_helper_image = "helper@sha256:pinned"
max_inflight = 2
cache_max_bytes = 0
transfer_idle_timeout_sec = 12
`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKER_CONFIG_FILE", file)
	cfg := Load().Snapshot
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.MaxInflight != 2 || cfg.CacheMaxBytes != 0 || cfg.TransferIdleTimeout != 12*time.Second {
		t.Fatalf("TOML configuration lost: %+v", cfg)
	}
	t.Setenv("WORKER_SNAPSHOT_MAX_INFLIGHT", "3")
	if Load().Snapshot.MaxInflight != 3 {
		t.Fatal("environment did not override file")
	}
	for key, value := range map[string]string{
		"WORKER_PERSISTENCE_ENABLED": "maybe", "WORKER_SNAPSHOT_MAX_INFLIGHT": "65",
		"WORKER_SNAPSHOT_TMP_DIR": "relative", "WORKER_SNAPSHOT_TMP_QUOTA_BYTES": "0",
		"WORKER_SNAPSHOT_CACHE_MAX_BYTES": "-1", "WORKER_SNAPSHOT_CACHE_TTL_SEC": "9223372036854775807",
		"WORKER_SNAPSHOT_TRANSFER_MAX_CONCURRENCY": "17", "WORKER_SNAPSHOT_DRAIN_TIMEOUT_SEC": "invalid-secret",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			err := Load().Snapshot.Validate()
			if err == nil {
				t.Fatal("unsafe configuration accepted")
			}
			if strings.Contains(err.Error(), "invalid-secret") {
				t.Fatal("invalid value leaked")
			}
		})
	}
}

func TestSnapshotDisabledByDefault(t *testing.T) {
	t.Setenv("WORKER_PERSISTENCE_ENABLED", "false")
	t.Setenv("WORKER_SNAPSHOT_TMP_QUOTA_BYTES", "invalid")
	cfg := Load().Snapshot
	if cfg.Enabled || cfg.Validate() != nil {
		t.Fatal("disabled persistence required snapshot configuration")
	}
}
