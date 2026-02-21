package httpapi

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
)

const (
	maxPageSize                     = 100
	defaultWorkerGRPCHost           = "127.0.0.1"
	defaultWorkerGRPCPort           = "50051"
	startupCommandHeartbeatInterval = 5
	startupCommandHeartbeatJitter   = 20
)

type WorkerHandler struct {
	store           *registry.Store
	offlineTTL      time.Duration
	dispatcher      CommandDispatcher
	provisioning    WorkerProvisioning
	inflightStats   InflightStatsProvider
	consoleGRPCAddr string
	nowFn           func() time.Time
}

type WorkerProvisioning interface {
	CreateProvisionedWorker(now time.Time, offlineTTL time.Duration) (string, string, error)
	DeleteProvisionedWorker(nodeID string) bool
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
	NodeID  string `json:"node_id"`
	Command string `json:"command"`
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
		store:           store,
		offlineTTL:      offlineTTL,
		dispatcher:      dispatcher,
		provisioning:    provisioning,
		inflightStats:   inflightStats,
		consoleGRPCAddr: strings.TrimSpace(consoleGRPCAddr),
		nowFn:           time.Now,
	}
}

func NewRouter(workerHandler *WorkerHandler, consoleAuth *ConsoleAuth, mcpAuth *MCPAuth) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	if mcpAuth == nil {
		panic("mcp auth is required")
	}
	router.Any("/mcp", mcpAuth.RequireToken(), gin.WrapH(NewMCPHandler(workerHandler.dispatcher)))

	api := router.Group("/api/v1")
	execAPI := api.Group("/")
	execAPI.Use(mcpAuth.RequireToken())
	execAPI.POST("/commands/echo", workerHandler.EchoCommand)
	execAPI.POST("/commands/terminal", workerHandler.TerminalCommand)
	execAPI.POST("/tasks", workerHandler.SubmitTask)
	execAPI.GET("/tasks/:task_id", workerHandler.GetTask)
	execAPI.POST("/tasks/:task_id/cancel", workerHandler.CancelTask)

	if consoleAuth == nil {
		api.GET("/workers", workerHandler.ListWorkers)
		api.GET("/workers/stats", workerHandler.WorkerStats)
		api.GET("/workers/inflight", workerHandler.WorkerInflight)
		api.POST("/workers", workerHandler.CreateWorker)
		api.DELETE("/workers/:node_id", workerHandler.DeleteWorker)
		registerEmbeddedWebRoutes(router)
		return router
	}

	api.POST("/console/login", consoleAuth.Login)
	api.POST("/console/logout", consoleAuth.Logout)
	api.GET("/console/session", consoleAuth.RequireAuth(), consoleAuth.Session)

	dashboard := api.Group("/")
	dashboard.Use(consoleAuth.RequireAuth())
	dashboard.GET("/console/tokens", mcpAuth.ListTokens)
	dashboard.POST("/console/tokens", mcpAuth.CreateToken)
	dashboard.DELETE("/console/tokens/:token_id", mcpAuth.DeleteToken)
	dashboard.GET("/console/tokens/:token_id/value", mcpAuth.GetTokenValue)
	dashboard.POST("/console/register", consoleAuth.RequireAdmin(), consoleAuth.Register)

	adminDashboard := api.Group("/")
	adminDashboard.Use(consoleAuth.RequireAuth(), consoleAuth.RequireAdmin())
	adminDashboard.GET("/workers", workerHandler.ListWorkers)
	adminDashboard.GET("/workers/stats", workerHandler.WorkerStats)
	adminDashboard.GET("/workers/inflight", workerHandler.WorkerInflight)
	adminDashboard.POST("/workers", workerHandler.CreateWorker)
	adminDashboard.DELETE("/workers/:node_id", workerHandler.DeleteWorker)
	adminDashboard.GET("/workers/:node_id/startup-command", workerHandler.GetWorkerStartupCommand)

	registerEmbeddedWebRoutes(router)

	return router
}

func (h *WorkerHandler) ListWorkers(c *gin.Context) {
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

	workers, total := h.store.List(status, page, pageSize, h.nowFn(), h.offlineTTL)
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
	if h.provisioning == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "worker provisioning is unavailable"})
		return
	}

	nodeID, workerSecret, err := h.provisioning.CreateProvisionedWorker(h.nowFn(), h.offlineTTL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create worker"})
		return
	}

	c.JSON(http.StatusCreated, workerStartupCommandResponse{
		NodeID:  nodeID,
		Command: h.buildWorkerStartupCommand(nodeID, workerSecret, c.Request),
	})
}

func (h *WorkerHandler) DeleteWorker(c *gin.Context) {
	nodeID := strings.TrimSpace(c.Param("node_id"))
	if nodeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node_id is required"})
		return
	}
	if h.provisioning == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "worker provisioning is unavailable"})
		return
	}
	if !h.provisioning.DeleteProvisionedWorker(nodeID) {
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

func (h *WorkerHandler) buildWorkerStartupCommand(nodeID string, workerSecret string, req *http.Request) string {
	return fmt.Sprintf(
		"WORKER_CONSOLE_GRPC_TARGET=%s WORKER_ID=%s WORKER_SECRET=%s WORKER_HEARTBEAT_INTERVAL_SEC=%d WORKER_HEARTBEAT_JITTER_PCT=%d ./path-to-binary",
		resolveWorkerGRPCTarget(h.consoleGRPCAddr, req),
		nodeID,
		workerSecret,
		startupCommandHeartbeatInterval,
		startupCommandHeartbeatJitter,
	)
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

func resolveWorkerGRPCTarget(consoleGRPCAddr string, req *http.Request) string {
	rawAddr := strings.TrimSpace(consoleGRPCAddr)

	host, port := parseAddrHostPort(rawAddr)
	if port == "" {
		port = defaultWorkerGRPCPort
	}
	if host == "" || isWildcardHost(host) {
		host = parseRequestHost(req)
	}
	if host == "" || isWildcardHost(host) {
		host = defaultWorkerGRPCHost
	}

	return net.JoinHostPort(host, port)
}

func parseAddrHostPort(addr string) (string, string) {
	if addr == "" {
		return "", ""
	}

	if strings.HasPrefix(addr, ":") {
		return "", strings.TrimPrefix(addr, ":")
	}

	host, port, err := net.SplitHostPort(addr)
	if err == nil {
		return strings.TrimSpace(host), strings.TrimSpace(port)
	}

	if _, convErr := strconv.Atoi(addr); convErr == nil {
		return "", addr
	}
	return "", ""
}

func parseRequestHost(req *http.Request) string {
	if req == nil {
		return ""
	}

	rawHost := strings.TrimSpace(req.Host)
	if rawHost == "" {
		return ""
	}

	host, _, err := net.SplitHostPort(rawHost)
	if err == nil {
		return strings.TrimSpace(host)
	}

	if strings.HasPrefix(rawHost, "[") && strings.Contains(rawHost, "]") {
		trimmed := strings.TrimPrefix(rawHost, "[")
		trimmed = strings.SplitN(trimmed, "]", 2)[0]
		return strings.TrimSpace(trimmed)
	}

	return rawHost
}

func isWildcardHost(host string) bool {
	trimmed := strings.TrimSpace(host)
	return trimmed == "" || trimmed == "0.0.0.0" || trimmed == "::"
}
