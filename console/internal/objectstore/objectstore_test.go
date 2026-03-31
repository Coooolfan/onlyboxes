package objectstore

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewRejectsInvalidEndpoint(t *testing.T) {
	_, err := New(Config{
		Endpoint:   "not-a-url",
		Region:     "test-region",
		BucketName: "exports",
		AccessKey:  "ak",
		SecretKey:  "sk",
	})
	if err == nil {
		t.Fatalf("expected invalid endpoint error")
	}
}

func TestNewRejectsMissingFields(t *testing.T) {
	_, err := New(Config{})
	if err == nil || !strings.Contains(err.Error(), "missing endpoint") {
		t.Fatalf("expected missing field error, got %v", err)
	}
}

func TestPresignUploadAndDownload(t *testing.T) {
	store, err := New(Config{
		Endpoint:   "https://minio.example.com",
		Region:     "cn-test-1",
		BucketName: "exports",
		AccessKey:  "ak-test",
		SecretKey:  "sk-test",
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	uploadURL, err := store.PresignUpload(context.Background(), "prefix/session/file.txt", 15*time.Minute)
	if err != nil {
		t.Fatalf("presign upload: %v", err)
	}
	downloadURL, err := store.PresignDownload(context.Background(), "prefix/session/file.txt", time.Hour)
	if err != nil {
		t.Fatalf("presign download: %v", err)
	}

	assertPresignedURL(t, uploadURL, "/exports/prefix/session/file.txt")
	assertPresignedURL(t, downloadURL, "/exports/prefix/session/file.txt")
}

func assertPresignedURL(t *testing.T, rawURL string, wantPath string) {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "minio.example.com" {
		t.Fatalf("unexpected endpoint in presigned url: %s", rawURL)
	}
	if parsed.Path != wantPath {
		t.Fatalf("expected path %q, got %q", wantPath, parsed.Path)
	}
	if parsed.Query().Get("X-Amz-Algorithm") == "" || parsed.Query().Get("X-Amz-Signature") == "" {
		t.Fatalf("expected aws signature query params in %s", rawURL)
	}
}
