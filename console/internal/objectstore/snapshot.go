package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const (
	MinSnapshotPartSize      int64 = 5 << 20
	MaxSnapshotPartSize      int64 = 5 << 30
	MaxSnapshotParts               = 10000
	MaxSnapshotManifestBytes       = 256 << 10
)

var (
	ErrSnapshotIntegrity = errors.New("snapshot integrity check failed")
	ErrSnapshotConflict  = errors.New("snapshot object already exists")
	snapshotIDPattern    = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
)

// SnapshotConfig configures a namespace independent from exported user files.
// Transfers use multipart and full-object readback verification.
type SnapshotConfig struct {
	Config
	Prefix     string
	MaxBytes   int64
	PartSize   int64
	PresignTTL time.Duration
}

type SnapshotStore struct {
	store     *Store
	config    SnapshotConfig
	namespace string
	now       func() time.Time
}

// SnapshotObject identifies immutable bytes, not a renewable download URL.
type SnapshotObject struct {
	StoreID    string `json:"store_id"`
	OwnerID    string `json:"owner_id"`
	SessionID  string `json:"session_id"`
	SnapshotID string `json:"snapshot_id"`
	Key        string `json:"key"`
	VersionID  string `json:"version_id,omitempty"`
	Size       int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
}

type SnapshotUpload struct {
	Object        SnapshotObject `json:"object"`
	UploadID      string         `json:"upload_id"`
	PartSize      int64          `json:"part_size"`
	ExpandedBytes int64          `json:"expanded_bytes"`
}

type SnapshotPart struct {
	Number int32  `json:"part_number"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size_bytes"`
}

// TransferGrant must not be persisted or logged; refresh it using stable IDs.
type TransferGrant struct {
	URL       string      `json:"url"`
	Method    string      `json:"method"`
	Headers   http.Header `json:"headers"`
	ExpiresAt time.Time   `json:"expires_at"`
	Offset    int64       `json:"offset"`
	Size      int64       `json:"size_bytes"`
}

func NewSnapshotStore(cfg SnapshotConfig) (*SnapshotStore, error) {
	cfg.Prefix = strings.TrimSpace(cfg.Prefix)
	if cfg.Prefix == "" || strings.HasPrefix(cfg.Prefix, "/") || strings.Contains(cfg.Prefix, "\\") || path.Clean(cfg.Prefix) != cfg.Prefix {
		return nil, errors.New("snapshot prefix must be a clean, non-empty relative key prefix")
	}
	for _, part := range strings.Split(cfg.Prefix, "/") {
		if part == "." || part == ".." {
			return nil, errors.New("snapshot prefix cannot contain parent segments")
		}
	}
	if cfg.MaxBytes <= 0 || cfg.PartSize < MinSnapshotPartSize || cfg.PartSize > MaxSnapshotPartSize || (cfg.MaxBytes-1)/cfg.PartSize+1 > MaxSnapshotParts {
		return nil, errors.New("invalid snapshot size or multipart part size")
	}
	if cfg.PresignTTL <= 0 || cfg.PresignTTL > 7*24*time.Hour {
		return nil, errors.New("snapshot presign TTL must be positive and at most seven days")
	}
	parsed, err := url.Parse(cfg.Endpoint)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("snapshot endpoint must be an HTTP(S) URL without credentials, query or fragment")
	}
	st, err := New(cfg.Config)
	if err != nil {
		return nil, err
	}
	namespace := sha256.Sum256([]byte(strings.TrimRight(cfg.Endpoint, "/") + "\x00" + cfg.Region + "\x00" + cfg.BucketName + "\x00" + cfg.Prefix))
	return &SnapshotStore{store: st, config: cfg, namespace: hex.EncodeToString(namespace[:]), now: time.Now}, nil
}

func (s *SnapshotStore) ID() string { return s.namespace }

func (s *SnapshotStore) Object(owner, session, id string, size int64, digest string) (SnapshotObject, error) {
	if owner == "" || session == "" || !snapshotIDPattern.MatchString(id) || size <= 0 || size > s.config.MaxBytes || !validSHA256(digest) {
		return SnapshotObject{}, errors.New("invalid snapshot identity, size or SHA-256")
	}
	ownerHash := sha256.Sum256([]byte(owner))
	sessionHash := sha256.Sum256([]byte(session))
	key := path.Join(s.config.Prefix, "v1", hex.EncodeToString(ownerHash[:]), hex.EncodeToString(sessionHash[:]), id, "archive")
	return SnapshotObject{StoreID: s.namespace, OwnerID: owner, SessionID: session, SnapshotID: id, Key: key, Size: size, SHA256: digest}, nil
}

// Validate checks an object's scope without applying the current admission size
// limit, so lowering a deployment limit cannot invalidate committed snapshots.
func (object SnapshotObject) Validate() error {
	if object.OwnerID == "" || object.SessionID == "" || !snapshotIDPattern.MatchString(object.SnapshotID) || !validSHA256(object.SHA256) || !validSHA256(object.StoreID) || object.Size <= 0 || object.Size > MaxSnapshotPartSize*MaxSnapshotParts {
		return errors.New("invalid snapshot object identity or size")
	}
	for _, segment := range strings.Split(object.Key, "/") {
		if segment == "." || segment == ".." {
			return errors.New("invalid scoped snapshot key")
		}
	}
	owner := sha256.Sum256([]byte(object.OwnerID))
	session := sha256.Sum256([]byte(object.SessionID))
	suffix := path.Join("v1", hex.EncodeToString(owner[:]), hex.EncodeToString(session[:]), object.SnapshotID, "archive")
	if path.Clean(object.Key) != object.Key || strings.HasPrefix(object.Key, "/") || strings.Contains(object.Key, "\\") || !strings.HasSuffix(object.Key, "/"+suffix) {
		return errors.New("invalid scoped snapshot key")
	}
	return nil
}

func (s *SnapshotStore) validateObject(object SnapshotObject) error {
	if err := object.Validate(); err != nil {
		return err
	}
	owner := sha256.Sum256([]byte(object.OwnerID))
	session := sha256.Sum256([]byte(object.SessionID))
	expected := path.Join(s.config.Prefix, "v1", hex.EncodeToString(owner[:]), hex.EncodeToString(session[:]), object.SnapshotID, "archive")
	if expected != object.Key || object.StoreID != s.namespace {
		return errors.New("snapshot object does not belong to this store and namespace")
	}
	return nil
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}

func (s *SnapshotStore) BeginUpload(ctx context.Context, object SnapshotObject) (SnapshotUpload, error) {
	if err := s.validateObject(object); err != nil {
		return SnapshotUpload{}, err
	}
	if object.Size > s.config.MaxBytes {
		return SnapshotUpload{}, errors.New("snapshot exceeds current admission size limit")
	}
	if object.VersionID != "" {
		return SnapshotUpload{}, errors.New("cannot upload an existing snapshot object version")
	}
	out, err := s.store.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(object.Key), ContentType: aws.String("application/octet-stream")})
	if err != nil {
		return SnapshotUpload{}, fmt.Errorf("begin snapshot upload: %w", err)
	}
	if aws.ToString(out.UploadId) == "" {
		return SnapshotUpload{}, errors.New("snapshot store returned an empty upload ID")
	}
	return SnapshotUpload{Object: object, UploadID: *out.UploadId, PartSize: s.config.PartSize}, nil
}

func (s *SnapshotStore) validateUpload(upload SnapshotUpload) error {
	if err := s.validateObject(upload.Object); err != nil {
		return err
	}
	if upload.UploadID == "" || (upload.PartSize < MinSnapshotPartSize || upload.PartSize > MaxSnapshotPartSize || (upload.Object.Size-1)/upload.PartSize+1 > MaxSnapshotParts) || upload.Object.VersionID != "" {
		return errors.New("invalid snapshot multipart upload")
	}
	return nil
}

func (s *SnapshotStore) partSize(upload SnapshotUpload, number int32) (int64, error) {
	count := (upload.Object.Size-1)/upload.PartSize + 1
	if number < 1 || int64(number) > count {
		return 0, errors.New("snapshot part number is outside the upload plan")
	}
	remaining := upload.Object.Size - int64(number-1)*upload.PartSize
	if remaining < upload.PartSize {
		return remaining, nil
	}
	return upload.PartSize, nil
}

func (s *SnapshotStore) PresignPart(ctx context.Context, upload SnapshotUpload, number int32) (TransferGrant, error) {
	if err := s.validateUpload(upload); err != nil {
		return TransferGrant{}, err
	}
	size, err := s.partSize(upload, number)
	if err != nil {
		return TransferGrant{}, err
	}
	out, err := s.store.presign.PresignUploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(upload.Object.Key), UploadId: aws.String(upload.UploadID), PartNumber: aws.Int32(number), ContentLength: aws.Int64(size)}, func(o *s3.PresignOptions) { o.Expires = s.config.PresignTTL })
	if err != nil {
		return TransferGrant{}, fmt.Errorf("presign snapshot part: %w", err)
	}
	return TransferGrant{URL: out.URL, Method: out.Method, Headers: out.SignedHeader.Clone(), ExpiresAt: s.now().Add(s.config.PresignTTL), Offset: int64(number-1) * upload.PartSize, Size: size}, nil
}

func (s *SnapshotStore) PresignSnapshotDownload(ctx context.Context, object SnapshotObject, offset int64) (TransferGrant, error) {
	if err := s.validateObject(object); err != nil {
		return TransferGrant{}, err
	}
	if offset < 0 || offset >= object.Size {
		return TransferGrant{}, errors.New("invalid snapshot download offset")
	}
	in := &s3.GetObjectInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(object.Key)}
	if object.VersionID != "" {
		in.VersionId = aws.String(object.VersionID)
	}
	if offset > 0 {
		in.Range = aws.String(fmt.Sprintf("bytes=%d-", offset))
	}
	out, err := s.store.presign.PresignGetObject(ctx, in, func(o *s3.PresignOptions) { o.Expires = s.config.PresignTTL })
	if err != nil {
		return TransferGrant{}, fmt.Errorf("presign snapshot download: %w", err)
	}
	return TransferGrant{URL: out.URL, Method: out.Method, Headers: out.SignedHeader.Clone(), ExpiresAt: s.now().Add(s.config.PresignTTL), Offset: offset, Size: object.Size - offset}, nil
}

func (s *SnapshotStore) ListSnapshotParts(ctx context.Context, upload SnapshotUpload) ([]SnapshotPart, error) {
	if err := s.validateUpload(upload); err != nil {
		return nil, err
	}
	paginator := s3.NewListPartsPaginator(s.store.client, &s3.ListPartsInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(upload.Object.Key), UploadId: aws.String(upload.UploadID)})
	var parts []SnapshotPart
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list snapshot parts: %w", err)
		}
		for _, p := range page.Parts {
			parts = append(parts, SnapshotPart{Number: aws.ToInt32(p.PartNumber), ETag: aws.ToString(p.ETag), Size: aws.ToInt64(p.Size)})
		}
		if len(parts) > MaxSnapshotParts {
			return nil, errors.New("snapshot store returned too many parts")
		}
	}
	return parts, nil
}

// CompleteUpload verifies the persisted worker-reported plan against the store.
// If-None-Match prevents response-loss retries from overwriting committed bytes.
func (s *SnapshotStore) CompleteUpload(ctx context.Context, upload SnapshotUpload, parts []SnapshotPart) (SnapshotObject, error) {
	return s.CompleteUploadWithVerifier(ctx, upload, parts, s.VerifySnapshot)
}

// CompleteUploadWithVerifier keeps ambiguous-Complete readback on the worker.
// The verifier must read the full immutable object and check its size/SHA-256.
func (s *SnapshotStore) CompleteUploadWithVerifier(ctx context.Context, upload SnapshotUpload, parts []SnapshotPart, verify func(context.Context, SnapshotObject) error) (SnapshotObject, error) {
	if verify == nil {
		return SnapshotObject{}, errors.New("snapshot verifier is required")
	}
	if err := s.validateUpload(upload); err != nil {
		return SnapshotObject{}, err
	}
	count := (upload.Object.Size-1)/upload.PartSize + 1
	if int64(len(parts)) != count {
		return SnapshotObject{}, errors.New("incomplete snapshot part plan")
	}
	completed := make([]types.CompletedPart, len(parts))
	for i, part := range parts {
		expected, err := s.partSize(upload, int32(i+1))
		if err != nil || part.Number != int32(i+1) || part.ETag == "" || part.Size != expected {
			return SnapshotObject{}, errors.New("invalid snapshot part plan")
		}
		completed[i] = types.CompletedPart{PartNumber: aws.Int32(part.Number), ETag: aws.String(part.ETag)}
	}
	stored, err := s.ListSnapshotParts(ctx, upload)
	if isNoSuchUpload(err) {
		return s.recoverCompletedUpload(ctx, upload, verify)
	}
	if err != nil {
		return SnapshotObject{}, err
	}
	if len(stored) != len(parts) {
		return SnapshotObject{}, errors.New("snapshot part list differs from the reported plan")
	}
	for i := range stored {
		if stored[i] != parts[i] {
			return SnapshotObject{}, errors.New("snapshot part list differs from the reported plan")
		}
	}
	out, err := s.store.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(upload.Object.Key), UploadId: aws.String(upload.UploadID), IfNoneMatch: aws.String("*"), MultipartUpload: &types.CompletedMultipartUpload{Parts: completed}})
	if isSnapshotPrecondition(err) || isNoSuchUpload(err) {
		return s.recoverCompletedUpload(ctx, upload, verify)
	}
	if err != nil {
		return SnapshotObject{}, fmt.Errorf("complete snapshot upload: %w", err)
	}
	object := upload.Object
	object.VersionID = aws.ToString(out.VersionId)
	return object, nil
}

func (s *SnapshotStore) AbortSnapshotUpload(ctx context.Context, upload SnapshotUpload) error {
	if err := s.validateUpload(upload); err != nil {
		return err
	}
	_, err := s.store.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(upload.Object.Key), UploadId: aws.String(upload.UploadID)})
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchUpload" {
		return nil
	}
	return err
}

// VerifySnapshot computes SHA-256 over the complete remote object. ETag and
// caller-supplied object metadata are deliberately not used as checksums.
func (s *SnapshotStore) VerifySnapshot(ctx context.Context, object SnapshotObject) error {
	if err := s.validateObject(object); err != nil {
		return err
	}
	head := &s3.HeadObjectInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(object.Key)}
	get := &s3.GetObjectInput{Bucket: head.Bucket, Key: head.Key}
	if object.VersionID != "" {
		head.VersionId = aws.String(object.VersionID)
		get.VersionId = head.VersionId
	}
	metadata, err := s.store.client.HeadObject(ctx, head)
	if err != nil {
		return fmt.Errorf("inspect snapshot: %w", err)
	}
	if aws.ToInt64(metadata.ContentLength) != object.Size {
		return ErrSnapshotIntegrity
	}
	out, err := s.store.client.GetObject(ctx, get)
	if err != nil {
		return fmt.Errorf("read snapshot: %w", err)
	}
	defer out.Body.Close()
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(out.Body, object.Size+1))
	if err != nil {
		return fmt.Errorf("verify snapshot read: %w", err)
	}
	if n != object.Size || hex.EncodeToString(digest.Sum(nil)) != object.SHA256 {
		return ErrSnapshotIntegrity
	}
	return nil
}

// PutSnapshotManifest writes once; retrying identical bytes is safe after a
// lost response, while a different existing manifest is a conflict.
func (s *SnapshotStore) PutSnapshotManifest(ctx context.Context, object SnapshotObject, manifest json.RawMessage) (string, error) {
	if err := s.validateObject(object); err != nil {
		return "", err
	}
	if len(manifest) == 0 || len(manifest) > MaxSnapshotManifestBytes || !json.Valid(manifest) {
		return "", errors.New("invalid snapshot manifest")
	}
	key := path.Join(path.Dir(object.Key), "manifest.json")
	created, err := s.store.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(key), Body: bytes.NewReader(manifest), ContentType: aws.String("application/json"), IfNoneMatch: aws.String("*")})
	if err == nil {
		return aws.ToString(created.VersionId), nil
	}
	if !isSnapshotPrecondition(err) {
		return "", err
	}
	out, err := s.store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(key)})
	if err != nil {
		return "", err
	}
	defer out.Body.Close()
	existing, err := io.ReadAll(io.LimitReader(out.Body, MaxSnapshotManifestBytes+1))
	if err != nil {
		return "", err
	}
	if !bytes.Equal(existing, manifest) {
		return "", ErrSnapshotConflict
	}
	return aws.ToString(out.VersionId), nil
}

// DeleteSnapshot requires the caller to tombstone an unreferenced snapshot first.
func (s *SnapshotStore) DeleteSnapshot(ctx context.Context, object SnapshotObject, manifestVersionID string) error {
	if err := s.validateObject(object); err != nil {
		return err
	}
	archive := &s3.DeleteObjectInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(object.Key)}
	if object.VersionID != "" {
		archive.VersionId = aws.String(object.VersionID)
	}
	if _, err := s.store.client.DeleteObject(ctx, archive); err != nil {
		return err
	}
	descriptor := &s3.DeleteObjectInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(path.Join(path.Dir(object.Key), "manifest.json"))}
	if manifestVersionID != "" {
		descriptor.VersionId = aws.String(manifestVersionID)
	}
	_, err := s.store.client.DeleteObject(ctx, descriptor)
	return err
}

func isSnapshotPrecondition(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "PreconditionFailed"
}

// RecoverCompletedUpload handles an ambiguous Complete response. Only the exact
// reserved size and SHA-256 can satisfy the retry; a matching key is insufficient.
func (s *SnapshotStore) RecoverCompletedUpload(ctx context.Context, upload SnapshotUpload) (SnapshotObject, error) {
	return s.recoverCompletedUpload(ctx, upload, s.VerifySnapshot)
}

func (s *SnapshotStore) recoverCompletedUpload(ctx context.Context, upload SnapshotUpload, verify func(context.Context, SnapshotObject) error) (SnapshotObject, error) {
	if err := s.validateUpload(upload); err != nil {
		return SnapshotObject{}, err
	}
	head, err := s.store.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.config.BucketName), Key: aws.String(upload.Object.Key)})
	if err != nil {
		return SnapshotObject{}, fmt.Errorf("recover completed snapshot: %w", err)
	}
	object := upload.Object
	object.VersionID = aws.ToString(head.VersionId)
	if aws.ToInt64(head.ContentLength) != object.Size {
		return SnapshotObject{}, ErrSnapshotIntegrity
	}
	if err = verify(ctx, object); err != nil {
		return SnapshotObject{}, err
	}
	return object, nil
}
func isNoSuchUpload(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchUpload"
}
