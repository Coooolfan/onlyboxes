package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
)

// Opt-in provider test. Only a fresh random prefix is mutated. Credentials are
// passed in the process environment and are never included in failure output.
func TestSnapshotOrphansRealS3(t *testing.T) {
	endpoint := os.Getenv("ONLYBOXES_TEST_SNAPSHOT_ENDPOINT")
	if endpoint == "" {
		t.Skip("requires an isolated S3 test endpoint")
	}
	versioned := os.Getenv("ONLYBOXES_TEST_SNAPSHOT_VERSIONED") == "true"
	bucketName := os.Getenv("ONLYBOXES_TEST_SNAPSHOT_BUCKET")
	if versioned {
		bucketName = "onlyboxes-orphan-" + uuid.NewString()
	}
	store, err := NewSnapshotStore(SnapshotConfig{Config: Config{Endpoint: endpoint, Region: os.Getenv("ONLYBOXES_TEST_SNAPSHOT_REGION"), BucketName: bucketName, AccessKey: os.Getenv("ONLYBOXES_TEST_SNAPSHOT_AK"), SecretKey: os.Getenv("ONLYBOXES_TEST_SNAPSHOT_SK"), ForcePathStyle: true}, Prefix: "snapshot-orphan-tests/" + uuid.NewString(), MaxBytes: 1 << 20, PartSize: MinSnapshotPartSize, PresignTTL: time.Minute})
	if err != nil {
		t.Fatal("S3 test configuration rejected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := store.store.client
	bucket := aws.String(store.config.BucketName)
	if versioned {
		create := &s3.CreateBucketInput{Bucket: bucket}
		if store.config.Region != "us-east-1" {
			create.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(store.config.Region)}
		}
		if _, err := client.CreateBucket(ctx, create); err != nil {
			t.Fatal("versioned test bucket creation failed")
		}
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if _, err := client.DeleteBucket(cleanup, &s3.DeleteBucketInput{Bucket: bucket}); err != nil {
				t.Error("versioned test bucket cleanup failed")
			}
		})
		if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: bucket, VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
			t.Fatal("test bucket versioning failed")
		}
	}
	var keys []string
	var uploads []SnapshotUpload
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		for _, upload := range uploads {
			if store.AbortSnapshotUpload(cleanup, upload) != nil {
				t.Error("test upload cleanup failed")
			}
		}
		for _, key := range keys {
			head, err := client.HeadObject(cleanup, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String(key)})
			if snapshotObjectMissing(err) {
				continue
			}
			if err != nil {
				t.Error("test object cleanup HEAD failed")
				continue
			}
			input := &s3.DeleteObjectInput{Bucket: bucket, Key: aws.String(key)}
			if aws.ToString(head.VersionId) != "" {
				input.VersionId = head.VersionId
			}
			if _, err := client.DeleteObject(cleanup, input); err != nil {
				t.Error("test object cleanup failed")
			}
		}
	})
	put := func(key string, body []byte) string {
		keys = append(keys, key)
		out, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: aws.String(key), Body: bytes.NewReader(body), IfNoneMatch: aws.String("*")})
		if err != nil {
			t.Fatal("test object PUT failed")
		}
		return aws.ToString(out.VersionId)
	}
	body := []byte("orphan filesystem bytes")
	sum := sha256.Sum256(body)
	object, err := store.Object("test-owner", "test-session", "lost", int64(len(body)), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal("object creation failed")
	}
	archiveVersion := put(object.Key, body)
	manifestKey := path.Join(path.Dir(object.Key), "manifest.json")
	manifestVersion := put(manifestKey, []byte(`{"orphan":true}`))
	// Invalid keys still occupy LIST pages. They must be skipped without
	// treating an empty candidate page as the end of enumeration.
	for i := 0; i < 103; i++ {
		put(fmt.Sprintf("%s/v1/000-foreign-%03d", store.config.Prefix, i), []byte("foreign"))
	}
	create := func(ref SnapshotObject) SnapshotUpload {
		ref.VersionID = ""
		upload, err := store.BeginUpload(ctx, ref)
		if err != nil {
			t.Fatal("multipart creation failed")
		}
		uploads = append(uploads, upload)
		return upload
	}
	pending := create(object)
	// Put an actual part in the unregistered upload so abort proves byte cleanup.
	if _, err := client.UploadPart(ctx, &s3.UploadPartInput{Bucket: bucket, Key: aws.String(object.Key), UploadId: aws.String(pending.UploadID), PartNumber: aws.Int32(1), Body: bytes.NewReader(body)}); err != nil {
		t.Fatal("orphan part upload failed")
	}
	other, err := store.Object("test-owner", "test-session", "uncompleted", int64(len(body)), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal("other identity failed")
	}
	untouched := create(other)
	var candidates []SnapshotOrphanEntry
	cursor := ""
	pages := 0
	for {
		entries, next, err := store.ListSnapshotOrphans(ctx, cursor)
		if err != nil {
			t.Fatal("real paginated listing failed")
		}
		candidates = append(candidates, entries...)
		pages++
		if next == "" {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("listing did not terminate")
		}
	}
	if pages < 2 || len(candidates) != 2 {
		t.Fatal("listing scope or pagination failed", pages, len(candidates))
	}
	for _, entry := range candidates {
		if entry.ArchiveKey != object.Key || entry.Modified.IsZero() {
			t.Fatal("unexpected candidate")
		}
	}
	candidate := candidates[0].SnapshotOrphan
	if err := store.PurgeSnapshotOrphan(ctx, candidate); err != nil {
		t.Fatal("real orphan purge failed")
	}
	if err := store.PurgeSnapshotOrphan(ctx, candidate); err != nil {
		t.Fatal("real orphan purge replay failed")
	}
	if _, err := store.ListSnapshotParts(ctx, pending); !isNoSuchUpload(err) {
		t.Fatal("orphan parts retained")
	}
	if _, err := store.ListSnapshotParts(ctx, untouched); err != nil {
		t.Fatal("unrelated upload aborted")
	}
	for key, version := range map[string]string{object.Key: archiveVersion, manifestKey: manifestVersion} {
		head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String(key)})
		if err != nil || head.Metadata["onlyboxes-deleted"] != "v1" {
			t.Fatal("durable deletion marker missing")
		}
		if version != "" && version != "null" {
			_, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String(key), VersionId: aws.String(version)})
			if !snapshotObjectMissing(err) {
				t.Fatal("old user data version retained")
			}
		}
		_, err = client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: aws.String(key), Body: strings.NewReader("late write"), IfNoneMatch: aws.String("*")})
		if !isSnapshotPrecondition(err) {
			t.Fatal("late write recreated collected bytes")
		}
	}
	t.Log("PASS: real paginated listing, exact-key multipart abort, marker replay and late-write fencing; versioned=", versioned)
}
