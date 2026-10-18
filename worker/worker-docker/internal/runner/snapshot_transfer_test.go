package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

func transferTestArchive(data []byte) wire.Archive {
	sum := sha256.Sum256(data)
	return wire.Archive{SnapshotID: "snapshot", Size: int64(len(data)), ExpandedBytes: 8192, SHA256: hex.EncodeToString(sum[:]), Format: wire.DockerRootFSFormat}
}
func testSnapshotTransfer(t *testing.T) *snapshotTransfer {
	t.Helper()
	transfer, err := newSnapshotTransfer(2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transfer.client.CloseIdleConnections)
	return transfer
}

func TestSnapshotDownloadResumesAndRestartsIgnoredRange(t *testing.T) {
	for _, ignoreRange := range []bool{false, true} {
		t.Run(fmt.Sprint(ignoreRange), func(t *testing.T) {
			data := []byte("complete snapshot archive")
			archive := transferTestArchive(data)
			var observed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "bytes=5-" {
					t.Error("missing resume Range")
				} else {
					observed.Store(true)
				}
				if ignoreRange {
					_, _ = w.Write(data)
				} else {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 5-%d/%d", len(data)-1, len(data)))
					w.WriteHeader(http.StatusPartialContent)
					_, _ = w.Write(data[5:])
				}
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "archive")
			if err := os.WriteFile(path+".partial", data[:5], 0600); err != nil {
				t.Fatal(err)
			}
			grant := wire.TransferGrant{URL: server.URL, Method: "GET", Size: archive.Size, ExpiresAt: time.Now().Add(time.Minute)}
			if err := testSnapshotTransfer(t).Download(context.Background(), grant, archive, path); err != nil {
				t.Fatal(err)
			}
			actual, err := os.ReadFile(path)
			if err != nil || string(actual) != string(data) || !observed.Load() {
				t.Fatalf("resume changed content: %q %v", actual, err)
			}
		})
	}
}

func TestSnapshotDownloadRejectsCorruptionAndUnsafeRedirect(t *testing.T) {
	data := []byte("snapshot bytes")
	archive := transferTestArchive(data)
	for _, mode := range []string{"corrupt", "redirect", "range-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, "http://not-an-authorized-object.invalid/", 302)
				case "range-mismatch":
					w.Header().Set("Content-Range", "bytes 1-2/3")
					w.WriteHeader(206)
					_, _ = w.Write(data)
				default:
					_, _ = w.Write([]byte(strings.Repeat("x", len(data))))
				}
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "archive")
			if mode == "range-mismatch" {
				_ = os.WriteFile(path+".partial", data[:1], 0600)
			}
			grant := wire.TransferGrant{URL: server.URL + "/?private-signature=never-log", Method: "GET", Size: archive.Size, ExpiresAt: time.Now().Add(time.Minute)}
			err := testSnapshotTransfer(t).Download(context.Background(), grant, archive, path)
			if err == nil || strings.Contains(err.Error(), "private-signature") {
				t.Fatalf("unsafe transfer outcome: %v", err)
			}
			if _, e := os.Stat(path); !os.IsNotExist(e) {
				t.Fatal("invalid archive was published locally")
			}
		})
	}
}

func TestSnapshotTransferExpiredGrantAndIdleBodyCanRetry(t *testing.T) {
	archive := transferTestArchive([]byte("snapshot"))
	transfer := testSnapshotTransfer(t)
	grant := wire.TransferGrant{URL: "http://127.0.0.1/?private-signature=never-log", Method: "GET", Size: archive.Size, ExpiresAt: time.Now().Add(-time.Second)}
	if err := transfer.VerifyRemote(context.Background(), grant, archive); !errors.Is(err, errSnapshotTransferRetry) {
		t.Fatal("expired grant cannot be renewed", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	grant.URL = server.URL
	grant.ExpiresAt = time.Now().Add(time.Minute)
	started := time.Now()
	err := transfer.VerifyRemote(context.Background(), grant, archive)
	if !errors.Is(err, errSnapshotTransferRetry) || time.Since(started) > 3*time.Second {
		t.Fatalf("idle transfer was not bounded: %v", err)
	}
}

func TestSnapshotUploadAcknowledgesExactPartsAndRemoteReadback(t *testing.T) {
	data := []byte("snapshot archive bytes")
	archive := transferTestArchive(data)
	path := filepath.Join(t.TempDir(), "archive")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			body, _ := io.ReadAll(r.Body)
			if string(body) != string(data[3:8]) {
				t.Error("uploaded wrong source section")
			}
			w.Header().Set("ETag", "opaque-part-etag")
			return
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	transfer := testSnapshotTransfer(t)
	parts, err := transfer.Upload(context.Background(), file, archive, []wire.UploadPart{{Number: 2, Grant: wire.TransferGrant{URL: server.URL, Method: "PUT", Offset: 3, Size: 5, ExpiresAt: time.Now().Add(time.Minute)}}})
	if err != nil || len(parts) != 1 || parts[0].Number != 2 || parts[0].Size != 5 || parts[0].ETag != "opaque-part-etag" {
		t.Fatal("lost part plan", parts, err)
	}
	if err := transfer.VerifyRemote(context.Background(), wire.TransferGrant{URL: server.URL, Method: "GET", Size: archive.Size, ExpiresAt: time.Now().Add(time.Minute)}, archive); err != nil {
		t.Fatal(err)
	}
}
