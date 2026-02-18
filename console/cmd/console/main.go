package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/config"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/httpapi"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"google.golang.org/grpc"
)

func main() {
	cfg := config.Load()
	dashboardCredentials, err := httpapi.ResolveDashboardCredentials(cfg.DashboardUsername, cfg.DashboardPassword)
	if err != nil {
		log.Fatalf("failed to resolve dashboard credentials: %v", err)
	}
	log.Printf(
		"console dashboard credentials username=%s password=%s",
		dashboardCredentials.Username,
		dashboardCredentials.Password,
	)

	store := registry.NewStore()

	registryService := grpcserver.NewRegistryService(
		store,
		nil,
		cfg.HeartbeatIntervalSec,
		int32(cfg.OfflineTTL/time.Second),
		cfg.ReplayWindow,
	)
	grpcSrv := grpcserver.NewServer(registryService)
	httpHandler := httpapi.NewWorkerHandler(
		store,
		cfg.OfflineTTL,
		registryService,
		registryService,
		cfg.GRPCAddr,
	)
	consoleAuth := httpapi.NewConsoleAuth(dashboardCredentials)
	httpSrv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: httpapi.NewRouter(httpHandler, consoleAuth),
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	go startOfflinePruner(runCtx, store, cfg.OfflineTTL)

	grpcListener, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		log.Fatalf("failed to listen gRPC on %s: %v", cfg.GRPCAddr, err)
	}
	defer grpcListener.Close()

	httpListener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		log.Fatalf("failed to listen HTTP on %s: %v", cfg.HTTPAddr, err)
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

	log.Printf("console HTTP listening on %s", httpListener.Addr().String())
	log.Printf("console gRPC listening on %s", grpcListener.Addr().String())

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-sigCtx.Done():
		log.Printf("shutdown signal received")
	case serveErr := <-errCh:
		log.Printf("server exited with error: %v", serveErr)
	}
	cancelRun()

	stopGRPCWithTimeout(grpcSrv, 5*time.Second)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown error: %v", err)
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
				log.Printf("pruned %d offline worker(s)", removed)
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
		log.Printf("gRPC graceful stop timed out after %s, forcing stop", timeout)
		grpcSrv.Stop()
		<-stopped
	}
}
