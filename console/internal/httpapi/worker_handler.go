package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/config"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
)

const (
	maxPageSize = 100
)

var ErrMCPAuthRequired = errors.New("mcp auth is required")

type WorkerHandler struct {
	store                      *registry.Store
	offlineTTL                 time.Duration
	dispatcher                 CommandDispatcher
	provisioning               WorkerProvisioning
	inflightStats              InflightStatsProvider
	consoleGRPCAddr            string
	exportStore                ExportStore
	exportPrefix               string
	exportUploadTTL            time.Duration
	exportDownloadTTL          time.Duration
	exportReturnSchema         string
	proxyRoutes                *ProxyRouteHandler
	sessions                   *grpcserver.RegistryService
	nowFn                      func() time.Time
	computerUseSessionIDPrefix string
}

type WorkerProvisioning interface {
	CreateProvisionedWorkerForOwner(ownerID string, workerType string, now time.Time, offlineTTL time.Duration) (string, string, error)
	DeleteProvisionedWorker(nodeID string) (bool, error)
}

type workerItem struct {
	NodeID       string                           `json:"node_id"`
	NodeName     string                           `json:"node_name"`
	ExecutorKind string                           `json:"executor_kind"`
	Capabilities []registry.CapabilityDeclaration `json:"capabilities"`
	Labels       map[string]string                `json:"labels"`
	Version      string                           `json:"version"`
	Status       registry.WorkerStatus            `json:"status"`
	RegisteredAt time.Time                        `json:"registered_at"`
	LastSeenAt   time.Time                        `json:"last_seen_at"`
}

type listWorkersResponse struct {
	Items    []workerItem `json:"items"`
	Total    int          `json:"total"`
	Page     int          `json:"page"`
	PageSize int          `json:"page_size"`
}

type workerStartupCommandResponse struct {
	NodeID       string `json:"node_id"`
	Type         string `json:"type"`
	WorkerSecret string `json:"worker_secret"`
}

type createWorkerRequest struct {
	Type string `json:"type"`
}

func NewWorkerHandler(
	store *registry.Store,
	offlineTTL time.Duration,
	dispatcher CommandDispatcher,
	provisioning WorkerProvisioning,
	inflightStats InflightStatsProvider,
	consoleGRPCAddr string,
) *WorkerHandler {
	return &WorkerHandler{
		store:                      store,
		offlineTTL:                 offlineTTL,
		dispatcher:                 dispatcher,
		provisioning:               provisioning,
		inflightStats:              inflightStats,
		consoleGRPCAddr:            strings.TrimSpace(consoleGRPCAddr),
		nowFn:                      time.Now,
		computerUseSessionIDPrefix: "CU:",
	}
}

func (h *WorkerHandler) SetComputerUseSessionIDPrefix(prefix string) {
	if h == nil {
		return
	}
	if trimmed := strings.TrimSpace(prefix); trimmed != "" {
		h.computerUseSessionIDPrefix = trimmed
	}
}

func (h *WorkerHandler) SetExportStore(store ExportStore, exportPrefix string, uploadTTL time.Duration, downloadTTL time.Duration, returnSchema string) {
	if h == nil {
		return
	}
	h.exportStore = store
	h.exportPrefix = strings.TrimSpace(exportPrefix)
	h.exportUploadTTL = uploadTTL
	h.exportDownloadTTL = downloadTTL
	h.exportReturnSchema = returnSchema
}

func (h *WorkerHandler) SetProxyRouteHandler(handler *ProxyRouteHandler) {
	if h == nil {
		return
	}
	h.proxyRoutes = handler
}

func NewRouter(workerHandler *WorkerHandler, consoleAuth *ConsoleAuth, mcpAuth *MCPAuth, apiKeyAuth *APIKeyAuth, hiddenTools map[string]bool, mcpToolOverrides map[string]config.MCPToolOverride) (*gin.Engine, error) {
	if mcpAuth == nil {
		return nil, ErrMCPAuthRequired
	}
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	if workerHandler != nil && workerHandler.proxyRoutes != nil {
		router.GET("/internal/v1/proxy/resolve", workerHandler.proxyRoutes.Resolve)
	}
	router.Any("/mcp", mcpAuth.RequireTokenWithQueryFallback(), gin.WrapH(NewMCPHandler(
		workerHandler.dispatcher,
		hiddenTools,
		workerHandler.exportStore,
		workerHandler.exportPrefix,
		workerHandler.exportUploadTTL,
		workerHandler.exportDownloadTTL,
		workerHandler.exportReturnSchema,
		workerHandler.computerUseSessionIDPrefix,
		mcpToolOverrides,
	)))

	api := router.Group("/api/v1")
	execAPI := api.Group("/")
	execAPI.Use(mcpAuth.RequireToken())
	execAPI.POST("/commands/echo", workerHandler.EchoCommand)
	execAPI.POST("/commands/terminal", workerHandler.TerminalCommand)
	execAPI.POST("/commands/computer-use", workerHandler.ComputerUseCommand)
	execAPI.GET("/sandbox/metadata", workerHandler.SandboxMetadata)
	execAPI.POST("/tasks", workerHandler.SubmitTask)
	execAPI.GET("/tasks/:task_id", workerHandler.GetTask)
	execAPI.POST("/tasks/:task_id/cancel", workerHandler.CancelTask)

	if consoleAuth == nil {
		api.GET("/workers", workerHandler.ListWorkers)
		api.GET("/workers/stats", workerHandler.WorkerStats)
		api.GET("/workers/inflight", workerHandler.WorkerInflight)
		api.POST("/workers", workerHandler.CreateWorker)
		api.DELETE("/workers/:node_id", workerHandler.DeleteWorker)
		if err := registerEmbeddedWebRoutes(router); err != nil {
			return nil, err
		}
		return router, nil
	}

	api.POST("/auth/login", consoleAuth.Login)
	api.POST("/auth/logout", consoleAuth.Logout)
	api.GET("/auth/session", consoleAuth.RequireAuth(apiKeyAuth), consoleAuth.Session)

	management := api.Group("/")
	management.Use(consoleAuth.RequireAuth(apiKeyAuth))
	management.POST("/auth/password", consoleAuth.RequireCookieSession(), consoleAuth.ChangePassword)
	management.GET("/api-keys", apiKeyAuth.ListAPIKeys)
	management.POST("/api-keys", consoleAuth.RequireCookieSession(), apiKeyAuth.CreateAPIKey)
	management.DELETE("/api-keys/:api_key_id", consoleAuth.RequireCookieSession(), apiKeyAuth.DeleteAPIKey)
	management.GET("/tokens", consoleAuth.RequireCookieSession(), mcpAuth.ListTokens)
	management.POST("/tokens", consoleAuth.RequireCookieSession(), mcpAuth.CreateToken)
	management.DELETE("/tokens/:token_id", consoleAuth.RequireCookieSession(), mcpAuth.DeleteToken)
	management.GET("/tokens/:token_id/value", consoleAuth.RequireCookieSession(), mcpAuth.GetTokenValue)
	management.GET("/workers", workerHandler.ListWorkers)
	management.GET("/workers/stats", workerHandler.WorkerStats)
	management.GET("/workers/inflight", workerHandler.WorkerInflight)
	management.POST("/workers", workerHandler.CreateWorker)
	management.DELETE("/workers/:node_id", workerHandler.DeleteWorker)
	management.GET("/workers/:node_id/startup-command", workerHandler.GetWorkerStartupCommand)
	management.GET("/sessions", workerHandler.ListSessions)
	management.GET("/sessions/:session_id", workerHandler.GetSession)
	management.DELETE("/sessions/:session_id", workerHandler.DeleteSession)
	if workerHandler.proxyRoutes != nil {
		management.POST("/proxy-routes", workerHandler.proxyRoutes.Create)
		management.GET("/proxy-routes", workerHandler.proxyRoutes.List)
		management.DELETE("/proxy-routes/:route_key", workerHandler.proxyRoutes.Delete)
	}

	adminManagement := api.Group("/")
	adminManagement.Use(consoleAuth.RequireAuth(apiKeyAuth), consoleAuth.RequireAdmin())
	adminManagement.POST("/accounts", consoleAuth.Register)
	adminManagement.GET("/accounts", consoleAuth.ListAccounts)
	adminManagement.DELETE("/accounts/:account_id", consoleAuth.DeleteAccount)

	// Keep previously published /api/v1/console routes as compatibility aliases.
	// Terminal session management was first published at /api/v1/sessions.
	api.POST("/console/login", consoleAuth.Login)
	api.POST("/console/logout", consoleAuth.Logout)
	api.GET("/console/session", consoleAuth.RequireAuth(apiKeyAuth), consoleAuth.Session)
	management.POST("/console/password", consoleAuth.RequireCookieSession(), consoleAuth.ChangePassword)
	management.GET("/console/api-keys", apiKeyAuth.ListAPIKeys)
	management.POST("/console/api-keys", consoleAuth.RequireCookieSession(), apiKeyAuth.CreateAPIKey)
	management.DELETE("/console/api-keys/:api_key_id", consoleAuth.RequireCookieSession(), apiKeyAuth.DeleteAPIKey)
	management.GET("/console/tokens", consoleAuth.RequireCookieSession(), mcpAuth.ListTokens)
	management.POST("/console/tokens", consoleAuth.RequireCookieSession(), mcpAuth.CreateToken)
	management.DELETE("/console/tokens/:token_id", consoleAuth.RequireCookieSession(), mcpAuth.DeleteToken)
	management.GET("/console/tokens/:token_id/value", consoleAuth.RequireCookieSession(), mcpAuth.GetTokenValue)
	adminManagement.POST("/console/register", consoleAuth.Register)
	adminManagement.GET("/console/accounts", consoleAuth.ListAccounts)
	adminManagement.DELETE("/console/accounts/:account_id", consoleAuth.DeleteAccount)

	if err := registerEmbeddedWebRoutes(router); err != nil {
		return nil, err
	}

	return router, nil
}

func (h *WorkerHandler) ListWorkers(c *gin.Context) {
	ownerID, isAdmin, ok := resolveWorkerAccessScope(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	page, ok := parsePositiveIntQuery(c, "page", 1)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page must be a positive integer"})
		return
	}
	pageSize, ok := parsePositiveIntQuery(c, "page_size", 20)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page_size must be a positive integer"})
		return
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}

	status := registry.WorkerStatus(c.DefaultQuery("status", string(registry.StatusAll)))
	if status != registry.StatusAll && status != registry.StatusOnline && status != registry.StatusOffline {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be one of all|online|offline"})
		return
	}

	var workers []registry.WorkerView
	total := 0
	if isAdmin {
		workers, total = h.store.List(status, page, pageSize, h.nowFn(), h.offlineTTL)
	} else {
		workers, total = h.store.ListScoped(
			status,
			page,
			pageSize,
			h.nowFn(),
			h.offlineTTL,
			ownerID,
			registry.WorkerTypeSys,
		)
	}
	items := make([]workerItem, 0, len(workers))
	for _, worker := range workers {
		items = append(items, workerItem{
			NodeID:       worker.NodeID,
			NodeName:     worker.NodeName,
			ExecutorKind: worker.ExecutorKind,
			Capabilities: worker.Capabilities,
			Labels:       worker.Labels,
			Version:      worker.Version,
			Status:       worker.Status,
			RegisteredAt: worker.RegisteredAt,
			LastSeenAt:   worker.LastSeenAt,
		})
	}

	c.JSON(http.StatusOK, listWorkersResponse{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

func (h *WorkerHandler) CreateWorker(c *gin.Context) {
	ownerID, isAdmin, ok := resolveWorkerAccessScope(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	if h.provisioning == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "worker provisioning is unavailable"})
		return
	}

	var req createWorkerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	workerType := strings.TrimSpace(strings.ToLower(req.Type))
	if workerType != registry.WorkerTypeNormal && workerType != registry.WorkerTypeSys {
		c.JSON(http.StatusBadRequest, gin.H{"error": "type must be one of normal|worker-sys"})
		return
	}
	if !isAdmin && workerType == registry.WorkerTypeNormal {
		c.JSON(http.StatusForbidden, gin.H{"error": "only admin can create normal worker"})
		return
	}

	nodeID, workerSecret, err := h.provisioning.CreateProvisionedWorkerForOwner(ownerID, workerType, h.nowFn(), h.offlineTTL)
	if err != nil {
		if errors.Is(err, grpcserver.ErrInvalidWorkerType) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "type must be one of normal|worker-sys"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create worker"})
		return
	}

	c.JSON(http.StatusCreated, workerStartupCommandResponse{
		NodeID:       nodeID,
		Type:         workerType,
		WorkerSecret: workerSecret,
	})
}

func (h *WorkerHandler) DeleteWorker(c *gin.Context) {
	ownerID, isAdmin, ok := resolveWorkerAccessScope(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	nodeID := strings.TrimSpace(c.Param("node_id"))
	if nodeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node_id is required"})
		return
	}
	if h.provisioning == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "worker provisioning is unavailable"})
		return
	}
	if !isAdmin {
		worker, found := h.store.GetByNodeID(nodeID, h.nowFn(), h.offlineTTL)
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "worker not found"})
			return
		}
		if strings.TrimSpace(worker.Labels[registry.LabelOwnerIDKey]) != ownerID ||
			strings.TrimSpace(strings.ToLower(worker.Labels[registry.LabelWorkerTypeKey])) != registry.WorkerTypeSys {
			c.JSON(http.StatusNotFound, gin.H{"error": "worker not found"})
			return
		}
	}
	deleted, err := h.provisioning.DeleteProvisionedWorker(nodeID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete worker"})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": "worker not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *WorkerHandler) GetWorkerStartupCommand(c *gin.Context) {
	nodeID := strings.TrimSpace(c.Param("node_id"))
	if nodeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node_id is required"})
		return
	}
	c.JSON(http.StatusGone, gin.H{
		"error": "worker secret is returned only when creating the worker; delete and recreate to get a new startup command",
	})
}

func parsePositiveIntQuery(c *gin.Context, key string, defaultValue int) (int, bool) {
	raw := c.Query(key)
	if raw == "" {
		return defaultValue, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}

func resolveWorkerAccessScope(c *gin.Context) (string, bool, bool) {
	account, ok := requestSessionAccountFromGin(c)
	if ok {
		ownerID := strings.TrimSpace(account.AccountID)
		if ownerID == "" {
			return "", false, false
		}
		return ownerID, account.IsAdmin, true
	}
	// Fallback for deployments/tests without dashboard auth.
	return "system", true, true
}
