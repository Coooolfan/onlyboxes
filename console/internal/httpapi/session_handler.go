package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
)

const defaultSessionPageSize = 20

type renewSessionLeaseRequest struct {
	LeaseTTLSec int `json:"lease_ttl_sec"`
}

type sessionItem struct {
	AccountID          string                `json:"account_id"`
	SessionID          string                `json:"session_id"`
	WorkerID           string                `json:"worker_id"`
	Status             string                `json:"status"`
	LeaseExpiresAt     time.Time             `json:"lease_expires_at"`
	LastUsedAt         time.Time             `json:"last_used_at"`
	CreatedAt          time.Time             `json:"created_at"`
	LifecycleState     sandboxsnapshot.State `json:"lifecycle_state"`
	Persistence        sessionPersistence    `json:"persistence"`
	CurrentOperationID *string               `json:"current_operation_id"`
}

type listSessionsResponse struct {
	Items    []sessionItem `json:"items"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
}

func (h *WorkerHandler) SetTerminalSessionRegistry(registry *grpcserver.RegistryService) {
	if h == nil {
		return
	}
	h.sessions = registry
}

func (h *WorkerHandler) ListSessions(c *gin.Context) {
	ownerID, ok := h.resolveConsoleSessionOwner(c, false)
	if !ok {
		return
	}
	if h.sessions == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "session registry is unavailable"})
		return
	}

	page, ok := parsePositiveIntQuery(c, "page", 1)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page must be a positive integer"})
		return
	}
	pageSize, ok := parsePositiveIntQuery(c, "page_size", defaultSessionPageSize)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page_size must be a positive integer"})
		return
	}
	if pageSize > maxPageSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page_size must be <= 100"})
		return
	}

	views, err := h.sessions.ListManagedSessions(c.Request.Context(), ownerID, h.nowFn())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read sessions"})
		return
	}
	total := len(views)
	start := total
	if page-1 <= total/pageSize {
		start = (page - 1) * pageSize
	}
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	items := make([]sessionItem, 0, end-start)
	for _, view := range views[start:end] {
		items = append(items, managedSessionItem(view))
	}
	c.JSON(http.StatusOK, listSessionsResponse{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

func (h *WorkerHandler) GetSession(c *gin.Context) {
	ownerID, ok := h.resolveConsoleSessionOwner(c, true)
	if !ok {
		return
	}
	if h.sessions == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "session registry is unavailable"})
		return
	}
	sessionID := strings.TrimSpace(c.Param("session_id"))
	if sessionID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	view, err := h.sessions.GetManagedSession(c.Request.Context(), ownerID, sessionID, h.nowFn())
	if errors.Is(err, sandboxsnapshot.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read session"})
		return
	}
	c.JSON(http.StatusOK, managedSessionItem(view))
}

func (h *WorkerHandler) DeleteSession(c *gin.Context) {
	ownerID, ok := h.resolveConsoleSessionOwner(c, true)
	if !ok {
		return
	}
	if h.sessions == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "session registry is unavailable"})
		return
	}
	sessionID := strings.TrimSpace(c.Param("session_id"))
	if sessionID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	deleted, err := h.sessions.DeleteManagedSession(c.Request.Context(), ownerID, sessionID, h.nowFn())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete session"})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *WorkerHandler) RenewSessionLease(c *gin.Context) {
	ownerID, ok := h.resolveConsoleSessionOwner(c, true)
	if !ok {
		return
	}
	if h.sessions == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "session registry is unavailable"})
		return
	}
	sessionID := strings.TrimSpace(c.Param("session_id"))
	if sessionID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	req := renewSessionLeaseRequest{}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if req.LeaseTTLSec <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "lease_ttl_sec must be positive"})
		return
	}

	_, err := h.sessions.RenewTerminalSessionLease(
		c.Request.Context(),
		ownerID,
		sessionID,
		req.LeaseTTLSec,
	)
	if err != nil {
		switch {
		case errors.Is(err, grpcserver.ErrTerminalLeaseInvalid):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, grpcserver.ErrTerminalSessionNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		case errors.Is(err, grpcserver.ErrTerminalSessionUnavailable), errors.Is(err, context.Canceled):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "session is unavailable"})
		case errors.Is(err, context.DeadlineExceeded):
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "session lease renewal timed out"})
		default:
			var commandErr *grpcserver.CommandExecutionError
			if errors.As(err, &commandErr) && commandErr.Code == "session_state_conflict" {
				h.snapshotError(c, commandErr)
				return
			}
			if errors.As(err, &commandErr) && commandErr.Code == "session_unavailable" {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": commandErr.Message, "code": commandErr.Code})
				return
			}
			if errors.As(err, &commandErr) && commandErr.Code == "invalid_payload" {
				c.JSON(http.StatusBadRequest, gin.H{"error": commandErr.Error()})
				return
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to renew session lease"})
		}
		return
	}
	view, err := h.sessions.GetManagedSession(c.Request.Context(), ownerID, sessionID, h.nowFn())
	if err != nil {
		h.snapshotError(c, err)
		return
	}
	c.JSON(http.StatusOK, managedSessionItem(view))
}

func (h *WorkerHandler) resolveConsoleSessionOwner(c *gin.Context, requireAdminAccountID bool) (string, bool) {
	accountOwner, isAdmin, ok := resolveWorkerAccessScope(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return "", false
	}
	requested := strings.TrimSpace(c.Query("account_id"))
	if isAdmin {
		if requireAdminAccountID && requested == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "account_id is required"})
			return "", false
		}
		return requested, true
	}
	if requested != "" && requested != accountOwner {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return "", false
	}
	return accountOwner, true
}

func sessionItemFromView(view grpcserver.TerminalSessionView) sessionItem {
	return sessionItem{
		LifecycleState: sandboxsnapshot.Running,
		AccountID:      view.AccountID,
		SessionID:      view.SessionID,
		WorkerID:       view.WorkerID,
		Status:         view.Status,
		LeaseExpiresAt: unixMilliTime(view.LeaseExpiresUnixMs),
		LastUsedAt:     unixMilliTime(view.LastUsedUnixMs),
		CreatedAt:      unixMilliTime(view.CreatedAtUnixMs),
	}
}

func unixMilliTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
