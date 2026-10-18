package sandboxsnapshot

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestWorkerRecoverySeparatesRunningAndHeldOwnership(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	assertPlan := func(worker string, running int, held int, committed bool, route bool) {
		t.Helper()
		plan, err := r.WorkerRecovery(ctx, worker)
		if err != nil {
			t.Fatal(err)
		}
		if !plan.PersistentScopes[ScopeSessionID("owner", "session")] || len(plan.Running) != running || len(plan.Held) != held {
			t.Fatalf("invalid recovery classification: %+v", plan)
		}
		if held > 0 && (plan.Held[0].SnapshotCommitted != committed || plan.Held[0].RouteCommitted != route) {
			t.Fatalf("wrong commit phase: %+v", plan.Held)
		}
	}
	assertPlan("worker-a", 1, 0, false, false)
	op := begin(t, r, Suspend, "suspend")
	assertPlan("worker-a", 0, 1, false, false)
	verified(t, r, op, "snapshot")
	if err := r.CommitSuspend(ctx, op, "snapshot", time.Hour); err != nil {
		t.Fatal(err)
	}
	assertPlan("worker-a", 0, 1, true, false)
	if err := r.Finish(ctx, op); err != nil {
		t.Fatal(err)
	}
	assertPlan("worker-a", 0, 0, false, false)
	wake := begin(t, r, Resume, "resume")
	if err := r.SetRestoreTarget(ctx, wake, "worker-b"); err != nil {
		t.Fatal(err)
	}
	assertPlan("worker-b", 0, 1, false, false)
	if err := r.CommitRestore(ctx, wake, "worker-b", time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	assertPlan("worker-b", 0, 1, false, true)
	if err := r.FailAndHold(ctx, wake, "timeout", "activation reply lost"); err != nil {
		t.Fatal(err)
	}
	assertPlan("worker-b", 0, 1, false, true)
	if err := r.ReconcileActivation(ctx, wake); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, wake); err != nil {
		t.Fatal(err)
	}
	assertPlan("worker-b", 1, 0, false, false)
}

func TestWorkerRecoveryNeverOmitsTargetDuringActivationFinish(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	suspend(t, r, "suspend", "snapshot")
	op := begin(t, r, Resume, "resume")
	if err := r.SetRestoreTarget(ctx, op, "worker-b"); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitRestore(ctx, op, "worker-b", time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if err := r.Finish(ctx, op); err != nil {
			t.Error(err)
		}
	}()
	close(start)
	for i := 0; i < 50; i++ {
		plan, err := r.WorkerRecovery(ctx, "worker-b")
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Running)+len(plan.Held) != 1 {
			t.Fatalf("activation transition exposed target to orphan cleanup: %+v", plan)
		}
	}
	wg.Wait()
}
