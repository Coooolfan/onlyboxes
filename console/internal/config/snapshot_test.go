package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSnapshotConfigurationStrictValidationAndFilePriority(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	body := `[persistence]
enabled = true
[snapshot.store]
endpoint = "https://snapshots.example.com"
region = "test"
bucket_name = "private-snapshots"
ak = "private-access-key"
sk = "private-secret-key"
force_path_style = true
[snapshot]
max_bytes = 16777216
multipart_part_size_bytes = 5242880
`
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONSOLE_CONFIG_FILE", file)
	cfg := Load()
	if err := cfg.ValidateSnapshot(); err != nil {
		t.Fatal(err)
	}
	if !cfg.Snapshot.Enabled || !cfg.Snapshot.ForcePathStyle || cfg.Snapshot.MaxBytes != 16<<20 || cfg.Snapshot.IntegrityMode != "readback" || cfg.Snapshot.RetentionTTL != 7*24*time.Hour {
		t.Fatal("snapshot TOML/defaults lost")
	}
	t.Setenv("CONSOLE_SNAPSHOT_MAX_BYTES", "33554432")
	if got := Load().Snapshot.MaxBytes; got != 32<<20 {
		t.Fatal("environment did not override file", got)
	}
	for key, value := range map[string]string{
		"CONSOLE_PERSISTENCE_ENABLED": "maybe", "CONSOLE_SNAPSHOT_STORE_FORCE_PATH_STYLE": "maybe",
		"CONSOLE_SNAPSHOT_STORE_ENDPOINT": "https://private-secret-key@example.com", "CONSOLE_SNAPSHOT_STORE_PREFIX": "../outside",
		"CONSOLE_SNAPSHOT_STORE_INTEGRITY_MODE": "server_verified", "CONSOLE_SNAPSHOT_MULTIPART_PART_SIZE_BYTES": "1",
		"CONSOLE_SNAPSHOT_MULTIPART_CONCURRENCY": "17", "CONSOLE_SNAPSHOT_MAX_BYTES": "-1", "CONSOLE_SNAPSHOT_MAX_EXPANDED_BYTES": "0",
		"CONSOLE_SNAPSHOT_PRESIGN_TTL_SEC": "604801", "CONSOLE_PERSISTENCE_MAX_RETENTION_TTL_SEC": "1",
		"CONSOLE_SNAPSHOT_GC_GRACE_SEC": "1", "CONSOLE_PERSISTENCE_OPERATION_TIMEOUT_SEC": "9223372036854775807",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			err := Load().ValidateSnapshot()
			if err == nil {
				t.Fatal("unsafe snapshot configuration accepted")
			}
			if strings.Contains(err.Error(), "private-secret-key") {
				t.Fatal("credentials leaked in validation")
			}
		})
	}
	cfg = Load()
	cfg.ExportFileEndpoint = cfg.Snapshot.Endpoint
	cfg.ExportFileRegion = "test"
	cfg.ExportFileBucketName = cfg.Snapshot.BucketName
	cfg.ExportFileAK = "export-ak"
	cfg.ExportFileSK = "export-sk"
	for _, prefix := range []string{cfg.Snapshot.Prefix, cfg.Snapshot.Prefix + "/exports"} {
		cfg.ExportFilePrefix = prefix
		if cfg.ValidateSnapshot() == nil {
			t.Fatal("snapshot/export storage prefix overlap accepted", prefix)
		}
	}
	cfg.ExportFilePrefix = "exports"
	if err := cfg.ValidateSnapshot(); err != nil {
		t.Fatal("isolated prefix rejected", err)
	}
}
func TestSnapshotConfigurationDisabledByDefault(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(file, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONSOLE_CONFIG_FILE", file)
	cfg := Load()
	if cfg.Snapshot.Enabled || cfg.Snapshot.Configured() || cfg.ValidateSnapshot() != nil {
		t.Fatal("disabled persistence requires object storage")
	}
}
