package objectstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Probe checks the actual endpoint and credentials using an isolated namespace.
// It never reads or deletes session snapshots. Errors deliberately omit signed
// URLs, endpoint credentials and provider response bodies.
func (s *SnapshotStore) Probe(ctx context.Context) (result error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errors.New("snapshot store probe could not create its isolated identity")
	}
	cfg := s.config
	cfg.Prefix = path.Join(cfg.Prefix, "probes", hex.EncodeToString(nonce[:]))
	cfg.MaxBytes = MinSnapshotPartSize + 17
	cfg.PartSize = MinSnapshotPartSize
	probe, err := NewSnapshotStore(cfg)
	if err != nil {
		return errors.New("snapshot store probe configuration failed")
	}
	data := make([]byte, cfg.MaxBytes)
	if _, err := rand.Read(data); err != nil {
		return errors.New("snapshot store probe could not create test bytes")
	}
	digest := sha256.Sum256(data)
	object, err := probe.Object("probe", "probe", "archive", int64(len(data)), hex.EncodeToString(digest[:]))
	if err != nil {
		return errors.New("snapshot store probe object failed")
	}
	var uploads []SnapshotUpload
	var manifestVersion string
	var extraArchives []SnapshotObject
	var extraManifests []string
	cleaned := false
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		failed := false
		for _, upload := range uploads {
			if probe.AbortSnapshotUpload(cleanup, upload) != nil {
				failed = true
			}
		}
		for _, version := range extraManifests {
			if _, err := probe.store.client.DeleteObject(cleanup, &s3.DeleteObjectInput{Bucket: aws.String(cfg.BucketName), Key: aws.String(path.Join(path.Dir(object.Key), "manifest.json")), VersionId: aws.String(version)}); err != nil {
				failed = true
			}
		}
		for _, extra := range extraArchives {
			if probe.DeleteSnapshot(cleanup, extra, manifestVersion) != nil {
				failed = true
			}
		}
		if !cleaned && probe.DeleteSnapshot(cleanup, object, manifestVersion) != nil {
			failed = true
		}
		if result == nil && failed {
			result = errors.New("snapshot store probe cleanup failed")
		}
	}()
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(grant TransferGrant, body io.Reader) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, grant.Method, grant.URL, body)
		if err != nil {
			return nil, err
		}
		req.Header = grant.Headers.Clone()
		if body != nil {
			req.ContentLength = grant.Size
		}
		return client.Do(req)
	}
	uploadBytes := func(target SnapshotObject, body []byte) (SnapshotUpload, []SnapshotPart, error) {
		upload, err := probe.BeginUpload(ctx, target)
		if err != nil {
			return upload, nil, err
		}
		uploads = append(uploads, upload)
		var parts []SnapshotPart
		for number, offset := int32(1), int64(0); offset < int64(len(body)); number++ {
			grant, err := probe.PresignPart(ctx, upload, number)
			if err != nil {
				return upload, nil, err
			}
			response, err := request(grant, bytes.NewReader(body[offset:offset+grant.Size]))
			if err != nil {
				return upload, nil, err
			}
			_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			etag := strings.TrimSpace(response.Header.Get("ETag"))
			if response.StatusCode < 200 || response.StatusCode >= 300 || etag == "" || readErr != nil {
				return upload, nil, errors.New("part rejected")
			}
			parts = append(parts, SnapshotPart{Number: number, Size: grant.Size, ETag: etag})
			offset += grant.Size
		}
		return upload, parts, nil
	}
	upload, parts, err := uploadBytes(object, data)
	if err != nil {
		return errors.New("snapshot store probe signed multipart upload failed")
	}
	completed, err := probe.CompleteUploadWithVerifier(ctx, upload, parts, probe.VerifySnapshot)
	if err != nil {
		return errors.New("snapshot store probe multipart completion or readback failed")
	}
	object = completed
	if err := probe.VerifySnapshot(ctx, object); err != nil {
		return errors.New("snapshot store probe immediate HEAD/GET verification failed")
	}

	invalidMatch, matchErr := probe.store.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(cfg.BucketName), Key: aws.String(object.Key), Body: bytes.NewReader([]byte("probe-if-match-rejection")), IfMatch: aws.String(`"onlyboxes-probe-must-not-match"`)})
	if !isSnapshotPrecondition(matchErr) {
		if matchErr == nil && invalidMatch != nil && aws.ToString(invalidMatch.VersionId) != "" && aws.ToString(invalidMatch.VersionId) != object.VersionID {
			extra := object
			extra.VersionID = aws.ToString(invalidMatch.VersionId)
			extraArchives = append(extraArchives, extra)
		}
		return errors.New("snapshot store probe conditional replacement was not enforced")
	}
	grant, err := probe.PresignSnapshotDownload(ctx, object, object.Size-17)
	if err != nil {
		return errors.New("snapshot store probe signed range request failed")
	}
	response, err := request(grant, nil)
	if err != nil {
		return errors.New("snapshot store probe signed range GET failed")
	}
	tail, readErr := io.ReadAll(io.LimitReader(response.Body, 18))
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusPartialContent || response.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", object.Size-17, object.Size-1, object.Size) || !bytes.Equal(tail, data[len(data)-17:]) {
		return errors.New("snapshot store probe range GET integrity failed")
	}
	manifestVersion, err = probe.PutSnapshotManifest(ctx, object, json.RawMessage(`{"probe":1}`))
	if err != nil {
		return errors.New("snapshot store probe conditional manifest write failed")
	}
	unexpectedManifest, err := probe.PutSnapshotManifest(ctx, object, json.RawMessage(`{"probe":2}`))
	if !errors.Is(err, ErrSnapshotConflict) {
		if err == nil && unexpectedManifest != "" && unexpectedManifest != manifestVersion {
			extraManifests = append(extraManifests, unexpectedManifest)
		}
		return errors.New("snapshot store probe manifest overwrite was not rejected")
	}
	// A second multipart at the same key must never overwrite committed bytes.
	replacementHash := sha256.Sum256([]byte("x"))
	replacement, err := probe.Object("probe", "probe", "archive", 1, hex.EncodeToString(replacementHash[:]))
	if err != nil {
		return errors.New("snapshot store probe replacement identity failed")
	}
	duplicate, duplicateParts, err := uploadBytes(replacement, []byte("x"))
	if err != nil {
		return errors.New("snapshot store probe conditional completion setup failed")
	}
	overwrite, err := probe.store.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(cfg.BucketName), Key: aws.String(replacement.Key), UploadId: aws.String(duplicate.UploadID), IfNoneMatch: aws.String("*"), MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: aws.String(duplicateParts[0].ETag)}}}})
	if !isSnapshotPrecondition(err) {
		if err == nil && overwrite != nil && aws.ToString(overwrite.VersionId) != "" && aws.ToString(overwrite.VersionId) != object.VersionID {
			extra := object
			extra.VersionID = aws.ToString(overwrite.VersionId)
			extraArchives = append(extraArchives, extra)
		}
		return errors.New("snapshot store probe archive overwrite was not rejected")
	}
	clientS3 := probe.store.client
	listed, err := clientS3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(cfg.BucketName), Prefix: aws.String(cfg.Prefix + "/")})
	if err != nil {
		return errors.New("snapshot store probe object listing failed")
	}
	archiveFound, manifestFound := false, false
	for _, entry := range listed.Contents {
		archiveFound = archiveFound || aws.ToString(entry.Key) == object.Key
		manifestFound = manifestFound || aws.ToString(entry.Key) == path.Join(path.Dir(object.Key), "manifest.json")
	}
	if !archiveFound || !manifestFound || aws.ToBool(listed.IsTruncated) {
		return errors.New("snapshot store probe object listing was incomplete")
	}
	abortObject, err := probe.Object("probe", "probe", "abort", 1, hex.EncodeToString(replacementHash[:]))
	if err != nil {
		return errors.New("snapshot store probe abort identity failed")
	}
	abortUpload, err := probe.BeginUpload(ctx, abortObject)
	if err != nil {
		return errors.New("snapshot store probe abort setup failed")
	}
	uploads = append(uploads, abortUpload)
	pending, err := clientS3.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(cfg.BucketName), Prefix: aws.String(cfg.Prefix + "/")})
	if err != nil {
		return errors.New("snapshot store probe multipart listing failed")
	}
	duplicateFound := false
	for _, entry := range pending.Uploads {
		duplicateFound = duplicateFound || (aws.ToString(entry.UploadId) == abortUpload.UploadID && aws.ToString(entry.Key) == abortObject.Key)
	}
	if !duplicateFound || aws.ToBool(pending.IsTruncated) {
		return errors.New("snapshot store probe pending multipart listing was incomplete")
	}
	if err := probe.AbortSnapshotUpload(ctx, abortUpload); err != nil {
		return errors.New("snapshot store probe multipart abort failed")
	}

	if err := probe.PurgeSnapshot(ctx, object, manifestVersion); err != nil {
		return errors.New("snapshot store probe cleanup replacement or version deletion failed")
	}
	archiveMarker, err := clientS3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(cfg.BucketName), Key: aws.String(object.Key)})
	if err != nil {
		return errors.New("snapshot store probe archive deletion marker was not readable")
	}
	manifestMarker, err := clientS3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(cfg.BucketName), Key: aws.String(path.Join(path.Dir(object.Key), "manifest.json"))})
	if err != nil {
		return errors.New("snapshot store probe descriptor deletion marker was not readable")
	}
	object.VersionID = aws.ToString(archiveMarker.VersionId)
	manifestVersion = aws.ToString(manifestMarker.VersionId)
	if err := probe.DeleteSnapshot(ctx, object, manifestVersion); err != nil {
		return errors.New("snapshot store probe object deletion failed")
	}
	head := &s3.HeadObjectInput{Bucket: aws.String(cfg.BucketName), Key: aws.String(object.Key)}
	if object.VersionID != "" {
		head.VersionId = aws.String(object.VersionID)
	}
	_, err = clientS3.HeadObject(ctx, head)
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || (apiErr.ErrorCode() != "NotFound" && apiErr.ErrorCode() != "NoSuchKey" && apiErr.ErrorCode() != "NoSuchVersion") {
		return errors.New("snapshot store probe deletion could not be confirmed")
	}
	cleaned = true
	return nil
}
