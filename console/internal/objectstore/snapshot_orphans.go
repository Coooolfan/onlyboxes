package objectstore

import (
	"context"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// SnapshotOrphan is a namespace-scoped candidate, never proof of unreferenced
// data. The repository must durably fence its ID before PurgeSnapshotOrphan.
// Keys contain scope hashes; no owner identity is inferred from object contents.
type SnapshotOrphan struct {
	StoreID    string `json:"store_id"`
	SnapshotID string `json:"snapshot_id"`
	ArchiveKey string `json:"archive_key"`
}

type SnapshotOrphanEntry struct {
	SnapshotOrphan
	Modified time.Time
}

func (s *SnapshotStore) orphanKey(key string) (SnapshotOrphan, bool) {
	prefix := s.config.Prefix + "/v1/"
	if !strings.HasPrefix(key, prefix) {
		return SnapshotOrphan{}, false
	}
	parts := strings.Split(strings.TrimPrefix(key, prefix), "/")
	if len(parts) != 4 || !validSHA256(parts[0]) || !validSHA256(parts[1]) || !snapshotIDPattern.MatchString(parts[2]) || (parts[3] != "archive" && parts[3] != "manifest.json") {
		return SnapshotOrphan{}, false
	}
	return SnapshotOrphan{StoreID: s.namespace, SnapshotID: parts[2], ArchiveKey: path.Join(path.Dir(key), "archive")}, true
}

// ListSnapshotOrphans scans one bounded page. LIST discovers candidates only;
// database references and admission fencing decide whether collection is safe.
func (s *SnapshotStore) ListSnapshotOrphans(ctx context.Context, cursor string) ([]SnapshotOrphanEntry, string, error) {
	input := &s3.ListObjectsV2Input{Bucket: aws.String(s.config.BucketName), Prefix: aws.String(s.config.Prefix + "/v1/"), MaxKeys: aws.Int32(100)}
	if cursor != "" {
		input.ContinuationToken = aws.String(cursor)
	}
	out, err := s.store.client.ListObjectsV2(ctx, input)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if aws.ToBool(out.IsTruncated) {
		next = aws.ToString(out.NextContinuationToken)
		if next == "" || next == cursor {
			return nil, "", errors.New("snapshot listing did not advance")
		}
	}
	var entries []SnapshotOrphanEntry
	for _, object := range out.Contents {
		candidate, ok := s.orphanKey(aws.ToString(object.Key))
		if ok && object.LastModified != nil && !object.LastModified.IsZero() {
			entries = append(entries, SnapshotOrphanEntry{SnapshotOrphan: candidate, Modified: *object.LastModified})
		}
	}
	return entries, next, nil
}

// PurgeSnapshotOrphan requires a durable database deletion fence. Immutable
// markers prevent a late conditional completion from recreating collected data.
// Unknown uploads with no listed object still require bucket lifecycle cleanup.
func (s *SnapshotStore) PurgeSnapshotOrphan(ctx context.Context, candidate SnapshotOrphan) error {
	wanted, ok := s.orphanKey(candidate.ArchiveKey)
	if !ok || wanted != candidate {
		return errors.New("orphan candidate belongs to another namespace")
	}
	if err := s.purgeSnapshotKey(ctx, candidate.ArchiveKey, "", candidate.SnapshotID); err != nil {
		return err
	}
	if err := s.purgeSnapshotKey(ctx, path.Join(path.Dir(candidate.ArchiveKey), "manifest.json"), "", candidate.SnapshotID); err != nil {
		return err
	}
	// Exact-key listing works with stores that cannot enumerate all multipart
	// uploads under a prefix. A truncated batch is retried from the first page.
	out, err := s.store.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(s.config.BucketName), Prefix: aws.String(candidate.ArchiveKey), MaxUploads: aws.Int32(100)})
	if err != nil {
		return err
	}
	for _, upload := range out.Uploads {
		if aws.ToString(upload.Key) != candidate.ArchiveKey {
			continue
		}
		id := aws.ToString(upload.UploadId)
		if id == "" {
			return ErrSnapshotCleanupPending
		}
		_, err := s.store.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(candidate.ArchiveKey), UploadId: aws.String(id)})
		if err != nil && !isNoSuchUpload(err) {
			return err
		}
		_, err = s.store.client.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(candidate.ArchiveKey), UploadId: aws.String(id), MaxParts: aws.Int32(1)})
		if !isNoSuchUpload(err) {
			if err != nil {
				return err
			}
			return ErrSnapshotCleanupPending
		}
	}
	if aws.ToBool(out.IsTruncated) {
		return ErrSnapshotCleanupPending
	}
	return nil
}
