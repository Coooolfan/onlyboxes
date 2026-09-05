package grpcserver

import (
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

func TestSysCapabilityMaxInflightFallsBackToOne(t *testing.T) {
	for _, declared := range []int{0, -1} {
		if got := sysCapabilityMaxInflight(declared); got != 1 {
			t.Fatalf("declared %d: expected 1, got %d", declared, got)
		}
	}
	if got := sysCapabilityMaxInflight(4); got != 4 {
		t.Fatalf("expected declared value to be kept, got %d", got)
	}
}

// worker-sys has a pinned capability allowlist, but the per-capability concurrency is
// the worker's own configuration and must survive the hello rewrite.
func TestResolveHelloByWorkerTypeKeepsSysDeclaredMaxInflight(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_000_100, 0)
	seeded := store.SeedProvisionedWorkers([]registry.ProvisionedWorker{
		{
			NodeID: "node-sys",
			Labels: map[string]string{
				registry.LabelOwnerIDKey:    "owner-a",
				registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
			},
		},
	}, now, 15*time.Second)
	if seeded != 1 {
		t.Fatalf("expected one seeded worker, got %d", seeded)
	}

	svc := NewRegistryService(store, map[string]string{"node-sys": "secret-sys"}, 5, 15)

	resolved, err := svc.resolveHelloByWorkerType(&registryv1.ConnectHello{
		NodeId: "node-sys",
		Capabilities: []*registryv1.CapabilityDeclaration{
			{Name: computerUseCapabilityDeclared, MaxInflight: 6},
			{Name: readImageCapabilityDeclared, MaxInflight: 3},
		},
	})
	if err != nil {
		t.Fatalf("resolve hello failed: %v", err)
	}

	got := map[string]int32{}
	for _, capability := range resolved.GetCapabilities() {
		got[capability.GetName()] = capability.GetMaxInflight()
	}
	if got[computerUseCapabilityDeclared] != 6 {
		t.Fatalf("expected computerUse max_inflight 6, got %d", got[computerUseCapabilityDeclared])
	}
	if got[readImageCapabilityDeclared] != 3 {
		t.Fatalf("expected readImage max_inflight 3, got %d", got[readImageCapabilityDeclared])
	}
}

func TestResolveHelloByWorkerTypeDefaultsSysMaxInflight(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_000_110, 0)
	store.SeedProvisionedWorkers([]registry.ProvisionedWorker{
		{
			NodeID: "node-sys",
			Labels: map[string]string{
				registry.LabelOwnerIDKey:    "owner-a",
				registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
			},
		},
	}, now, 15*time.Second)

	svc := NewRegistryService(store, map[string]string{"node-sys": "secret-sys"}, 5, 15)

	resolved, err := svc.resolveHelloByWorkerType(&registryv1.ConnectHello{
		NodeId: "node-sys",
		Capabilities: []*registryv1.CapabilityDeclaration{
			{Name: computerUseCapabilityDeclared},
			{Name: readImageCapabilityDeclared},
		},
	})
	if err != nil {
		t.Fatalf("resolve hello failed: %v", err)
	}

	for _, capability := range resolved.GetCapabilities() {
		if capability.GetMaxInflight() != 1 {
			t.Fatalf("%s: expected default 1, got %d", capability.GetName(), capability.GetMaxInflight())
		}
	}
}
