package config

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type SnapshotConfig struct {
	Enabled             bool
	TmpDir              string
	HelperImage         string
	MaxInflight         int
	TmpQuotaBytes       int64
	CacheMaxBytes       int64
	CacheTTL            time.Duration
	TransferConcurrency int
	TransferIdleTimeout time.Duration
	DrainTimeout        time.Duration
	Error               string
}

func (s source) snapshot() SnapshotConfig {
	cfg := SnapshotConfig{TmpDir: strings.TrimSpace(s.get("WORKER_SNAPSHOT_TMP_DIR")), HelperImage: strings.TrimSpace(s.get("WORKER_SNAPSHOT_DOCKER_HELPER_IMAGE")), MaxInflight: 1, TmpQuotaBytes: 100 << 30, CacheMaxBytes: 10 << 30, CacheTTL: 24 * time.Hour, TransferConcurrency: 4, TransferIdleTimeout: time.Minute, DrainTimeout: time.Minute}
	if enabled := strings.TrimSpace(s.get("WORKER_PERSISTENCE_ENABLED")); enabled != "" {
		value, err := strconv.ParseBool(enabled)
		if err != nil {
			cfg.Error = "invalid WORKER_PERSISTENCE_ENABLED"
		} else {
			cfg.Enabled = value
		}
	}
	if !cfg.Enabled {
		return cfg
	}
	number := func(key string, fallback int64) int64 {
		raw := strings.TrimSpace(s.get(key))
		if raw == "" {
			return fallback
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			cfg.Error = "invalid " + key
			return -1
		}
		return value
	}
	cfg.MaxInflight = int(number("WORKER_SNAPSHOT_MAX_INFLIGHT", 1))
	cfg.TmpQuotaBytes = number("WORKER_SNAPSHOT_TMP_QUOTA_BYTES", 100<<30)
	cfg.CacheMaxBytes = number("WORKER_SNAPSHOT_CACHE_MAX_BYTES", 10<<30)
	cfg.TransferConcurrency = int(number("WORKER_SNAPSHOT_TRANSFER_MAX_CONCURRENCY", 4))
	seconds := func(key string, fallback int64) time.Duration {
		value := number(key, fallback)
		if value < 1 || value > 2592000 {
			cfg.Error = "invalid " + key
			return 0
		}
		return time.Duration(value) * time.Second
	}
	cfg.CacheTTL = seconds("WORKER_SNAPSHOT_CACHE_TTL_SEC", 86400)
	cfg.TransferIdleTimeout = seconds("WORKER_SNAPSHOT_TRANSFER_IDLE_TIMEOUT_SEC", 60)
	cfg.DrainTimeout = seconds("WORKER_SNAPSHOT_DRAIN_TIMEOUT_SEC", 60)
	return cfg
}

func (c SnapshotConfig) Validate() error {
	if c.Error != "" {
		return errors.New(c.Error)
	}
	if !c.Enabled {
		return nil
	}
	if !filepath.IsAbs(c.TmpDir) || c.HelperImage == "" || c.MaxInflight < 1 || c.MaxInflight > 64 || c.TmpQuotaBytes <= 0 || c.TmpQuotaBytes > 1<<60 || c.CacheMaxBytes < 0 || c.CacheMaxBytes > c.TmpQuotaBytes || c.TransferConcurrency < 1 || c.TransferConcurrency > 16 || c.CacheTTL < time.Second || c.TransferIdleTimeout < time.Second || c.DrainTimeout < time.Second {
		return errors.New("invalid Worker snapshot configuration: private absolute tmp directory, pinned GNU tar helper and bounded resource limits are required")
	}
	return nil
}
