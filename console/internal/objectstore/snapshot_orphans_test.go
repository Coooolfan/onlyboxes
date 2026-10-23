package objectstore

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
	"time"
)

func TestSnapshotOrphanListingPagesAndNamespace(t *testing.T) {
	var key string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("prefix") != "snapshots/v1/" || q.Get("max-keys") != "100" {
			t.Error("unbounded or unscoped listing")
		}
		w.Header().Set("Content-Type", "application/xml")
		switch q.Get("continuation-token") {
		case "":
			fmt.Fprintf(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>page2</NextContinuationToken><Contents><Key>%s</Key><LastModified>2026-01-01T00:00:00Z</LastModified></Contents><Contents><Key>foreign/v1/a/b/c/archive</Key><LastModified>2026-01-01T00:00:00Z</LastModified></Contents></ListBucketResult>`, key)
		case "page2":
			fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>%s</Key><LastModified>2026-01-01T00:00:00Z</LastModified></Contents><Contents><Key>%s</Key></Contents></ListBucketResult>`, path.Join(path.Dir(key), "manifest.json"), key)
		default:
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>`)
		}
	}))
	defer server.Close()
	store, err := NewSnapshotStore(SnapshotConfig{Config: Config{Endpoint: server.URL, Region: "test", BucketName: "bucket", AccessKey: "ak", SecretKey: "sk", ForcePathStyle: true}, Prefix: "snapshots", MaxBytes: 100, PartSize: MinSnapshotPartSize, PresignTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Object("owner", "session", "lost", 1, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	key = object.Key
	entries, cursor, err := store.ListSnapshotOrphans(context.Background(), "")
	if err != nil || cursor != "page2" || len(entries) != 1 || entries[0].ArchiveKey != key || entries[0].StoreID != object.StoreID {
		t.Fatal(entries, cursor, err)
	}
	entries, cursor, err = store.ListSnapshotOrphans(context.Background(), cursor)
	if err != nil || cursor != "" || len(entries) != 1 || entries[0].ArchiveKey != key {
		t.Fatal(entries, cursor, err)
	}
	if _, _, err := store.ListSnapshotOrphans(context.Background(), "bad"); err == nil {
		t.Fatal("non-advancing listing accepted")
	}
	for _, invalid := range []string{key + "/extra", strings.Replace(key, "/v1/", "/probes/", 1), strings.Replace(key, "/lost/", "/../", 1), strings.Replace(key, strings.Repeat("a", 64), "abc", 1)} {
		if invalid == key {
			continue
		}
		if _, ok := store.orphanKey(invalid); ok {
			t.Fatal("unsafe key admitted", invalid)
		}
	}
	wrong := SnapshotOrphan{StoreID: "foreign", SnapshotID: "lost", ArchiveKey: key}
	if err := store.PurgeSnapshotOrphan(context.Background(), wrong); err == nil {
		t.Fatal("foreign store admitted")
	}
}

func TestSnapshotOrphanPurgeAbortsOnlyExactKey(t *testing.T) {
	fake, store := newSnapshotS3(t)
	ctx := context.Background()
	upload, parts := uploadSnapshot(t, store, []byte("orphan bytes"))
	object, err := store.CompleteUpload(ctx, upload, parts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutSnapshotManifest(ctx, object, []byte(`{"orphan":true}`)); err != nil {
		t.Fatal(err)
	}
	pending, err := store.BeginUpload(ctx, upload.Object)
	if err != nil {
		t.Fatal(err)
	}
	other := object
	other.VersionID = ""
	other.SnapshotID += "-other"
	other.Key = path.Join(path.Dir(path.Dir(object.Key)), other.SnapshotID, "archive")
	untouched, err := store.BeginUpload(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	candidate, ok := store.orphanKey(object.Key)
	if !ok {
		t.Fatal("invalid fixture")
	}
	for i := 0; i < 2; i++ {
		if err := store.PurgeSnapshotOrphan(ctx, candidate); err != nil {
			t.Fatal(err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, exists := fake.uploads[pending.UploadID]; exists {
		t.Fatal("orphan upload retained")
	}
	if _, exists := fake.uploads[untouched.UploadID]; !exists {
		t.Fatal("other upload aborted")
	}
	for _, key := range []string{object.Key, path.Join(path.Dir(object.Key), "manifest.json")} {
		if !fake.markers["/snapshots/"+key] {
			t.Fatal("late completion fence missing")
		}
	}
}

func TestSnapshotOrphanPurgeCapturesVersionsWithoutDatabaseReference(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		t.Run(fmt.Sprint(versioned), func(t *testing.T) {
			bucket, store, object, _ := newPurgeBucket(t, versioned)
			candidate, ok := store.orphanKey(object.Key)
			if !ok {
				t.Fatal("invalid fixture")
			}
			bucket.rejectDelete = versioned
			if versioned && store.PurgeSnapshotOrphan(context.Background(), candidate) == nil {
				t.Fatal("failed version deletion reported success")
			}
			bucket.rejectDelete = false
			if err := store.PurgeSnapshotOrphan(context.Background(), candidate); err != nil {
				t.Fatal(err)
			}
			assertPurgeMarkers(t, bucket)
		})
	}
}
