package objectstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

var ErrSnapshotCleanupPending = errors.New("snapshot storage cleanup is not confirmed")

func (s *SnapshotStore) AbortAndConfirmSnapshotUpload(ctx context.Context, upload SnapshotUpload) error {
	if err := s.AbortSnapshotUpload(ctx, upload); err != nil {
		return err
	}
	parts, err := s.ListSnapshotParts(ctx, upload)
	if isNoSuchUpload(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(parts) != 0 {
		return ErrSnapshotCleanupPending
	}
	return nil
}

type snapshotDeletionMarker struct {
	Schema   string   `json:"schema"`
	Snapshot string   `json:"snapshot_id"`
	Versions []string `json:"versions_to_delete,omitempty"`
}

// PurgeSnapshot atomically replaces unreferenced user bytes with small deletion
// markers, then removes the old versions by ID. Both keys stay reserved against
// late conditional Complete/Put requests. The marker retains version identities
// so a crash between replacement and deletion cannot lose physical cleanup work.
func (s *SnapshotStore) PurgeSnapshot(ctx context.Context, object SnapshotObject, manifestVersion string) error {
	if err := s.validateObject(object); err != nil {
		return err
	}
	if err := s.purgeSnapshotKey(ctx, object.Key, object.VersionID, object.SnapshotID); err != nil {
		return err
	}
	return s.purgeSnapshotKey(ctx, path.Join(path.Dir(object.Key), "manifest.json"), manifestVersion, object.SnapshotID)
}
func (s *SnapshotStore) purgeSnapshotKey(ctx context.Context, key, knownVersion, snapshot string) error {
	client := s.store.client
	removeVersions := func(versions []string, markerVersion string) error {
		for _, version := range uniqueSnapshotVersions(versions) {
			if version == markerVersion {
				continue
			} // Suspended versioning can reuse "null".
			_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(key), VersionId: aws.String(version)})
			if err != nil && !snapshotObjectMissing(err) {
				return err
			}
		}
		return nil
	}
	for attempt := 0; attempt < 4; attempt++ {
		head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(key)})
		missing := snapshotObjectMissing(err)
		if err != nil && !missing {
			return err
		}
		if !missing && head.Metadata["onlyboxes-deleted"] == "v1" && aws.ToInt64(head.ContentLength) > 0 && aws.ToInt64(head.ContentLength) <= 16<<10 {
			read := &s3.GetObjectInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(key)}
			markerVersion := aws.ToString(head.VersionId)
			if markerVersion != "" {
				read.VersionId = aws.String(markerVersion)
			}
			out, err := client.GetObject(ctx, read)
			if err != nil {
				return err
			}
			data, readErr := io.ReadAll(io.LimitReader(out.Body, 16<<10+1))
			_ = out.Body.Close()
			if readErr != nil {
				return readErr
			}
			var marker snapshotDeletionMarker
			if len(data) <= 16<<10 && json.Unmarshal(data, &marker) == nil && marker.Schema == "onlyboxes.snapshot.deleted.v1" && marker.Snapshot == snapshot && len(marker.Versions) <= 4 {
				return removeVersions(append(marker.Versions, knownVersion), markerVersion)
			}
		}
		versions := []string{knownVersion}
		if !missing {
			versions = append(versions, aws.ToString(head.VersionId))
		}
		marker := snapshotDeletionMarker{Schema: "onlyboxes.snapshot.deleted.v1", Snapshot: snapshot, Versions: uniqueSnapshotVersions(versions)}
		data, _ := json.Marshal(marker)
		if len(data) > 16<<10 {
			return errors.New("snapshot deletion version metadata exceeds its limit")
		}
		input := &s3.PutObjectInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(key), Body: bytes.NewReader(data), ContentType: aws.String("application/json"), Metadata: map[string]string{"onlyboxes-deleted": "v1"}}
		// Existing bytes are replaced atomically, never deleted through an absent
		// key gap. For an absent key, CAS discovers a late completion before capture.
		if missing {
			input.IfNoneMatch = aws.String("*")
		} else {
			if aws.ToString(head.ETag) == "" {
				return errors.New("snapshot storage omitted conditional replacement identity")
			}
			input.IfMatch = head.ETag
		}
		_, err = client.PutObject(ctx, input)
		if err != nil && !isSnapshotPrecondition(err) {
			return err
		}
		// Read the durable marker before deleting recorded versions. This also
		// resolves lost Put acknowledgements on the next invocation.
	}
	return ErrSnapshotCleanupPending
}
func uniqueSnapshotVersions(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
func snapshotObjectMissing(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchVersion")
}
