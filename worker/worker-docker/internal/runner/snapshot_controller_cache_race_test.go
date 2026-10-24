package runner

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// Pause at the first checksum read, after copyCached has pinned the source.
// This exercises the actual copy and eviction paths without a production hook
// or a race that depends on file size and filesystem speed.
type cacheChecksumGate struct {
	context.Context
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *cacheChecksumGate) Err() error {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return g.Context.Err()
}

func cacheCopyFixture(t *testing.T) (*snapshotController, *snapshotJournal, snapshotOperation, snapshotOperation, []byte) {
	t.Helper()
	c, journal := testSnapshotController(t)
	data := bytes.Repeat([]byte("immutable snapshot bytes\n"), 4096)
	archive := transferTestArchive(data)
	source := testSnapshotOperation()
	source.ID = "cache-source"
	source.Artifact = snapshotArtifactName(source.SessionID, source.ID, source.Generation)
	archive.SnapshotID = source.ID
	if err := journal.Claim(source); err != nil {
		t.Fatal(err)
	}
	if err := journal.Update(source.ID, func(o *snapshotOperation) error {
		o.Phase, o.Archive, o.Verified, o.RemoteVerified = "frozen", &archive, true, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Update(source.ID, func(o *snapshotOperation) error {
		o.Phase, o.Committed = "committed", true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Update(source.ID, func(o *snapshotOperation) error {
		o.Phase, o.CacheUntilUnixMS = "finalized", time.Now().Add(time.Hour).UnixMilli()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.artifact(source.Artifact), data, 0600); err != nil {
		t.Fatal(err)
	}
	target := source
	target.ID, target.Kind, target.Phase, target.Generation = "cache-target", "target", "prepared", 2
	target.ContainerID, target.LeaseExpiresUnixMS = "", 0
	target.Archive = &archive
	target.Artifact = snapshotArtifactName(target.SessionID, target.ID, target.Generation)
	if err := journal.Claim(target); err != nil {
		t.Fatal(err)
	}
	if err := journal.Update(target.ID, func(o *snapshotOperation) error { o.Phase = "importing"; return nil }); err != nil {
		t.Fatal(err)
	}
	return c, journal, source, target, data
}

func TestSnapshotCacheCopyOverlapsPressureEviction(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "complete"
		if canceled {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			c, journal, source, target, data := cacheCopyFixture(t)
			before, err := os.ReadFile(journal.root + "/journal.json")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			gate := &cacheChecksumGate{Context: ctx, entered: make(chan struct{}), release: make(chan struct{})}
			done := make(chan error, 1)
			go func() { done <- c.copyCached(gate, target) }()
			// Always release and join the accepted copy before fixture cleanup.
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(gate.release) }) }
			joined := false
			defer func() {
				release()
				if !joined {
					<-done
				}
			}()
			select {
			case <-gate.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("copy did not reach the checksum with its source pinned")
			}
			c.mu.Lock()
			pins := c.pins[source.Artifact]
			c.mu.Unlock()
			if pins != 1 {
				t.Fatalf("copy has no exclusive source pin: %d", pins)
			}
			if err := c.evictCacheBudget(0); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(c.artifact(source.Artifact)); err != nil || !bytes.Equal(got, data) {
				t.Fatal("pressure eviction changed the source during checksum", err)
			}
			if _, err := os.Stat(c.artifact(target.Artifact)); !os.IsNotExist(err) {
				t.Fatal("copy published its destination before verification", err)
			}
			if canceled {
				cancel()
			}
			release()
			copyErr := <-done
			joined = true
			if canceled != (copyErr != nil) {
				t.Fatalf("unexpected copy result after cancellation=%v: %v", canceled, copyErr)
			}
			if canceled {
				if _, err := os.Stat(c.artifact(target.Artifact)); !os.IsNotExist(err) {
					t.Fatal("canceled copy published a destination", err)
				}
			} else {
				if err := c.evictCacheBudget(0); err != nil {
					t.Fatal(err)
				}
				if got, err := os.ReadFile(c.artifact(target.Artifact)); err != nil || !bytes.Equal(got, data) {
					t.Fatal("source eviction damaged the published target", err)
				}
			}
			if err := c.evictCacheBudget(0); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(c.artifact(source.Artifact)); !os.IsNotExist(err) {
				t.Fatal("finished copy retained a source pin", err)
			}
			after, err := os.ReadFile(journal.root + "/journal.json")
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("cache copy or eviction changed durable ownership", err)
			}
		})
	}
}

func TestSnapshotCacheEvictedBeforeCopyIsAMiss(t *testing.T) {
	c, _, source, target, _ := cacheCopyFixture(t)
	if err := c.evictCacheBudget(0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.artifact(source.Artifact)); !os.IsNotExist(err) {
		t.Fatal("pressure eviction did not remove the optional source", err)
	}
	if err := c.copyCached(context.Background(), target); !os.IsNotExist(err) {
		t.Fatal("evicted archive was not reported as a cache miss", err)
	}
	if _, err := os.Stat(c.artifact(target.Artifact)); !os.IsNotExist(err) {
		t.Fatal("cache miss published a destination", err)
	}
}

func TestSnapshotCacheIdentityMismatchIsAMiss(t *testing.T) {
	for _, kind := range []string{"snapshot", "checksum", "runtime-version", "platform", "environment"} {
		t.Run(kind, func(t *testing.T) {
			c, _, source, target, data := cacheCopyFixture(t)
			archive := *target.Archive
			target.Archive = &archive
			switch kind {
			case "snapshot":
				archive.SnapshotID = "another-committed-snapshot"
			case "checksum":
				archive.SHA256 = transferTestArchive([]byte("different content")).SHA256
			case "runtime-version":
				target.Spec.RuntimeVersion = "29.5.0"
			case "platform":
				target.Spec.Platform = "linux/amd64"
			case "environment":
				target.Spec.Env = map[string]string{"PRIVATE": "different startup value"}
			}
			if err := c.copyCached(context.Background(), target); !os.IsNotExist(err) {
				t.Fatal("incompatible source cache was reused", err)
			}
			if _, err := os.Stat(c.artifact(target.Artifact)); !os.IsNotExist(err) {
				t.Fatal("incompatible cache published a target archive", err)
			}
			if got, err := os.ReadFile(c.artifact(source.Artifact)); err != nil || !bytes.Equal(got, data) {
				t.Fatal("cache miss changed an unrelated retained archive", err)
			}
		})
	}
}
