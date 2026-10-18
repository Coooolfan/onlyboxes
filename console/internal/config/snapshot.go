package config

import (
	"errors"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

type SnapshotConfig struct {
	Enabled              bool
	Endpoint             string
	Region               string
	BucketName           string
	Prefix               string
	AccessKey            string
	SecretKey            string
	ForcePathStyle       bool
	IntegrityMode        string
	PresignTTL           time.Duration
	PartSize             int64
	MultipartConcurrency int
	MaxBytes             int64
	MaxExpandedBytes     int64
	AccountQuotaBytes    int64
	RetentionTTL         time.Duration
	MaxRetentionTTL      time.Duration
	OperationTimeout     time.Duration
	GCInterval           time.Duration
	GCGrace              time.Duration
	Error                string
}

func (s source) snapshot() SnapshotConfig {
	cfg := SnapshotConfig{Endpoint: strings.TrimSpace(s.get("CONSOLE_SNAPSHOT_STORE_ENDPOINT")), Region: strings.TrimSpace(s.get("CONSOLE_SNAPSHOT_STORE_REGION")), BucketName: strings.TrimSpace(s.get("CONSOLE_SNAPSHOT_STORE_BUCKET_NAME")), Prefix: s.trimmedStringValue("CONSOLE_SNAPSHOT_STORE_PREFIX", "session-snapshots"), AccessKey: strings.TrimSpace(s.get("CONSOLE_SNAPSHOT_STORE_AK")), SecretKey: strings.TrimSpace(s.get("CONSOLE_SNAPSHOT_STORE_SK")), IntegrityMode: s.trimmedStringValue("CONSOLE_SNAPSHOT_STORE_INTEGRITY_MODE", "readback")}
	boolean := func(key string) bool {
		raw := strings.TrimSpace(s.get(key))
		if raw == "" {
			return false
		}
		value, err := strconv.ParseBool(raw)
		if err != nil {
			cfg.Error = "invalid " + key
		}
		return value
	}
	cfg.Enabled = boolean("CONSOLE_PERSISTENCE_ENABLED")
	cfg.ForcePathStyle = boolean("CONSOLE_SNAPSHOT_STORE_FORCE_PATH_STYLE")
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
	seconds := func(key string, fallback int64) time.Duration {
		n := number(key, fallback)
		if n < 1 || n > 31536000 {
			cfg.Error = "invalid " + key
			return 0
		}
		return time.Duration(n) * time.Second
	}
	cfg.PresignTTL = seconds("CONSOLE_SNAPSHOT_PRESIGN_TTL_SEC", 900)
	cfg.PartSize = number("CONSOLE_SNAPSHOT_MULTIPART_PART_SIZE_BYTES", 64<<20)
	cfg.MultipartConcurrency = int(number("CONSOLE_SNAPSHOT_MULTIPART_CONCURRENCY", 4))
	cfg.MaxBytes = number("CONSOLE_SNAPSHOT_MAX_BYTES", 10<<30)
	cfg.MaxExpandedBytes = number("CONSOLE_SNAPSHOT_MAX_EXPANDED_BYTES", 50<<30)
	cfg.AccountQuotaBytes = number("CONSOLE_SNAPSHOT_ACCOUNT_QUOTA_BYTES", 100<<30)
	cfg.RetentionTTL = seconds("CONSOLE_PERSISTENCE_RETENTION_TTL_SEC", 604800)
	cfg.MaxRetentionTTL = seconds("CONSOLE_PERSISTENCE_MAX_RETENTION_TTL_SEC", 2592000)
	cfg.OperationTimeout = seconds("CONSOLE_PERSISTENCE_OPERATION_TIMEOUT_SEC", 1800)
	cfg.GCInterval = seconds("CONSOLE_SNAPSHOT_GC_INTERVAL_SEC", 300)
	cfg.GCGrace = seconds("CONSOLE_SNAPSHOT_GC_GRACE_SEC", 86400)
	return cfg
}
func (c SnapshotConfig) Configured() bool {
	return c.Endpoint != "" || c.Region != "" || c.BucketName != "" || c.AccessKey != "" || c.SecretKey != ""
}
func (c SnapshotConfig) Validate() error {
	if c.Error != "" {
		return errors.New(c.Error)
	}
	if !c.Enabled && !c.Configured() {
		return nil
	}
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || c.Region == "" || c.BucketName == "" || c.AccessKey == "" || c.SecretKey == "" {
		return errors.New("snapshot store requires endpoint, region, bucket and credentials; endpoint must be HTTP(S) without embedded credentials or query")
	}
	if c.Prefix == "" || c.Prefix == "." || strings.HasPrefix(c.Prefix, "/") || strings.Contains(c.Prefix, "\\") || path.Clean(c.Prefix) != c.Prefix || c.Prefix == ".." || strings.HasPrefix(c.Prefix, "../") {
		return errors.New("snapshot store prefix must be a clean relative key prefix")
	}
	if c.IntegrityMode != "readback" {
		return errors.New("snapshot integrity mode must be readback; server-verified checksums are not certified")
	}
	if c.MaxBytes <= 0 || c.MaxBytes > 1<<60 || c.MaxExpandedBytes <= 0 || c.MaxExpandedBytes > 1<<60 || c.AccountQuotaBytes < c.MaxBytes || c.AccountQuotaBytes > 1<<60 || c.PartSize < 5<<20 || c.PartSize > 5<<30 || (c.MaxBytes-1)/c.PartSize+1 > 10000 || c.MultipartConcurrency < 1 || c.MultipartConcurrency > 16 || c.PresignTTL < time.Second || c.PresignTTL > 7*24*time.Hour || c.RetentionTTL < time.Second || c.MaxRetentionTTL < c.RetentionTTL || c.MaxRetentionTTL > 365*24*time.Hour || c.OperationTimeout < time.Second || c.OperationTimeout > 24*time.Hour || c.GCInterval < time.Second || c.GCGrace < c.OperationTimeout {
		return errors.New("snapshot size, concurrency, retention or timeout limits are invalid")
	}
	return nil
}
func (c Config) ValidateSnapshot() error {
	if err := c.Snapshot.Validate(); err != nil {
		return err
	}
	if !c.Snapshot.Enabled && !c.Snapshot.Configured() {
		return nil
	}
	if c.ExportFileEnabled() && strings.TrimRight(c.Snapshot.Endpoint, "/") == strings.TrimRight(c.ExportFileEndpoint, "/") && c.Snapshot.BucketName == c.ExportFileBucketName {
		a, b := strings.Trim(c.Snapshot.Prefix, "/"), strings.Trim(c.ExportFilePrefix, "/")
		if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
			return errors.New("snapshot and exported-file prefixes must not overlap in the same bucket")
		}
	}
	return nil
}
