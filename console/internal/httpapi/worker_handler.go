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
	store            *registry.Store
	offlineTTL       time.Duration
	dispatcher       CommandDispatcher
	workerSecretByID map[string]string
	consoleGRPCAddr  string
	nowFn            func() time.Time
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
	workerSecretByID map[string]string,
	consoleGRPCAddr string,
) *WorkerHandler {
	secretByIDCopy := make(map[string]string, len(workerSecretByID))
	for workerID, workerSecret := range workerSecretByID {
		secretByIDCopy[workerID] = workerSecret
	}

	return &WorkerHandler{
		store:            store,
		offlineTTL:       offlineTTL,
		dispatcher:       dispatcher,
		workerSecretByID: secretByIDCopy,
		consoleGRPCAddr:  strings.TrimSpace(consoleGRPCAddr),
		nowFn:            time.Now,
	}
}

func NewRouter(workerHandler *WorkerHandler, consoleAuth *ConsoleAuth) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	router.Any("/mcp", gin.WrapH(NewMCPHandler(workerHandler.dispatcher)))

	api := router.Group("/api/v1")
	api.POST("/commands/echo", workerHandler.EchoCommand)
	api.POST("/commands/terminal", workerHandler.TerminalCommand)
	api.POST("/tasks", workerHandler.SubmitTask)
	api.GET("/tasks/:task_id", workerHandler.GetTask)
	api.POST("/tasks/:task_id/cancel", workerHandler.CancelTask)

	if consoleAuth == nil {
		api.GET("/workers", workerHandler.ListWorkers)
		api.GET("/workers/stats", workerHandler.WorkerStats)
		return router
	}

	api.POST("/console/login", consoleAuth.Login)
	api.POST("/console/logout", consoleAuth.Logout)

	dashboard := api.Group("/")
	dashboard.Use(consoleAuth.RequireAuth())
	dashboard.GET("/workers", workerHandler.ListWorkers)
	dashboard.GET("/workers/stats", workerHandler.WorkerStats)
	dashboard.GET("/workers/:node_id/startup-command", workerHandler.GetWorkerStartupCommand)

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

func (h *WorkerHandler) GetWorkerStartupCommand(c *gin.Context) {
	nodeID := strings.TrimSpace(c.Param("node_id"))
	if nodeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node_id is required"})
		return
	}

	workerSecret, ok := h.workerSecretByID[nodeID]
	if !ok || strings.TrimSpace(workerSecret) == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "worker not found"})
		return
	}

	command := fmt.Sprintf(
		"WORKER_CONSOLE_GRPC_TARGET=%s WORKER_ID=%s WORKER_SECRET=%s WORKER_HEARTBEAT_INTERVAL_SEC=%d WORKER_HEARTBEAT_JITTER_PCT=%d go run ./cmd/worker-docker",
		resolveWorkerGRPCTarget(h.consoleGRPCAddr, c.Request),
		nodeID,
		workerSecret,
		startupCommandHeartbeatInterval,
		startupCommandHeartbeatJitter,
	)

	c.JSON(http.StatusOK, workerStartupCommandResponse{
		NodeID:  nodeID,
		Command: command,
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
