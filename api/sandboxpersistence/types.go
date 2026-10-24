// Package sandboxpersistence defines private JSON metadata carried by the
// registry lifecycle controls. Metadata and transfer grants may contain secrets.
package sandboxpersistence

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	DockerRootFSFormat   = "docker-rootfs-v1"
	BoxliteArchiveFormat = "boxlite-archive-v1"
)

type RestoreSpec struct {
	Runtime        string            `json:"runtime"`
	RuntimeVersion string            `json:"runtime_version"`
	Platform       string            `json:"platform"`
	Format         string            `json:"format"`
	Image          string            `json:"image"`
	MemoryMiB      int64             `json:"memory_mib"`
	CPUs           float64           `json:"cpus"`
	MaxProcesses   int32             `json:"max_processes"`
	DiskSizeGB     *int64            `json:"disk_size_gb,omitempty"`
	User           string            `json:"user,omitempty"`
	WorkingDir     string            `json:"working_dir,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Entrypoint     []string          `json:"entrypoint,omitempty"`
	Command        []string          `json:"command,omitempty"`
}

var pinnedImage = regexp.MustCompile(`^.+@sha256:[0-9a-f]{64}$`)
var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var snapshotID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func (s RestoreSpec) Validate() error {
	if s.Runtime != "docker" && s.Runtime != "boxlite" {
		return errors.New("snapshot runtime must be docker or boxlite")
	}
	if strings.TrimSpace(s.RuntimeVersion) == "" || len(s.RuntimeVersion) > 128 || (s.Platform != "linux/amd64" && s.Platform != "linux/arm64") || !pinnedImage.MatchString(s.Image) || s.MemoryMiB <= 0 || s.CPUs <= 0 || math.IsNaN(s.CPUs) || math.IsInf(s.CPUs, 0) || s.MaxProcesses <= 0 {
		return errors.New("restore spec requires runtime version, guest platform, pinned image and positive resource limits")
	}
	if (s.Runtime == "docker" && s.Format != DockerRootFSFormat) || (s.Runtime == "boxlite" && s.Format != BoxliteArchiveFormat) {
		return errors.New("snapshot format is not supported by its runtime")
	}
	if s.DiskSizeGB != nil && (s.Runtime != "boxlite" || *s.DiskSizeGB < 1 || *s.DiskSizeGB > 10240) {
		return errors.New("disk_size_gb requires BoxLite and a value from 1 to 10240")
	}
	encoded, err := json.Marshal(s)
	if err != nil || len(encoded) > 32<<10 {
		return errors.New("restore spec exceeds its metadata limit")
	}
	return nil
}

type Archive struct {
	SnapshotID    string `json:"snapshot_id"`
	Size          int64  `json:"size_bytes"`
	SHA256        string `json:"sha256"`
	ExpandedBytes int64  `json:"expanded_bytes"`
	Format        string `json:"format"`
}

func (a Archive) Validate() error {
	if !snapshotID.MatchString(a.SnapshotID) || a.Size <= 0 || a.ExpandedBytes <= 0 || !digest.MatchString(a.SHA256) || (a.Format != DockerRootFSFormat && a.Format != BoxliteArchiveFormat) {
		return errors.New("invalid snapshot archive descriptor")
	}
	return nil
}

// TransferGrant is ephemeral. Persist archive identity and uploaded part results,
// then obtain a new grant after expiry; never persist or log the URL or headers.
type TransferGrant struct {
	URL       string      `json:"url"`
	Method    string      `json:"method"`
	Headers   http.Header `json:"headers"`
	ExpiresAt time.Time   `json:"expires_at"`
	Offset    int64       `json:"offset"`
	Size      int64       `json:"size_bytes"`
}

type UploadPart struct {
	Number int32         `json:"part_number"`
	Grant  TransferGrant `json:"grant"`
}

type CompletedPart struct {
	Number int32  `json:"part_number"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size_bytes"`
}

// Control contains action-specific metadata. Workers validate the fields needed
// by each action against their durable operation journal before side effects.
type Control struct {
	Spec              *RestoreSpec   `json:"restore_spec,omitempty"`
	Archive           *Archive       `json:"archive,omitempty"`
	Parts             []UploadPart   `json:"parts,omitempty"`
	Download          *TransferGrant `json:"download,omitempty"`
	MaxBytes          int64          `json:"max_bytes,omitempty"`
	MaxExpandedBytes  int64          `json:"max_expanded_bytes,omitempty"`
	StagingBytes      int64          `json:"staging_bytes,omitempty"`
	LeaseTTLSec       int            `json:"lease_ttl_sec,omitempty"`
	DrainTimeoutSec   int            `json:"drain_timeout_sec,omitempty"`
	Rollback          bool           `json:"rollback,omitempty"`
	PurgeSession      bool           `json:"purge_session,omitempty"`
	Delete            bool           `json:"delete,omitempty"`
	SnapshotCommitted bool           `json:"snapshot_committed,omitempty"`
}

type Result struct {
	State              string          `json:"state"`
	Spec               *RestoreSpec    `json:"restore_spec,omitempty"`
	Archive            *Archive        `json:"archive,omitempty"`
	Parts              []CompletedPart `json:"parts,omitempty"`
	LeaseExpiresUnixMS int64           `json:"lease_expires_unix_ms,omitempty"`
	InstanceID         string          `json:"instance_id,omitempty"`
}
