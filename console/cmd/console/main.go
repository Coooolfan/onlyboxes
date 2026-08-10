package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/config"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/httpapi"
	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"google.golang.org/grpc"
)

func main() {
	cfg := config.Load()
	slog.SetDefault(newLogger(cfg))
	if cfg.ConfigFile != "" {
		slog.Info("config file loaded", "path", cfg.ConfigFile)
	}

	dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dbCancel()
	db, err := persistence.Open(dbCtx, persistence.Options{
		Path:             cfg.DBPath,
		BusyTimeoutMS:    cfg.DBBusyTimeoutMS,
		HashKey:          cfg.HashKey,
		TaskRetentionDay: cfg.TaskRetentionDays,
	})
	if err != nil {
		fatal("failed to initialize persistence", "error", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			slog.Error("failed to close database", "error", closeErr)
		}
	}()

	adminAccount, err := httpapi.InitializeAdminAccount(
		context.Background(),
		db,
		cfg.DashboardUsername,
		cfg.DashboardPassword,
		cfg.InitialAdminAPIKey,
	)
	if err != nil {
		fatal("failed to initialize admin account", "error", err)
	}
	if adminAccount.EnvIgnored {
		slog.Info("initial admin bootstrap env ignored because persisted admin account exists")
	}
	if adminAccount.InitializedNow {
		attrs := []any{
			"username", adminAccount.Username,
			"password", adminAccount.PasswordPlaintext,
		}
		if adminAccount.APIKeyInitialized {
			attrs = append(
				attrs,
				"api_key_name", adminAccount.APIKeyName,
				"api_key", adminAccount.APIKeyPlaintext,
			)
		}
		slog.Info("console admin account initialized", attrs...)
	} else {
		slog.Info(
			"console admin account loaded",
			"username",
			adminAccount.Username,
		)
	}

	store, err := registry.NewStoreWithPersistence(db)
	if err != nil {
		fatal("failed to initialize registry store", "error", err)
	}
	initialCredentialHashes := store.ListCredentialHashes()

	registryService := grpcserver.NewRegistryService(
		store,
		initialCredentialHashes,
		cfg.HeartbeatIntervalSec,
		int32(cfg.OfflineTTL/time.Second),
		cfg.ReplayWindow,
	)
	registryService.SetHasher(db.Hasher)
	registryService.SetTaskRetention(time.Duration(cfg.TaskRetentionDays) * 24 * time.Hour)
	registryService.ConfigureProxy(cfg.ProxyEnabled, cfg.ProxyAllowedWorkerCIDRs, cfg.ProxyAllowedWorkerPorts, cfg.ProxyAllowedDirectDomains)
	restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := registryService.RestoreTerminalSessionRoutes(restoreCtx, time.Now()); err != nil {
		restoreCancel()
		fatal("failed to restore terminal session routes", "error", err)
	}
	restoreCancel()
	grpcSrv := grpcserver.NewServer(registryService)
	httpHandler := httpapi.NewWorkerHandler(
		store,
		cfg.OfflineTTL,
		registryService,
		registryService,
		registryService,
		cfg.GRPCAddr,
	)
	var proxyRouteHandler *httpapi.ProxyRouteHandler
	if cfg.ProxyEnabled {
		if len(cfg.ProxyAllowedDirectDomains) == 0 {
			fatal("CONSOLE_PROXY_ALLOWED_DIRECT_DOMAINS must contain at least one valid domain when proxy is enabled")
		}
		proxyRouteHandler, err = httpapi.NewProxyRouteHandler(
			registryService,
			store,
			cfg.ProxyPublicBaseDomain,
			cfg.ProxyPublicScheme,
			cfg.ProxyInternalAuthToken,
			cfg.ProxyRouteTTL,
			cfg.ProxyRouteKeyLength,
			cfg.ProxyRouteMaxPerAccount,
			cfg.ProxyRouteMaxPerSession,
		)
		if err != nil {
			fatal("failed to initialize proxy routes", "error", err)
		}
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := proxyRouteHandler.Restore(restoreCtx, time.Now()); err != nil {
			restoreCancel()
			fatal("failed to restore proxy routes", "error", err)
		}
		restoreCancel()
		httpHandler.SetProxyRouteHandler(proxyRouteHandler)
	}
	if cfg.ExportFileEnabled() {
		exportStore, err := objectstore.New(objectstore.Config{
			Endpoint:   cfg.ExportFileEndpoint,
			Region:     cfg.ExportFileRegion,
			BucketName: cfg.ExportFileBucketName,
			AccessKey:  cfg.ExportFileAK,
			SecretKey:  cfg.ExportFileSK,
		})
		if err != nil {
			fatal("failed to initialize export objectstore", "error", err)
		}
		httpHandler.SetExportStore(
			exportStore,
			cfg.ExportFilePrefix,
			cfg.ExportFileUploadTTL,
			cfg.ExportFileDownloadTTL,
			cfg.ExportReturnSchema,
		)
	}
	consoleAuth, err := httpapi.NewConsoleAuth(db.Queries, cfg.EnableRegistration)
	if err != nil {
		fatal("failed to initialize console auth", "error", err)
	}
	consoleAuth.SetPersistenceDB(db)
	if proxyRouteHandler != nil {
		consoleAuth.SetProxyRouteRevoker(proxyRouteHandler)
	}
	if trimmedDashboardKey := strings.TrimSpace(cfg.DashboardJITSigningKey); trimmedDashboardKey != "" {
		if trimmedDashboardKey == strings.TrimSpace(cfg.JITSigningKey) {
			fatal("CONSOLE_DASHBOARD_JIT_SIGNING_KEY must differ from CONSOLE_JIT_SIGNING_KEY")
		}
		consoleAuth.SetDashboardJITSigningKey(trimmedDashboardKey)
	}
	mcpAuth, err := httpapi.NewMCPAuthWithPersistence(db)
	if err != nil {
		fatal("failed to initialize mcp auth", "error", err)
	}
	mcpAuth.SetJITSigningKey(cfg.JITSigningKey)
	mcpAuth.SetTokenQueryParam(cfg.MCPTokenQueryParam)
	apiKeyAuth, err := httpapi.NewAPIKeyAuth(db)
	if err != nil {
		fatal("failed to initialize api key auth", "error", err)
	}
	router, err := httpapi.NewRouter(httpHandler, consoleAuth, mcpAuth, apiKeyAuth, cfg.HiddenTools, cfg.MCPToolOverrides)
	if err != nil {
		fatal("failed to initialize http router", "error", err)
	}
	httpSrv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: router,
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	go startOfflinePruner(runCtx, store, cfg.OfflineTTL)
	go startTaskPruner(runCtx, registryService)
	if proxyRouteHandler != nil {
		go startProxyRoutePruner(runCtx, proxyRouteHandler)
	}

	grpcListener, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		fatal("failed to listen gRPC", "addr", cfg.GRPCAddr, "error", err)
	}
	defer grpcListener.Close()

	httpListener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		fatal("failed to listen HTTP", "addr", cfg.HTTPAddr, "error", err)
	}
	defer httpListener.Close()

	errCh := make(chan error, 2)
	go func() {
		if serveErr := grpcSrv.Serve(grpcListener); serveErr != nil {
			reportServeErr(runCtx, errCh, serveErr)
		}
	}()
	go func() {
		if serveErr := httpSrv.Serve(httpListener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			reportServeErr(runCtx, errCh, serveErr)
		}
	}()

	slog.Info("console HTTP listening", "addr", httpListener.Addr().String())
	slog.Info("console gRPC listening", "addr", grpcListener.Addr().String())

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-sigCtx.Done():
		slog.Info("shutdown signal received")
	case serveErr := <-errCh:
		slog.Error("server exited with error", "error", serveErr)
	}
	cancelRun()

	stopGRPCWithTimeout(grpcSrv, 5*time.Second)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http shutdown error", "error", err)
	}
}

func reportServeErr(runCtx context.Context, errCh chan<- error, err error) {
	select {
	case errCh <- err:
	case <-runCtx.Done():
	}
}

func startOfflinePruner(ctx context.Context, store *registry.Store, offlineTTL time.Duration) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			removed := store.PruneOffline(now, offlineTTL)
			if removed > 0 {
				slog.Info("pruned offline workers", "removed", removed)
			}
		}
	}
}

func stopGRPCWithTimeout(grpcSrv *grpc.Server, timeout time.Duration) {
	stopped := make(chan struct{})
	go func() {
		grpcSrv.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(timeout):
		slog.Warn("gRPC graceful stop timed out, forcing stop", "timeout", timeout)
		grpcSrv.Stop()
		<-stopped
	}
}

func startTaskPruner(ctx context.Context, service *grpcserver.RegistryService) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			removed := service.PruneExpiredTasks(now)
			if removed > 0 {
				slog.Info("pruned expired tasks", "removed", removed)
			}
		}
	}
}

func startProxyRoutePruner(ctx context.Context, handler *httpapi.ProxyRouteHandler) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			pruneCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			removed, err := handler.PruneExpired(pruneCtx, now)
			cancel()
			if err != nil {
				slog.Warn("failed to prune expired proxy routes", "error", err)
				continue
			}
			if removed > 0 {
				slog.Info("pruned expired proxy routes", "removed", removed)
			}
		}
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	options := &slog.HandlerOptions{
		Level:     level,
		AddSource: cfg.LogAddSource,
	}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, options))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, options))
}

func fatal(message string, attrs ...any) {
	slog.Error(message, attrs...)
	os.Exit(1)
}
