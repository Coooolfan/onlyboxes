package grpcserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/api/proxytoken"
	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
)

func TestPreviewTokensFollowDurableGenerationAndOwner(t *testing.T) {
	for _, worker := range []string{"worker-a", "worker-b"} {
		t.Run(worker, func(t *testing.T) {
			svc, source, target, scopedID := setupSnapshotRouteTest(t)
			ctx := context.Background()
			mint := func(worker string, key []byte, generation int64) string {
				t.Helper()
				now := time.Now()
				auth, err := svc.AuthorizeProxyRoute(ctx, worker, scopedID, 8080, now.Add(time.Hour), now)
				if err != nil {
					t.Fatal(err)
				}
				claims, err := proxytoken.Verify(key, auth.Token, now)
				if err != nil || claims.Generation != generation || claims.WorkerID != worker || claims.SessionID != scopedID {
					t.Fatalf("wrong signed execution identity: %+v %v", claims, err)
				}
				return auth.Token
			}
			closed := func(worker string) {
				t.Helper()
				if _, err := svc.AuthorizeProxyRoute(ctx, worker, scopedID, 8080, time.Now().Add(time.Hour), time.Now()); !errors.Is(err, ErrProxySessionNotFound) {
					t.Fatalf("inactive or stale route signed a token: %v", err)
				}
			}
			oldToken := mint("worker-a", source.routeTokenKey, 1)
			suspend := beginRouteSnapshotTest(t, svc, sandboxsnapshot.Suspend, "suspend")
			closed("worker-a")
			commitRouteSnapshotTest(t, svc, suspend)
			closed("worker-a")
			resume := beginRouteSnapshotTest(t, svc, sandboxsnapshot.Resume, "resume")
			if err := svc.snapshotRepository.SetRestoreTarget(ctx, resume, worker); err != nil {
				t.Fatal(err)
			}
			if err := svc.snapshotRepository.CommitRestore(ctx, resume, worker, time.Now().Add(time.Hour).UnixMilli()); err != nil {
				t.Fatal(err)
			}
			if err := svc.RefreshSnapshotRoute(ctx, "owner", "session"); err != nil {
				t.Fatal(err)
			}
			closed(worker)
			if err := svc.snapshotRepository.Finish(ctx, resume); err != nil {
				t.Fatal(err)
			}
			active := source
			if worker == "worker-b" {
				active = target
			}
			newToken := mint(worker, active.routeTokenKey, resume.Generation)
			if oldToken == newToken || resume.Generation <= 1 {
				t.Fatal("restore reused old preview identity")
			}
			if worker == "worker-b" {
				// Simulate a stale cached source route after the durable owner changed.
				svc.terminalRoutesMu.Lock()
				stale := svc.terminalSessionToNode[scopedID]
				stale.NodeID = "worker-a"
				svc.terminalSessionToNode[scopedID] = stale
				svc.terminalRoutesMu.Unlock()
				closed("worker-a")
				if _, err := svc.ResolveProxySession("owner", "session", time.Now()); !errors.Is(err, ErrProxySessionNotFound) {
					t.Fatalf("stale cache overrode durable owner: %v", err)
				}
				if err := svc.RefreshSnapshotRoute(ctx, "owner", "session"); err != nil {
					t.Fatal(err)
				}
				mint(worker, active.routeTokenKey, resume.Generation)
			}
			if _, err := svc.ResolveProxySession("another-owner", "session", time.Now()); !errors.Is(err, ErrProxySessionNotFound) {
				t.Fatalf("cross-account preview: %v", err)
			}
		})
	}
}

func TestPreviewCannotFallBackToOrdinaryGenerationOnDatabaseFailure(t *testing.T) {
	svc, _, _, scopedID := setupSnapshotRouteTest(t)
	if err := svc.store.Persistence().Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthorizeProxyRoute(context.Background(), "worker-a", scopedID, 8080, time.Now().Add(time.Hour), time.Now()); !errors.Is(err, ErrProxySessionNotFound) {
		t.Fatalf("unreadable lifecycle gate signed a token: %v", err)
	}
}

func TestExpiredPreviewCacheCannotEraseDurableExecutionLease(t *testing.T) {
	svc, _, _, scopedID := setupSnapshotRouteTest(t)
	ctx := context.Background()
	before, err := svc.snapshotRepository.ExecutionRoute(ctx, scopedID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	svc.lastTerminalRoutePruneUnixMs.Store(now.UnixMilli())
	svc.terminalRoutesMu.Lock()
	stale := svc.terminalSessionToNode[scopedID]
	stale.LeaseExpiresUnixMs = now.Add(-time.Second).UnixMilli()
	svc.terminalSessionToNode[scopedID] = stale
	svc.terminalRoutesMu.Unlock()
	if _, err := svc.AuthorizeProxyRoute(ctx, "worker-a", scopedID, 8080, now.Add(time.Hour), now); !errors.Is(err, ErrProxySessionNotFound) {
		t.Fatalf("expired preview cache signed a token: %v", err)
	}
	after, err := svc.snapshotRepository.ExecutionRoute(ctx, scopedID)
	if err != nil || after != before {
		t.Fatalf("stale preview cache erased active durable execution route: %+v %v", after, err)
	}
}
