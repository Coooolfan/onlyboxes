package registry

import (
	"fmt"
	"sync"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

func TestStoreRegisterOverwrite(t *testing.T) {
	store := NewStore()
	start := time.Unix(1_700_000_000, 0)

	store.Upsert(&registryv1.ConnectHello{
		NodeId:       "node-1",
		NodeName:     "node-a",
		ExecutorKind: "docker",
		Capabilities: []*registryv1.CapabilityDeclaration{{
			Name: "echo",
		}},
		Labels:  map[string]string{"zone": "a"},
		Version: "v1",
	}, "session-a", start)

	store.Upsert(&registryv1.ConnectHello{
		NodeId:       "node-1",
		NodeName:     "node-b",
		ExecutorKind: "docker",
		Capabilities: []*registryv1.CapabilityDeclaration{{
			Name: "build",
		}},
		Labels:  map[string]string{"zone": "b"},
		Version: "v2",
	}, "session-b", start.Add(10*time.Second))

	items, total := store.List(StatusAll, 1, 10, start.Add(10*time.Second), 15*time.Second)
	if total != 1 || len(items) != 1 {
		t.Fatalf("expected one worker, got total=%d len=%d", total, len(items))
	}
	if items[0].NodeName != "node-b" {
		t.Fatalf("expected latest node name, got %s", items[0].NodeName)
	}
	if items[0].SessionID != "session-b" {
		t.Fatalf("expected latest session, got %s", items[0].SessionID)
	}
	if !items[0].RegisteredAt.Equal(start.Add(10 * time.Second)) {
		t.Fatalf("expected registered_at to reset to latest registration")
	}
	if !items[0].LastSeenAt.Equal(start.Add(10 * time.Second)) {
		t.Fatalf("expected last_seen_at to update on upsert")
	}
}

func TestStoreHeartbeatAndOfflineStatus(t *testing.T) {
	store := NewStore()
	start := time.Unix(1_700_000_100, 0)
	store.Upsert(&registryv1.ConnectHello{NodeId: "node-1", NodeName: "n1"}, "session-1", start)

	if err := store.TouchWithSession("node-1", "session-1", start.Add(5*time.Second)); err != nil {
		t.Fatalf("touch should succeed: %v", err)
	}
	if err := store.TouchWithSession("missing", "session-x", start.Add(5*time.Second)); err != ErrNodeNotFound {
		t.Fatalf("expected ErrNodeNotFound, got %v", err)
	}
	if err := store.TouchWithSession("node-1", "session-x", start.Add(5*time.Second)); err != ErrSessionMismatch {
		t.Fatalf("expected ErrSessionMismatch, got %v", err)
	}

	onlineItems, _ := store.List(StatusOnline, 1, 10, start.Add(10*time.Second), 15*time.Second)
	if len(onlineItems) != 1 || onlineItems[0].Status != StatusOnline {
		t.Fatalf("expected one online worker")
	}
	offlineItems, _ := store.List(StatusOffline, 1, 10, start.Add(25*time.Second), 15*time.Second)
	if len(offlineItems) != 1 || offlineItems[0].Status != StatusOffline {
		t.Fatalf("expected one offline worker")
	}
}

func TestStoreConcurrentAccess(t *testing.T) {
	store := NewStore()
	base := time.Unix(1_700_000_200, 0)

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			nodeID := fmt.Sprintf("node-%d", i%8)
			for j := 0; j < 100; j++ {
				now := base.Add(time.Duration(i*j) * time.Millisecond)
				sessionID := fmt.Sprintf("session-%d", i)
				store.Upsert(&registryv1.ConnectHello{NodeId: nodeID, NodeName: nodeID}, sessionID, now)
				_ = store.TouchWithSession(nodeID, sessionID, now.Add(time.Millisecond))
				_, _ = store.List(StatusAll, 1, 10, now.Add(5*time.Second), 15*time.Second)
			}
		}(i)
	}
	wg.Wait()

	_, total := store.List(StatusAll, 1, 100, base.Add(10*time.Second), 15*time.Second)
	if total == 0 {
		t.Fatalf("expected workers to exist after concurrent writes")
	}
}

func TestStoreStats(t *testing.T) {
	store := NewStore()
	now := time.Unix(1_700_001_000, 0)

	store.Upsert(&registryv1.ConnectHello{NodeId: "online-node", NodeName: "online-node"}, "session-online", now.Add(-5*time.Second))
	store.Upsert(&registryv1.ConnectHello{NodeId: "offline-node-a", NodeName: "offline-node-a"}, "session-a", now.Add(-20*time.Second))
	store.Upsert(&registryv1.ConnectHello{NodeId: "offline-node-b", NodeName: "offline-node-b"}, "session-b", now.Add(-40*time.Second))

	stats := store.Stats(now, 15*time.Second, 30*time.Second)
	if stats.Total != 3 {
		t.Fatalf("expected total=3, got %d", stats.Total)
	}
	if stats.Online != 1 {
		t.Fatalf("expected online=1, got %d", stats.Online)
	}
	if stats.Offline != 2 {
		t.Fatalf("expected offline=2, got %d", stats.Offline)
	}
	if stats.Stale != 1 {
		t.Fatalf("expected stale=1, got %d", stats.Stale)
	}
}

func TestStorePruneOffline(t *testing.T) {
	store := NewStore()
	now := time.Unix(1_700_002_000, 0)

	store.Upsert(&registryv1.ConnectHello{NodeId: "online-node", NodeName: "online-node"}, "session-online", now.Add(-5*time.Second))
	store.Upsert(&registryv1.ConnectHello{NodeId: "offline-node-a", NodeName: "offline-node-a"}, "session-a", now.Add(-20*time.Second))
	store.Upsert(&registryv1.ConnectHello{NodeId: "offline-node-b", NodeName: "offline-node-b"}, "session-b", now.Add(-60*time.Second))

	removed := store.PruneOffline(now, 15*time.Second)
	if removed != 2 {
		t.Fatalf("expected removed=2, got %d", removed)
	}

	items, total := store.List(StatusAll, 1, 10, now, 15*time.Second)
	if total != 1 || len(items) != 1 {
		t.Fatalf("expected one worker left, got total=%d len=%d", total, len(items))
	}
	if items[0].NodeID != "online-node" {
		t.Fatalf("expected online-node to remain, got %s", items[0].NodeID)
	}
}

func TestStoreSeedProvisionedWorkersCountsAsOffline(t *testing.T) {
	store := NewStore()
	now := time.Unix(1_700_003_000, 0)

	seeded := store.SeedProvisionedWorkers([]ProvisionedWorker{
		{Slot: 1, NodeID: "seed-node-1", Labels: map[string]string{"region": "us"}},
		{Slot: 2, NodeID: "seed-node-2"},
	}, now, 15*time.Second)
	if seeded != 2 {
		t.Fatalf("expected seeded=2, got %d", seeded)
	}

	stats := store.Stats(now, 15*time.Second, 30*time.Second)
	if stats.Total != 2 {
		t.Fatalf("expected total=2, got %d", stats.Total)
	}
	if stats.Offline != 2 {
		t.Fatalf("expected offline=2, got %d", stats.Offline)
	}
	if stats.Online != 0 {
		t.Fatalf("expected online=0, got %d", stats.Online)
	}
}

func TestStorePruneOfflineKeepsProvisionedSlots(t *testing.T) {
	store := NewStore()
	now := time.Unix(1_700_004_000, 0)

	store.SeedProvisionedWorkers([]ProvisionedWorker{
		{Slot: 1, NodeID: "seed-node-1"},
	}, now, 15*time.Second)
	store.Upsert(&registryv1.ConnectHello{NodeId: "offline-runtime-node"}, "session-offline", now.Add(-30*time.Second))

	removed := store.PruneOffline(now, 15*time.Second)
	if removed != 1 {
		t.Fatalf("expected removed=1 runtime node, got %d", removed)
	}

	items, total := store.List(StatusAll, 1, 10, now, 15*time.Second)
	if total != 1 || len(items) != 1 {
		t.Fatalf("expected one provisioned worker left, got total=%d len=%d", total, len(items))
	}
	if items[0].NodeID != "seed-node-1" {
		t.Fatalf("expected seed-node-1 to remain, got %s", items[0].NodeID)
	}
	if !items[0].Provisioned {
		t.Fatalf("expected provisioned flag to be true")
	}
}

func TestStoreListOnlineByCapability(t *testing.T) {
	store := NewStore()
	now := time.Unix(1_700_005_000, 0)

	store.Upsert(&registryv1.ConnectHello{
		NodeId:       "echo-node",
		NodeName:     "echo-node",
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: "echo"}},
	}, "session-echo", now.Add(-2*time.Second))
	store.Upsert(&registryv1.ConnectHello{
		NodeId:       "build-node",
		NodeName:     "build-node",
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: "build"}},
	}, "session-build", now.Add(-2*time.Second))
	store.Upsert(&registryv1.ConnectHello{
		NodeId:       "offline-echo-node",
		NodeName:     "offline-echo-node",
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: "echo"}},
	}, "session-offline", now.Add(-40*time.Second))

	workers := store.ListOnlineByCapability("echo", now, 15*time.Second)
	if len(workers) != 1 {
		t.Fatalf("expected one online echo worker, got %d", len(workers))
	}
	if workers[0].NodeID != "echo-node" {
		t.Fatalf("expected echo-node, got %s", workers[0].NodeID)
	}
}
