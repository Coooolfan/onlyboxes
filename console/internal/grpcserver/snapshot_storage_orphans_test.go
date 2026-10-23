package grpcserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
)

type orphanCleanerObjects struct {
	*cleanerObjects
	entries                  []objectstore.SnapshotOrphanEntry
	cursors                  []string
	purged                   []objectstore.SnapshotOrphan
	rejectOrphan, rejectList bool
}

func (o *orphanCleanerObjects) ListSnapshotOrphans(_ context.Context, cursor string) ([]objectstore.SnapshotOrphanEntry, string, error) {
	o.cursors = append(o.cursors, cursor)
	if o.rejectList {
		return nil, "", errors.New("listing unavailable")
	}
	if cursor == "" {
		return o.entries, "next", nil
	}
	return nil, "", nil
}
func (o *orphanCleanerObjects) PurgeSnapshotOrphan(_ context.Context, candidate objectstore.SnapshotOrphan) error {
	if o.rejectOrphan {
		return errors.New("storage unavailable")
	}
	o.purged = append(o.purged, candidate)
	return nil
}
func TestSnapshotCleanerOrphanPagesAndDurableFailureRetry(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := setupSnapshotRouteTest(t)
	now := time.Now()
	objects := &orphanCleanerObjects{cleanerObjects: &cleanerObjects{}, entries: []objectstore.SnapshotOrphanEntry{{SnapshotOrphan: objectstore.SnapshotOrphan{StoreID: strings.Repeat("a", 64), SnapshotID: "lost", ArchiveKey: "private/key/archive"}, Modified: now.Add(-24 * time.Hour)}}}
	objects.entries = append(objects.entries, objectstore.SnapshotOrphanEntry{SnapshotOrphan: objectstore.SnapshotOrphan{StoreID: strings.Repeat("a", 64), SnapshotID: "concurrent", ArchiveKey: "private/concurrent/archive"}, Modified: now.Add(24 * time.Hour)})
	cfg := SnapshotCleanerConfig{Interval: time.Second, Grace: time.Hour, RetryInterval: time.Second, CallTimeout: time.Second, MaxJobs: 2}
	cleaner, err := NewSnapshotCleaner(svc, objects, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleaner.collectStorageOrphans(ctx, now); err != nil {
		t.Fatal(err)
	}
	if len(objects.purged) != 0 {
		t.Fatal("orphan was not quarantined")
	}
	objects.rejectOrphan = true
	if err := cleaner.collectStorageOrphans(ctx, now.Add(2*time.Hour)); err == nil {
		t.Fatal("storage failure hidden")
	}
	if len(objects.cursors) != 2 || objects.cursors[1] != "next" {
		t.Fatal("pagination stalled", objects.cursors)
	}
	// A replacement cleaner can retry durable work even while LIST is broken.
	cleaner, err = NewSnapshotCleaner(svc, objects, cfg)
	if err != nil {
		t.Fatal(err)
	}
	objects.rejectOrphan = false
	objects.rejectList = true
	if err := cleaner.collectStorageOrphans(ctx, now.Add(3*time.Hour)); err == nil {
		t.Fatal("listing failure hidden")
	}
	if len(objects.purged) != 1 {
		t.Fatal("durable purge lost when listing failed")
	}
	if pending, err := svc.snapshotRepository.ClaimStorageOrphans(ctx, now.Add(4*time.Hour), time.Hour, 10); err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
}

func TestSnapshotCleanerOrphanCannotCollectCurrentSnapshot(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := setupSnapshotRouteTest(t)
	op := beginRouteSnapshotTest(t, svc, sandboxsnapshot.Suspend, "suspend")
	commitRouteSnapshotTest(t, svc, op)
	snapshot, err := svc.snapshotRepository.Snapshot(ctx, "owner", "session", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	object := snapshot.Upload.Object
	now := time.Now()
	objects := &orphanCleanerObjects{cleanerObjects: &cleanerObjects{}, entries: []objectstore.SnapshotOrphanEntry{{SnapshotOrphan: objectstore.SnapshotOrphan{StoreID: object.StoreID, SnapshotID: object.SnapshotID, ArchiveKey: object.Key}, Modified: now.Add(-48 * time.Hour)}}}
	cleaner, err := NewSnapshotCleaner(svc, objects, SnapshotCleanerConfig{Interval: time.Second, Grace: time.Hour, RetryInterval: time.Second, CallTimeout: time.Second, MaxJobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{now, now.Add(2 * time.Hour)} {
		if err := cleaner.collectStorageOrphans(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	if len(objects.purged) != 0 {
		t.Fatal("current recovery point collected")
	}
}
