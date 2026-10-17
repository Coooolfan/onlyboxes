// Package sandboxsnapshot stores durable filesystem snapshot operations separately
// from transient terminal routes and ordinary command tasks. It does not activate
// workers or publish routes; the lifecycle coordinator must fence execution first.
package sandboxsnapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
)

type State string

const (
	Running      State = "running"
	Snapshotting State = "snapshotting"
	Suspended    State = "suspended"
	Restoring    State = "restoring"
	Deleting     State = "deleting"
)

type Kind string

const (
	Suspend Kind = "suspend"
	Resume  Kind = "resume"
)

var (
	ErrNotFound            = errors.New("persistent sandbox session or operation not found")
	ErrConflict            = errors.New("persistent sandbox state or generation conflict")
	ErrIdempotencyConflict = errors.New("persistence idempotency key has different parameters")
	ErrAlreadyInState      = errors.New("persistent sandbox already in requested state")
	ErrQuota               = errors.New("snapshot quota exceeded")
)

// RestoreSpec is private metadata and may contain secrets in Env. It must not be
// returned in public session responses or written to application logs.
type RestoreSpec struct {
	Runtime        string            `json:"runtime"`
	RuntimeVersion string            `json:"runtime_version"`
	Platform       string            `json:"platform"`
	Format         string            `json:"format"`
	Image          string            `json:"image"`
	MemoryMiB      int64             `json:"memory_mib"`
	CPUs           int32             `json:"cpus"`
	User           string            `json:"user,omitempty"`
	WorkingDir     string            `json:"working_dir,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Entrypoint     []string          `json:"entrypoint,omitempty"`
	Command        []string          `json:"command,omitempty"`
}

var imageDigestPattern = regexp.MustCompile(`^.+@sha256:[0-9a-f]{64}$`)

func (s RestoreSpec) Validate() error {
	if s.Runtime != "docker" && s.Runtime != "boxlite" {
		return errors.New("snapshot runtime must be docker or boxlite")
	}
	if strings.TrimSpace(s.RuntimeVersion) == "" || (s.Platform != "linux/amd64" && s.Platform != "linux/arm64") || !imageDigestPattern.MatchString(s.Image) || s.CPUs <= 0 || s.MemoryMiB <= 0 {
		return errors.New("snapshot restore spec requires runtime version, supported platform, pinned image and positive resources")
	}
	if (s.Runtime == "docker" && s.Format != "docker-rootfs-v1") || (s.Runtime == "boxlite" && s.Format != "boxlite-archive-v1") {
		return errors.New("snapshot format is not supported by its runtime")
	}
	return nil
}

type Session struct {
	OwnerID                string
	SessionID              string
	State                  State
	Generation             int64
	Spec                   RestoreSpec
	WorkerID               string
	LatestSnapshotID       string
	OperationID            string
	RetentionExpiresUnixMS int64
	CreatedAtUnixMS        int64
	UpdatedAtUnixMS        int64
}

type Operation struct {
	ID             string
	OwnerID        string
	SessionID      string
	Kind           Kind
	IdempotencyKey string
	RequestSHA256  string
	// Canonical request parameters are needed to resume asynchronous work after
	// a Console restart. Never put transfer grants or credentials in this value.
	Parameters      json.RawMessage
	Generation      int64
	Status          string
	Phase           string
	SourceWorkerID  string
	TargetWorkerID  string
	ErrorCode       string
	ErrorMessage    string
	DeadlineUnixMS  int64
	CreatedAtUnixMS int64
	UpdatedAtUnixMS int64
}

type BeginRequest struct {
	OwnerID        string
	SessionID      string
	Kind           Kind
	IdempotencyKey string
	Parameters     json.RawMessage
	DeadlineUnixMS int64
}

func (r BeginRequest) validate(now int64) (string, json.RawMessage, error) {
	if len(r.Parameters) > 16384 {
		return "", nil, errors.New("operation parameters exceed their limit")
	}
	if r.OwnerID == "" || r.SessionID == "" || strings.TrimSpace(r.IdempotencyKey) == "" || len(r.IdempotencyKey) > 256 || r.DeadlineUnixMS <= now || (r.Kind != Suspend && r.Kind != Resume) {
		return "", nil, errors.New("invalid persistence operation request")
	}
	// Normalize JSON whitespace/key order so a retry hashes the logical request.
	// Preserve numeric spellings with UseNumber rather than rounding through float64.
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(r.Parameters)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", nil, fmt.Errorf("invalid operation parameters: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return "", nil, errors.New("operation parameters contain multiple JSON values")
	}
	hash := sha256.Sum256(canonical)
	return hex.EncodeToString(hash[:]), canonical, nil
}

type Manifest struct {
	Version     int                        `json:"version"`
	Archive     objectstore.SnapshotObject `json:"archive"`
	RestoreSpec RestoreSpec                `json:"restore_spec"`
}

type Snapshot struct {
	ID                string
	OwnerID           string
	SessionID         string
	OperationID       string
	Generation        int64
	Status            string
	Upload            objectstore.SnapshotUpload
	Manifest          *Manifest
	ManifestVersionID string
	CreatedAtUnixMS   int64
	UpdatedAtUnixMS   int64
}
