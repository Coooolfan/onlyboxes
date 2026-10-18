package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This server checks wire-level S3 requests, including conditional completion,
// signatures, version-pinned reads/deletes and data integrity. It is not a claim
// of conformance for a particular production S3-compatible implementation.
type snapshotS3 struct {
	mu               sync.Mutex
	rejectArchiveGet bool
	archiveGets      int
	objects          map[string][]byte
	uploads          map[string]map[int][]byte
	uploadKeys       map[string]string
	nextID           int
	corrupt          bool
	deletes          map[string]string
}

func newSnapshotS3(t *testing.T) (*snapshotS3, *SnapshotStore) {
	t.Helper()
	fake := &snapshotS3{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}, uploadKeys: map[string]string{}, deletes: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	store, err := NewSnapshotStore(SnapshotConfig{Config: Config{Endpoint: server.URL, Region: "test", BucketName: "snapshots", AccessKey: "ak", SecretKey: "sk", ForcePathStyle: true}, Prefix: "sessions", MaxBytes: 10 << 30, PartSize: MinSnapshotPartSize, PresignTTL: 15 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return fake, store
}
func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code><Message>test store error</Message></Error>", code)
}
func etag(body []byte) string {
	hash := sha256.Sum256(body)
	return `"` + hex.EncodeToString(hash[:]) + `"`
}
func (s *snapshotS3) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := r.URL.Path
	q := r.URL.Query()
	uploadID := q.Get("uploadId")
	if !strings.HasPrefix(key, "/snapshots/sessions/") {
		s3Error(w, 400, "InvalidKey")
		return
	}
	if r.Header.Get("Authorization") == "" && q.Get("X-Amz-Signature") == "" {
		s3Error(w, 403, "AccessDenied")
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.nextID++
		id := fmt.Sprintf("upload-%d", s.nextID)
		s.uploads[id] = map[int][]byte{}
		s.uploadKeys[id] = key
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><Bucket>snapshots</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", key, id)
	case r.Method == http.MethodPut && uploadID != "":
		parts, ok := s.uploads[uploadID]
		if !ok {
			s3Error(w, 404, "NoSuchUpload")
			return
		}
		if s.uploadKeys[uploadID] != key {
			s3Error(w, 400, "InvalidKey")
			return
		}
		number, _ := strconv.Atoi(q.Get("partNumber"))
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s3Error(w, 400, "ReadError")
			return
		}
		parts[number] = body
		w.Header().Set("ETag", etag(body))
	case r.Method == http.MethodGet && uploadID != "":
		parts, ok := s.uploads[uploadID]
		if !ok {
			s3Error(w, 404, "NoSuchUpload")
			return
		}
		fmt.Fprint(w, "<ListPartsResult><IsTruncated>false</IsTruncated>")
		for n := 1; n <= len(parts); n++ {
			body, ok := parts[n]
			if !ok {
				continue
			}
			fmt.Fprintf(w, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag><Size>%d</Size></Part>", n, etag(body), len(body))
		}
		fmt.Fprint(w, "</ListPartsResult>")
	case r.Method == http.MethodPost && uploadID != "":
		parts, ok := s.uploads[uploadID]
		if !ok {
			s3Error(w, 404, "NoSuchUpload")
			return
		}
		if r.Header.Get("If-None-Match") != "*" {
			s3Error(w, 400, "MissingCondition")
			return
		}
		if _, exists := s.objects[key]; exists {
			s3Error(w, 412, "PreconditionFailed")
			return
		}
		var body []byte
		for n := 1; n <= len(parts); n++ {
			body = append(body, parts[n]...)
		}
		s.objects[key] = body
		delete(s.uploads, uploadID)
		w.Header().Set("x-amz-version-id", "archive-v1")
		fmt.Fprintf(w, "<CompleteMultipartUploadResult><Bucket>snapshots</Bucket><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>", key, etag(body))
	case r.Method == http.MethodDelete && uploadID != "":
		if _, ok := s.uploads[uploadID]; !ok {
			s3Error(w, 404, "NoSuchUpload")
			return
		}
		delete(s.uploads, uploadID)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		if r.Header.Get("If-None-Match") != "*" {
			s3Error(w, 400, "MissingCondition")
			return
		}
		if _, exists := s.objects[key]; exists {
			s3Error(w, 412, "PreconditionFailed")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s3Error(w, 400, "ReadError")
			return
		}
		s.objects[key] = body
		w.Header().Set("x-amz-version-id", "manifest-v1")
	case r.Method == http.MethodHead || r.Method == http.MethodGet:
		if r.Method == http.MethodGet && strings.HasSuffix(key, "/archive") {
			s.archiveGets++
			if s.rejectArchiveGet {
				s3Error(w, 403, "AccessDenied")
				return
			}
		}
		body, ok := s.objects[key]
		if !ok {
			s3Error(w, 404, "NoSuchKey")
			return
		}
		version := "archive-v1"
		if strings.HasSuffix(key, "manifest.json") {
			version = "manifest-v1"
		}
		if v := q.Get("versionId"); v != "" && v != version {
			s3Error(w, 404, "NoSuchVersion")
			return
		}
		w.Header().Set("x-amz-version-id", version)
		w.Header().Set("ETag", `"not-a-content-digest"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodHead {
			return
		}
		if s.corrupt && len(body) > 0 {
			body = bytes.Clone(body)
			body[0] ^= 0xff
		}
		if value := r.Header.Get("Range"); value != "" {
			start, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(value, "bytes="), "-"))
			w.Header().Set("Content-Length", strconv.Itoa(len(body)-start))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			w.WriteHeader(http.StatusPartialContent)
			body = body[start:]
		}
		_, _ = w.Write(body)
	case r.Method == http.MethodDelete:
		s.deletes[key] = q.Get("versionId")
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, 400, "UnexpectedRequest")
	}
}

func uploadSnapshot(t *testing.T, s *SnapshotStore, data []byte) (SnapshotUpload, []SnapshotPart) {
	t.Helper()
	hash := sha256.Sum256(data)
	object, err := s.Object("owner", "session", "snap-1", int64(len(data)), hex.EncodeToString(hash[:]))
	if err != nil {
		t.Fatal(err)
	}
	upload, err := s.BeginUpload(context.Background(), object)
	if err != nil {
		t.Fatal(err)
	}
	var parts []SnapshotPart
	for i := int32(1); int64(i-1)*upload.PartSize < int64(len(data)); i++ {
		grant, err := s.PresignPart(context.Background(), upload, i)
		if err != nil {
			t.Fatal(err)
		}
		body := data[grant.Offset : grant.Offset+grant.Size]
		req, err := http.NewRequest(grant.Method, grant.URL, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = grant.Headers.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("upload part status %d", resp.StatusCode)
		}
		parts = append(parts, SnapshotPart{Number: i, ETag: resp.Header.Get("ETag"), Size: grant.Size})
	}
	return upload, parts
}

func TestSnapshotMultipartReadbackAndImmutableManifest(t *testing.T) {
	fake, store := newSnapshotS3(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("x"), int(MinSnapshotPartSize)+3)
	upload, parts := uploadSnapshot(t, store, data)
	if len(parts) != 2 || parts[1].Size != 3 {
		t.Fatalf("wrong multipart layout: %+v", parts)
	}
	object, err := store.CompleteUpload(ctx, upload, parts)
	if err != nil {
		t.Fatal(err)
	}
	if object.VersionID != "archive-v1" {
		t.Fatalf("missing version pin: %+v", object)
	}
	if err = store.VerifySnapshot(ctx, object); err != nil {
		t.Fatal(err)
	}
	// A retry after Complete succeeded but the caller lost its response must verify
	// the existing bytes rather than start another overwrite.
	recovered, err := store.CompleteUpload(ctx, upload, parts)
	if err != nil || recovered != object {
		t.Fatalf("lost-response retry: %+v %v", recovered, err)
	}
	manifest := []byte(`{"version":1}`)
	version, err := store.PutSnapshotManifest(ctx, object, manifest)
	if err != nil || version != "manifest-v1" {
		t.Fatalf("manifest: %s %v", version, err)
	}
	if _, err = store.PutSnapshotManifest(ctx, object, manifest); err != nil {
		t.Fatalf("manifest retry failed: %v", err)
	}
	if _, err = store.PutSnapshotManifest(ctx, object, []byte(`{"version":2}`)); !errors.Is(err, ErrSnapshotConflict) {
		t.Fatalf("overwrote descriptor: %v", err)
	}
	grant, err := store.PresignSnapshotDownload(ctx, object, 5)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(grant.URL)
	if parsed.Query().Get("versionId") != "archive-v1" || grant.Headers.Get("Range") != "bytes=5-" {
		t.Fatalf("download not version/range pinned")
	}
	request, err := http.NewRequest(grant.Method, grant.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header = grant.Headers.Clone()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusPartialContent || !bytes.Equal(downloaded, data[5:]) {
		t.Fatalf("range download failed: status=%d error=%v", response.StatusCode, err)
	}
	if err = store.DeleteSnapshot(ctx, object, version); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.deletes["/snapshots/"+object.Key] != "archive-v1" || fake.deletes["/snapshots/"+strings.TrimSuffix(object.Key, "archive")+"manifest.json"] != "manifest-v1" {
		t.Fatal("delete did not pin object versions")
	}
}

func TestSnapshotRejectsCorruptionAndForgedNamespace(t *testing.T) {
	fake, store := newSnapshotS3(t)
	ctx := context.Background()
	upload, parts := uploadSnapshot(t, store, []byte("archive-data"))
	object, err := store.CompleteUpload(ctx, upload, parts)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.corrupt = true
	fake.mu.Unlock()
	if err = store.VerifySnapshot(ctx, object); !errors.Is(err, ErrSnapshotIntegrity) {
		t.Fatalf("ETag masked corruption: %v", err)
	}
	if _, err = store.CompleteUpload(ctx, upload, parts); !errors.Is(err, ErrSnapshotIntegrity) {
		t.Fatalf("ambiguous completion accepted corrupt bytes: %v", err)
	}
	altered := object
	altered.OwnerID = "other"
	if _, err = store.PresignSnapshotDownload(ctx, altered, 0); err == nil {
		t.Fatal("forged owner accepted")
	}
	altered = object
	altered.Key = "exports/user-file"
	if err = store.DeleteSnapshot(ctx, altered, ""); err == nil {
		t.Fatal("deleted key outside snapshot namespace")
	}
	altered = object
	altered.Key = "../" + object.Key
	if err = altered.Validate(); err == nil {
		t.Fatal("parent key segment accepted")
	}
	altered = object
	altered.StoreID = "another-store"
	if err = store.VerifySnapshot(ctx, altered); err == nil {
		t.Fatal("wrong storage identity accepted")
	}
}

func TestSnapshotPartPlanAndAbort(t *testing.T) {
	_, store := newSnapshotS3(t)
	ctx := context.Background()
	upload, parts := uploadSnapshot(t, store, []byte("small"))
	if _, err := store.PresignPart(ctx, upload, 2); err == nil {
		t.Fatal("signed out-of-plan part")
	}
	wrong := append([]SnapshotPart(nil), parts...)
	wrong[0].Size++
	if _, err := store.CompleteUpload(ctx, upload, wrong); err == nil {
		t.Fatal("completed wrong-sized part")
	}
	wrong[0] = parts[0]
	wrong[0].ETag = "different"
	if _, err := store.CompleteUpload(ctx, upload, wrong); err == nil {
		t.Fatal("completed unreported bytes")
	}
	if err := store.AbortSnapshotUpload(ctx, upload); err != nil {
		t.Fatal(err)
	}
	if err := store.AbortSnapshotUpload(ctx, upload); err != nil {
		t.Fatalf("abort not idempotent: %v", err)
	}
}

func TestSnapshotConfigurationValidation(t *testing.T) {
	cfg := SnapshotConfig{Config: Config{Endpoint: "https://storage.example.com", Region: "r", BucketName: "b", AccessKey: "ak", SecretKey: "sk"}, Prefix: "snapshots", MaxBytes: 10 << 30, PartSize: 64 << 20, PresignTTL: time.Minute}
	for _, mutate := range []func(*SnapshotConfig){
		func(c *SnapshotConfig) { c.Prefix = "../exports" }, func(c *SnapshotConfig) { c.Prefix = "a/../exports" }, func(c *SnapshotConfig) { c.PartSize = 1 }, func(c *SnapshotConfig) { c.PartSize = MaxSnapshotPartSize + 1 }, func(c *SnapshotConfig) { c.MaxBytes = c.PartSize * 10001 }, func(c *SnapshotConfig) { c.Endpoint = "https://user:secret@storage.example.com" }, func(c *SnapshotConfig) { c.PresignTTL = 8 * 24 * time.Hour },
	} {
		bad := cfg
		mutate(&bad)
		if _, err := NewSnapshotStore(bad); err == nil {
			t.Fatal("accepted invalid snapshot configuration")
		}
	}
	store, err := NewSnapshotStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Object("owner", "session", "../exports", 1, strings.Repeat("a", 64)); err == nil {
		t.Fatal("accepted path traversal snapshot ID")
	}
}

func TestSnapshotConfigurationChangesDoNotInvalidateReservedUploads(t *testing.T) {
	_, store := newSnapshotS3(t)
	ctx := context.Background()
	upload, parts := uploadSnapshot(t, store, []byte("existing-snapshot"))
	store.config.PartSize = 64 << 20
	store.config.MaxBytes = 1
	if _, err := store.PresignPart(ctx, upload, 1); err != nil {
		t.Fatalf("old part plan invalidated: %v", err)
	}
	object, err := store.CompleteUpload(ctx, upload, parts)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.VerifySnapshot(ctx, object); err != nil {
		t.Fatalf("existing snapshot invalidated by lower limit: %v", err)
	}
	if _, err = store.PresignSnapshotDownload(ctx, object, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = store.BeginUpload(ctx, upload.Object); err == nil {
		t.Fatal("new upload ignored reduced admission limit")
	}
}

func TestSnapshotLostCompleteUsesRemoteVerifierWithoutConsoleArchiveGet(t *testing.T) {
	fake, store := newSnapshotS3(t)
	ctx := context.Background()
	upload, parts := uploadSnapshot(t, store, []byte("snapshot"))
	object, err := store.CompleteUpload(ctx, upload, parts)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.rejectArchiveGet = true
	fake.mu.Unlock()
	calls := 0
	remote := func(_ context.Context, candidate SnapshotObject) error {
		calls++
		if candidate != object {
			t.Fatalf("remote verification lost immutable identity: %+v", candidate)
		}
		return nil
	}
	got, err := store.CompleteUploadWithVerifier(ctx, upload, parts, remote)
	if err != nil || got != object || calls != 1 {
		t.Fatalf("lost Complete did not delegate readback: %+v calls=%d err=%v", got, calls, err)
	}
	_, err = store.CompleteUploadWithVerifier(ctx, upload, parts, func(context.Context, SnapshotObject) error { return ErrSnapshotIntegrity })
	if !errors.Is(err, ErrSnapshotIntegrity) {
		t.Fatalf("remote corruption was ignored: %v", err)
	}
	fake.mu.Lock()
	gets := fake.archiveGets
	fake.mu.Unlock()
	if gets != 0 {
		t.Fatalf("Console downloaded archive %d times", gets)
	}
}
