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
	if cfg.WorkerMaxCount <= 0 {
		log.Fatalf("CONSOLE_WORKER_MAX_COUNT must be a positive integer")
	}

	workerCredentials, secretByWorkerID, err := grpcserver.GenerateWorkerCredentials(cfg.WorkerMaxCount)
	if err != nil {
		log.Fatalf("failed to generate worker credentials: %v", err)
	}
	if err := grpcserver.WriteWorkerCredentialsFile(cfg.WorkerCredentialsFile, workerCredentials); err != nil {
		log.Fatalf("failed to write worker credentials: %v", err)
	}
	log.Printf("generated %d worker credential(s) to %s", len(workerCredentials), cfg.WorkerCredentialsFile)

	store := registry.NewStore()
	provisionedWorkers := make([]registry.ProvisionedWorker, 0, len(workerCredentials))
	for _, credential := range workerCredentials {
		provisionedWorkers = append(provisionedWorkers, registry.ProvisionedWorker{
			Slot:   credential.Slot,
			NodeID: credential.WorkerID,
			Labels: map[string]string{
				"source": "console-generated",
			},
		})
	}
	seeded := store.SeedProvisionedWorkers(provisionedWorkers, time.Now(), cfg.OfflineTTL)
	log.Printf("seeded %d provisioned worker slot(s) into registry state", seeded)

	registryService := grpcserver.NewRegistryService(
		store,
		secretByWorkerID,
		cfg.HeartbeatIntervalSec,
		int32(cfg.OfflineTTL/time.Second),
		cfg.ReplayWindow,
	)
	grpcSrv := grpcserver.NewServer(registryService)
	httpHandler := httpapi.NewWorkerHandler(store, cfg.OfflineTTL, registryService)
	httpSrv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: httpapi.NewRouter(httpHandler),
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
