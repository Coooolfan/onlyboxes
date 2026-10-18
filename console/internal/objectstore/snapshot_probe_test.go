package objectstore

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSnapshotProbeVerifiesCapabilitiesAndCleansIsolatedObjects(t *testing.T) {
	fake, store := newSnapshotS3(t)
	fake.mu.Lock()
	fake.objects["/snapshots/sessions/actual-session/archive"] = []byte("retain-session-data")
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.objects) != 1 || string(fake.objects["/snapshots/sessions/actual-session/archive"]) != "retain-session-data" || len(fake.uploads) != 0 {
		t.Fatal("probe leaked resources or touched session data", len(fake.objects), len(fake.uploads))
	}
	for key := range fake.deletes {
		if !strings.HasPrefix(key, "/snapshots/sessions/probes/") {
			t.Fatal("probe deleted outside its isolated namespace")
		}
	}
}
func TestSnapshotProbeRejectsCorruptionWithoutLeakingURLs(t *testing.T) {
	fake, store := newSnapshotS3(t)
	fake.mu.Lock()
	fake.corrupt = true
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := store.Probe(ctx)
	if err == nil || strings.Contains(err.Error(), store.config.Endpoint) || strings.Contains(err.Error(), "X-Amz") {
		t.Fatal("corrupted store was accepted or signed request details leaked", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.objects) != 0 || len(fake.uploads) != 0 {
		t.Fatal("failed probe did not clean up")
	}
}

func TestSnapshotProbeRequiresConditionalArchiveWritesAndListing(t *testing.T) {
	for _, mode := range []string{"overwrite", "listing"} {
		t.Run(mode, func(t *testing.T) {
			fake, store := newSnapshotS3(t)
			fake.mu.Lock()
			fake.ignoreArchiveCondition = mode == "overwrite"
			fake.rejectListing = mode == "listing"
			fake.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := store.Probe(ctx); err == nil {
				t.Fatal("unsupported storage capability was accepted", mode)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.objects) != 0 || len(fake.uploads) != 0 {
				t.Fatal("rejected probe leaked resources", mode)
			}
		})
	}
}
