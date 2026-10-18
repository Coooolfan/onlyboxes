package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
)

type sessionPersistence struct {
	Enabled            bool             `json:"enabled"`
	Runtime            string           `json:"runtime,omitempty"`
	Platform           string           `json:"platform,omitempty"`
	RetentionExpiresAt *time.Time       `json:"retention_expires_at"`
	LatestSnapshot     *sessionSnapshot `json:"latest_snapshot"`
}
type sessionSnapshot struct {
	ID        string    `json:"snapshot_id"`
	CreatedAt time.Time `json:"created_at"`
	SizeBytes int64     `json:"size_bytes"`
	SHA256    string    `json:"sha256"`
}

func (s sessionItem) MarshalJSON() ([]byte, error) {
	type plain sessionItem
	var worker, status *string
	if s.WorkerID != "" {
		worker = &s.WorkerID
	}
	if s.Status != "" {
		status = &s.Status
	}
	return json.Marshal(struct {
		plain
		WorkerID       *string    `json:"worker_id"`
		Status         *string    `json:"status"`
		LeaseExpiresAt *time.Time `json:"lease_expires_at"`
	}{plain: plain(s), WorkerID: worker, Status: status, LeaseExpiresAt: optionalTime(s.LeaseExpiresAt)})
}
func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func managedSessionItem(v grpcserver.ManagedSessionView) sessionItem {
	item := sessionItemFromView(v.TerminalSessionView)
	item.LifecycleState = v.LifecycleState
	item.CurrentOperationID = optionalString(v.OperationID)
	item.Persistence = sessionPersistence{Enabled: v.SnapshotID != "", Runtime: v.Runtime, Platform: v.Platform, RetentionExpiresAt: optionalTime(unixMilliTime(v.RetentionExpiresUnixMS))}
	if v.SnapshotID != "" {
		item.Persistence.LatestSnapshot = &sessionSnapshot{ID: v.SnapshotID, CreatedAt: unixMilliTime(v.SnapshotCreatedAtUnixMS), SizeBytes: v.SnapshotSizeBytes, SHA256: v.SnapshotSHA256}
	}
	return item
}

type snapshotBeginner interface {
	Begin(context.Context, string, string, sandboxsnapshot.Kind, string, time.Duration, int) (sandboxsnapshot.Session, *sandboxsnapshot.Operation, error)
}

func (h *WorkerHandler) SetSnapshotCoordinator(coordinator *grpcserver.SnapshotCoordinator, enabled bool) {
	if coordinator == nil {
		h.snapshotCoordinator = nil
	} else {
		h.snapshotCoordinator = coordinator
	}
	h.persistenceEnabled = enabled
}

type snapshotOperationResponse struct {
	ID         string                    `json:"operation_id"`
	SessionID  string                    `json:"session_id"`
	Kind       sandboxsnapshot.Kind      `json:"kind"`
	Status     string                    `json:"status"`
	Phase      string                    `json:"phase"`
	Generation int64                     `json:"generation"`
	Progress   snapshotOperationProgress `json:"progress"`
	Error      *snapshotOperationError   `json:"error"`
}
type snapshotOperationProgress struct {
	BytesCompleted int64  `json:"bytes_completed"`
	BytesTotal     *int64 `json:"bytes_total"`
}
type snapshotOperationError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func operationResponse(op sandboxsnapshot.Operation) snapshotOperationResponse {
	response := snapshotOperationResponse{ID: op.ID, SessionID: op.SessionID, Kind: op.Kind, Status: op.Status, Phase: op.Phase, Generation: op.Generation}
	if op.ErrorCode != "" {
		_, message, retryable := snapshotPublicError(op.ErrorCode)
		response.Error = &snapshotOperationError{Code: snapshotPublicCode(op.ErrorCode), Message: message, Retryable: retryable}
	}
	return response
}

func (h *WorkerHandler) SuspendSession(c *gin.Context) { h.beginSnapshot(c, sandboxsnapshot.Suspend) }
func (h *WorkerHandler) ResumeSession(c *gin.Context)  { h.beginSnapshot(c, sandboxsnapshot.Resume) }
func (h *WorkerHandler) beginSnapshot(c *gin.Context, kind sandboxsnapshot.Kind) {
	owner, ok := h.resolveConsoleSessionOwner(c, true)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("session_id"))
	if id == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" || len(key) > 256 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Idempotency-Key is required and must be at most 256 bytes"})
		return
	}
	var retention time.Duration
	var lease int
	if kind == sandboxsnapshot.Suspend {
		var req *struct {
			RetentionTTLSec json.RawMessage `json:"retention_ttl_sec"`
		}
		if strictSnapshotJSON(c, &req) != nil || req == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid suspend request body"})
			return
		}
		if len(req.RetentionTTLSec) > 0 {
			var seconds int64
			if json.Unmarshal(req.RetentionTTLSec, &seconds) != nil || seconds <= 0 || seconds > int64((1<<63-1)/time.Second) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "retention_ttl_sec must be positive and within the configured limit"})
				return
			}
			retention = time.Duration(seconds) * time.Second
		}
	} else {
		var req *struct {
			LeaseTTLSec int `json:"lease_ttl_sec"`
		}
		if strictSnapshotJSON(c, &req) != nil || req == nil || (req.LeaseTTLSec <= 0 || req.LeaseTTLSec > (1<<31-1)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "lease_ttl_sec must be a positive integer"})
			return
		}
		lease = req.LeaseTTLSec
	}
	if !h.persistenceEnabled || h.snapshotCoordinator == nil || h.sessions == nil {
		h.snapshotError(c, grpcserver.ErrPersistenceUnavailable)
		return
	}
	session, op, err := h.snapshotCoordinator.Begin(c.Request.Context(), owner, id, kind, key, retention, lease)
	if err != nil {
		h.snapshotError(c, err)
		return
	}
	if op == nil || (op.Status == "succeeded" && ((kind == sandboxsnapshot.Suspend && session.State == sandboxsnapshot.Suspended) || (kind == sandboxsnapshot.Resume && session.State == sandboxsnapshot.Running)) && session.OperationID == "") {
		view, err := h.sessions.GetManagedSession(c.Request.Context(), owner, id, h.nowFn())
		if err != nil {
			h.snapshotError(c, err)
			return
		}
		c.JSON(http.StatusOK, managedSessionItem(view))
		return
	}
	location := "/api/v1/sessions/" + url.PathEscape(id) + "/operations/" + url.PathEscape(op.ID) + "?account_id=" + url.QueryEscape(owner)
	c.Header("Location", location)
	c.JSON(http.StatusAccepted, operationResponse(*op))
}
func strictSnapshotJSON(c *gin.Context, target any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}
func (h *WorkerHandler) GetSnapshotOperation(c *gin.Context) {
	owner, ok := h.resolveConsoleSessionOwner(c, true)
	if !ok {
		return
	}
	if h.sessions == nil {
		h.snapshotError(c, grpcserver.ErrPersistenceUnavailable)
		return
	}
	op, err := h.sessions.SnapshotOperation(c.Request.Context(), owner, strings.TrimSpace(c.Param("session_id")), strings.TrimSpace(c.Param("operation_id")))
	if err != nil {
		h.snapshotError(c, err)
		return
	}
	c.JSON(http.StatusOK, operationResponse(op))
}
func (h *WorkerHandler) snapshotError(c *gin.Context, err error) {
	code := "snapshot_storage_error"
	switch {
	case errors.Is(err, sandboxsnapshot.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "session or operation not found"})
		return
	case errors.Is(err, grpcserver.ErrSnapshotParameters):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid snapshot operation parameters"})
		return
	case errors.Is(err, sandboxsnapshot.ErrConflict), errors.Is(err, sandboxsnapshot.ErrIdempotencyConflict):
		code = "session_state_conflict"
	case errors.Is(err, sandboxsnapshot.ErrQuota):
		code = "snapshot_quota_exceeded"
	case errors.Is(err, grpcserver.ErrPersistenceUnavailable):
		code = "persistence_unavailable"
	case errors.Is(err, grpcserver.ErrNoCapabilityWorker):
		code = "no_worker"
	case errors.Is(err, grpcserver.ErrNoWorkerCapacity):
		code = "no_capacity"
	case errors.Is(err, context.DeadlineExceeded):
		code = "timeout"
	default:
		var worker *grpcserver.CommandExecutionError
		if errors.As(err, &worker) {
			code = snapshotPublicCode(worker.Code)
		}
	}
	status, message, retryable := snapshotPublicError(code)
	body := gin.H{"error": message, "code": code, "retryable": retryable}
	if code == "session_state_conflict" && h.sessions != nil {
		owner, _, ok := resolveWorkerAccessScope(c)
		if ok {
			if selected := strings.TrimSpace(c.Query("account_id")); selected != "" {
				owner = selected
			}
			if view, err := h.sessions.GetManagedSession(c.Request.Context(), owner, strings.TrimSpace(c.Param("session_id")), h.nowFn()); err == nil {
				body["lifecycle_state"] = view.LifecycleState
				body["current_operation_id"] = optionalString(view.OperationID)
			}
		}
	}
	c.JSON(status, body)
}
func snapshotPublicCode(code string) string {
	switch code {
	case "invalid_payload", "session_state_conflict", "snapshot_quota_exceeded", "persistence_unavailable", "no_worker", "no_capacity", "session_capacity_exceeded", "timeout", "snapshot_integrity_error", "snapshot_storage_error":
		return code
	}
	return "snapshot_storage_error"
}
func snapshotPublicError(code string) (int, string, bool) {
	switch code {
	case "invalid_payload":
		return http.StatusBadRequest, "snapshot parameters are outside the worker limits", false
	case "session_state_conflict":
		return http.StatusConflict, "session lifecycle conflicts with this request", false
	case "snapshot_quota_exceeded":
		return http.StatusRequestEntityTooLarge, "snapshot storage quota exceeded", false
	case "persistence_unavailable":
		return http.StatusServiceUnavailable, "sandbox persistence is unavailable", true
	case "no_worker":
		return http.StatusServiceUnavailable, "no compatible online worker is available", true
	case "no_capacity", "session_capacity_exceeded":
		return http.StatusTooManyRequests, "compatible worker capacity is exhausted", true
	case "timeout":
		return http.StatusGatewayTimeout, "sandbox lifecycle operation timed out; backend reconciliation is required", true
	case "snapshot_integrity_error":
		return http.StatusBadGateway, "snapshot archive verification failed", false
	default:
		return http.StatusBadGateway, "sandbox snapshot storage or lifecycle action failed", true
	}
}
