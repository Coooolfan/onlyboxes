package runtimelease

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBackend struct {
	mu               sync.Mutex
	running, missing map[string]bool
	stops            map[string]int
	block            string
	reached, release chan struct{}
	fail             bool
}

func backend() *fakeBackend {
	return &fakeBackend{running: map[string]bool{}, missing: map[string]bool{}, stops: map[string]int{}}
}
func (b *fakeBackend) Inspect(ctx context.Context, e Entry) (bool, bool, error) {
	if e.ContainerID == b.block {
		select {
		case b.reached <- struct{}{}:
		default:
		}
		select {
		case <-b.release:
		case <-ctx.Done():
			return false, false, ctx.Err()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.missing[e.ContainerID], b.running[e.ContainerID], nil
}
func (b *fakeBackend) Stop(_ context.Context, e Entry) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail {
		return ErrUnavailable
	}
	b.stops[e.ContainerID]++
	b.running[e.ContainerID] = false
	return nil
}
func lease(id string) Entry {
	return Entry{ContainerID: strings.Repeat(id, 64), Scope: "obx:owner:" + id, Generation: 1, Revision: 1, ExpiresUnixMS: time.Now().Add(time.Minute).UnixMilli()}
}
func privateDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}
func openTest(t *testing.T, b *fakeBackend) *Guard {
	t.Helper()
	g, err := Open(privateDir(t), "worker-test", b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	return g
}
func applyTest(t *testing.T, g *Guard, action string, e Entry) Entry {
	t.Helper()
	got, err := g.Apply(context.Background(), Request{Action: action, ExpectedRevision: e.Revision - 1, Entry: e})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestLeaseExpiresOnMonotonicTimeDespiteWallClockRollback(t *testing.T) {
	b := backend()
	g := openTest(t, b)
	wall := time.Now().UnixMilli()
	mono := time.Now()
	g.wall = func() int64 { return wall }
	g.mono = func() time.Time { return mono }
	e := lease("a")
	e.ExpiresUnixMS = wall + 1000
	b.running[e.ContainerID] = true
	applyTest(t, g, "arm", e)
	wall -= 3600000
	mono = mono.Add(2 * time.Second)
	if g.Tick(context.Background()) != nil {
		t.Fatal("expiry failed")
	}
	b.mu.Lock()
	running := b.running[e.ContainerID]
	b.mu.Unlock()
	if running {
		t.Fatal("wall clock rollback extended execution")
	}
	stored, err := g.Status(e.ContainerID)
	if err != nil || !stored.Revoked {
		t.Fatalf("missing durable revocation: %+v %v", stored, err)
	}
	// A delayed backend start is stopped again, without deleting its layer.
	b.mu.Lock()
	b.running[e.ContainerID] = true
	b.mu.Unlock()
	if g.Tick(context.Background()) != nil {
		t.Fatal("late start escaped fence")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running[e.ContainerID] || b.stops[e.ContainerID] != 2 {
		t.Fatal("late writer escaped")
	}
}

func TestLeaseCASRejectsOldRenewalAndExpiredReplay(t *testing.T) {
	b := backend()
	g := openTest(t, b)
	e := lease("a")
	applyTest(t, g, "arm", e)
	if _, err := g.Apply(context.Background(), Request{Action: "arm", Entry: e}); err != nil {
		t.Fatal("exact replay", err)
	}
	renew := e
	renew.Revision++
	renew.ExpiresUnixMS += 1000
	applyTest(t, g, "renew", renew)
	if _, err := g.Apply(context.Background(), Request{Action: "arm", Entry: e}); !errors.Is(err, ErrConflict) {
		t.Fatal("stale arm revived old lease", err)
	}
	g.mu.Lock()
	g.deadlines[e.ContainerID] = time.Now().Add(-time.Second)
	g.mu.Unlock()
	if _, err := g.Apply(context.Background(), Request{Action: "renew", ExpectedRevision: 1, Entry: renew}); !errors.Is(err, ErrConflict) {
		t.Fatal("expired replay returned live acknowledgement", err)
	}
	next := renew
	next.Revision++
	next.ExpiresUnixMS += 10000
	if _, err := g.Apply(context.Background(), Request{Action: "renew", ExpectedRevision: 2, Entry: next}); !errors.Is(err, ErrConflict) {
		t.Fatal("renew revived expired execution", err)
	}
	applyTest(t, g, "arm", next)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stops[e.ContainerID] != 1 {
		t.Fatal("rearm did not confirm prior execution stopped")
	}
}

func TestGuardianRestartRevokesBeforeAdmissionAndRequiresFreshArm(t *testing.T) {
	b := backend()
	root := privateDir(t)
	g, err := Open(root, "worker-test", b)
	if err != nil {
		t.Fatal(err)
	}
	e := lease("a")
	applyTest(t, g, "arm", e)
	if _, err := Open(root, "worker-test", b); err == nil {
		t.Fatal("second guardian acquired live journal")
	}
	g.Close()
	g, err = Open(root, "worker-test", b)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	stored, err := g.Status(e.ContainerID)
	if err != nil || !stored.Revoked || stored.Revision != 2 {
		t.Fatal(stored, err)
	}
	if g.Tick(context.Background()) != nil {
		t.Fatal("restart did not stop prior execution")
	}
	renew := stored
	renew.Revoked = false
	renew.Revision++
	renew.ExpiresUnixMS += 1000
	if _, err := g.Apply(context.Background(), Request{Action: "renew", ExpectedRevision: 2, Entry: renew}); !errors.Is(err, ErrConflict) {
		t.Fatal("restart implicitly renewed ownership", err)
	}
	applyTest(t, g, "arm", renew)
}

func TestStalledNativeAdmissionCannotBlockAnotherExpiry(t *testing.T) {
	b := backend()
	g := openTest(t, b)
	e := lease("a")
	applyTest(t, g, "arm", e)
	g.mu.Lock()
	g.deadlines[e.ContainerID] = time.Now().Add(-time.Second)
	g.mu.Unlock()
	other := lease("b")
	b.block = other.ContainerID
	b.reached = make(chan struct{}, 1)
	b.release = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := g.Apply(ctx, Request{Action: "arm", Entry: other}); done <- err }()
	select {
	case <-b.reached:
	case <-ctx.Done():
		t.Fatal("no native admission")
	}
	ticked := make(chan error, 1)
	go func() { ticked <- g.Tick(ctx) }()
	select {
	case err := <-ticked:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("unrelated lease expiry blocked behind native admission")
	}
	close(b.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestForgetRequiresMissingImmutableRootAndFreesSameScope(t *testing.T) {
	b := backend()
	g := openTest(t, b)
	e := lease("a")
	applyTest(t, g, "arm", e)
	if g.Forget(context.Background(), e.ContainerID, e.Revision) == nil {
		t.Fatal("stopped root lost protection")
	}
	other := lease("b")
	other.Scope = e.Scope
	if _, err := g.Apply(context.Background(), Request{Action: "arm", Entry: other}); !errors.Is(err, ErrConflict) {
		t.Fatal("two native roots acquired same scope")
	}
	b.mu.Lock()
	b.missing[e.ContainerID] = true
	b.mu.Unlock()
	if err := g.Forget(context.Background(), e.ContainerID, e.Revision); err != nil {
		t.Fatal(err)
	}
	applyTest(t, g, "arm", other)
}

func TestPrivateJournalRejectsForeignOwnershipAndCorruption(t *testing.T) {
	for _, kind := range []string{"worker", "corrupt", "hardlink", "public", "pending"} {
		t.Run(kind, func(t *testing.T) {
			b := backend()
			root := privateDir(t)
			g, err := Open(root, "worker-test", b)
			if err != nil {
				t.Fatal(err)
			}
			e := lease("a")
			applyTest(t, g, "arm", e)
			g.Close()
			worker := "worker-test"
			file := filepath.Join(root, e.ContainerID+".json")
			switch kind {
			case "worker":
				worker = "other"
			case "corrupt":
				_ = os.WriteFile(file, []byte(`{"truncated":true}`), 0600)
			case "hardlink":
				if os.Link(file, filepath.Join(t.TempDir(), "link")) != nil {
					t.Fatal("link")
				}
			case "public":
				_ = os.Chmod(file, 0644)
			case "pending":
				_ = os.WriteFile(filepath.Join(root, ".pending-interrupted"), []byte("partial"), 0600)
			}
			g, err = Open(root, worker, b)
			if kind == "pending" {
				if err != nil {
					t.Fatal("interrupted atomic write blocked recovery", err)
				}
				g.Close()
			} else if err == nil {
				g.Close()
				t.Fatal("unsafe journal reopened")
			}
		})
	}
}

func TestFailedAcknowledgementStillStopsNewlyKnownNativeID(t *testing.T) {
	b := backend()
	g := openTest(t, b)
	e := lease("a")
	b.running[e.ContainerID] = true
	moved := g.store.root + "-moved"
	if err := os.Rename(g.store.root, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	if _, err := g.Apply(context.Background(), Request{Action: "arm", Entry: e}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("failed durable write returned successful authority", err)
	}
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal("poisoned journal prevented native stop", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running[e.ContainerID] || b.stops[e.ContainerID] == 0 {
		t.Fatal("unknown acknowledgement left new root executing")
	}
}
