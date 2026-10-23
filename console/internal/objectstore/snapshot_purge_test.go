package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type purgeObject struct {
	body   []byte
	marker bool
}
type purgeBucket struct {
	mu           sync.Mutex
	versioned    bool
	next         int
	current      map[string]string
	versions     map[string]map[string]purgeObject
	afterMarker  func()
	rejectDelete bool
}

func (b *purgeBucket) put(key string, value purgeObject) string {
	id := "null"
	if b.versioned {
		b.next++
		id = fmt.Sprintf("version-%d", b.next)
	}
	if b.versions[key] == nil {
		b.versions[key] = map[string]purgeObject{}
	}
	b.versions[key][id] = value
	b.current[key] = id
	return id
}
func (b *purgeBucket) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	if r.Method == http.MethodGet && r.URL.Query().Has("uploads") {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, "<ListMultipartUploadsResult><IsTruncated>false</IsTruncated></ListMultipartUploadsResult>")
		return
	}
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		id := r.URL.Query().Get("versionId")
		if id == "" {
			id = b.current[key]
		}
		value, ok := b.versions[key][id]
		if !ok {
			s3Error(w, 404, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(value.body)))
		w.Header().Set("ETag", etag(value.body))
		if b.versioned {
			w.Header().Set("x-amz-version-id", id)
		}
		if value.marker {
			w.Header().Set("x-amz-meta-onlyboxes-deleted", "v1")
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write(value.body)
		}
	case http.MethodPut:
		if match := r.Header.Get("If-Match"); match != "" {
			value, ok := b.versions[key][b.current[key]]
			if !ok || match != etag(value.body) {
				s3Error(w, 412, "PreconditionFailed")
				return
			}
		}
		if r.Header.Get("If-None-Match") == "*" && b.current[key] != "" {
			s3Error(w, 412, "PreconditionFailed")
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			s3Error(w, 400, "ReadError")
			return
		}
		marker := r.Header.Get("x-amz-meta-onlyboxes-deleted") == "v1"
		id := b.put(key, purgeObject{body: data, marker: marker})
		if b.versioned {
			w.Header().Set("x-amz-version-id", id)
		}
		if marker && b.afterMarker != nil {
			b.afterMarker()
		}
	case http.MethodDelete:
		if b.rejectDelete {
			s3Error(w, 403, "AccessDenied")
			return
		}
		id := r.URL.Query().Get("versionId")
		if id == "" {
			id = b.current[key]
		}
		delete(b.versions[key], id)
		if b.current[key] == id {
			delete(b.current, key)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, 400, "UnexpectedRequest")
	}
}
func newPurgeBucket(t *testing.T, versioned bool) (*purgeBucket, *SnapshotStore, SnapshotObject, string) {
	t.Helper()
	bucket := &purgeBucket{versioned: versioned, current: map[string]string{}, versions: map[string]map[string]purgeObject{}}
	server := httptest.NewServer(http.HandlerFunc(bucket.serve))
	t.Cleanup(server.Close)
	store, err := NewSnapshotStore(SnapshotConfig{Config: Config{Endpoint: server.URL, Region: "test", BucketName: "bucket", AccessKey: "ak", SecretKey: "sk", ForcePathStyle: true}, Prefix: "snapshots", MaxBytes: 10 << 30, PartSize: 5 << 20, PresignTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("private-archive-filesystem-bytes")
	sum := sha256.Sum256(body)
	object, err := store.Object("owner", "session", "snapshot", int64(len(body)), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	object.VersionID = bucket.put(object.Key, purgeObject{body: body})
	manifest := bucket.put(path.Join(path.Dir(object.Key), "manifest.json"), purgeObject{body: []byte(`{"private-env":"private-restore-secret"}`)})
	if !versioned {
		object.VersionID = ""
		manifest = ""
	}
	return bucket, store, object, manifest
}
func assertPurgeMarkers(t *testing.T, b *purgeBucket) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.current) != 2 {
		t.Fatal("both immutable object keys must remain reserved")
	}
	for _, versions := range b.versions {
		for _, value := range versions {
			if !value.marker || bytes.Contains(value.body, []byte("private-archive")) || bytes.Contains(value.body, []byte("private-restore")) {
				t.Fatal("cleanup retained private object versions")
			}
		}
	}
}
func TestSnapshotPurgeDeletesUserVersionsAndReservesKeys(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		t.Run(strconv.FormatBool(versioned), func(t *testing.T) {
			bucket, store, object, manifest := newPurgeBucket(t, versioned)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for i := 0; i < 2; i++ {
				if err := store.PurgeSnapshot(ctx, object, manifest); err != nil {
					t.Fatal(err)
				}
			}
			assertPurgeMarkers(t, bucket)
			_, err := store.store.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String(object.Key), Body: bytes.NewReader([]byte("late-private-data")), IfNoneMatch: aws.String("*")})
			if !isSnapshotPrecondition(err) {
				t.Fatal("late conditional write recreated a deleted archive", err)
			}
		})
	}
}
func TestSnapshotPurgeRecoversUnknownVersionsAfterLostReplacementReply(t *testing.T) {
	bucket, store, object, _ := newPurgeBucket(t, true)
	object.VersionID = "" // Complete succeeded but its reply was never persisted.
	ctx, cancel := context.WithCancel(context.Background())
	bucket.mu.Lock()
	bucket.afterMarker = cancel
	bucket.mu.Unlock()
	if err := store.PurgeSnapshot(ctx, object, ""); err == nil {
		t.Fatal("canceled replacement unexpectedly completed cleanup")
	}
	bucket.mu.Lock()
	bucket.afterMarker = nil
	bucket.mu.Unlock()
	retry, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := store.PurgeSnapshot(retry, object, ""); err != nil {
		t.Fatal("lost reply discarded the old version identity", err)
	}
	assertPurgeMarkers(t, bucket)
}
func TestSnapshotPurgeConcurrentRetriesNeverRemoveDeletionFence(t *testing.T) {
	bucket, store, object, manifest := newPurgeBucket(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- store.PurgeSnapshot(ctx, object, manifest) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	assertPurgeMarkers(t, bucket)
}
func TestSnapshotPurgeKeepsVersionDeletionFailureRetryable(t *testing.T) {
	bucket, store, object, manifest := newPurgeBucket(t, true)
	bucket.mu.Lock()
	bucket.rejectDelete = true
	bucket.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.PurgeSnapshot(ctx, object, manifest); err == nil {
		t.Fatal("version cleanup failure was acknowledged")
	}
	bucket.mu.Lock()
	bucket.rejectDelete = false
	bucket.mu.Unlock()
	if err := store.PurgeSnapshot(ctx, object, manifest); err != nil {
		t.Fatal(err)
	}
	assertPurgeMarkers(t, bucket)
}
