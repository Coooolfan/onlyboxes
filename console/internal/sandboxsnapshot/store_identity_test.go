package sandboxsnapshot

import (
	"context"
	"testing"
)

func TestRetainedSnapshotsKeepTheirStorageNamespace(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	if required, err := r.RequiresStore(ctx); err != nil || required {
		t.Fatal("plain adopted session requires remote storage", required, err)
	}
	op := begin(t, r, Suspend, "suspend")
	if required, err := r.RequiresStore(ctx); err != nil || !required {
		t.Fatal("accepted operation abandoned its store dependency", required, err)
	}
	verified(t, r, op, "snapshot")
	snapshot, err := r.Snapshot(ctx, "owner", "session", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateStore(ctx, snapshot.Upload.Object.StoreID); err != nil {
		t.Fatal("same namespace rejected", err)
	}
	if err := r.ValidateStore(ctx, "another-store"); err == nil {
		t.Fatal("retained data silently rebound to another store")
	}
	if err := r.DeleteSession(ctx, "owner", "session"); err != nil {
		t.Fatal(err)
	}
	if required, err := r.RequiresStore(ctx); err != nil || !required {
		t.Fatal("logical deletion dropped its cleanup dependency", required, err)
	}
}
