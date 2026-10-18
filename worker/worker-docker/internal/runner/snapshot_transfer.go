package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

var errSnapshotTransferRetry = errors.New("snapshot transfer requires a fresh grant or retry")

type snapshotTransfer struct {
	client *http.Client
	slots  chan struct{}
	idle   time.Duration
}

func newSnapshotTransfer(concurrency int, idle time.Duration) (*snapshotTransfer, error) {
	if concurrency < 1 || concurrency > 16 || idle < time.Second {
		return nil, errSnapshotRuntimeUnsupported
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = idle
	transport.MaxConnsPerHost = concurrency
	transport.DialContext = (&net.Dialer{Timeout: idle, KeepAlive: 30 * time.Second}).DialContext
	return &snapshotTransfer{client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, slots: make(chan struct{}, concurrency), idle: idle}, nil
}

func snapshotRequest(ctx context.Context, grant wire.TransferGrant, method string) (*http.Request, context.CancelFunc, error) {
	u, err := url.Parse(grant.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" || grant.Method != method || grant.Size <= 0 || grant.Offset < 0 {
		return nil, nil, errSnapshotArchiveIntegrity
	}
	if !grant.ExpiresAt.After(time.Now()) {
		return nil, nil, errSnapshotTransferRetry
	}
	ctx, cancel := context.WithDeadline(ctx, grant.ExpiresAt)
	request, err := http.NewRequestWithContext(ctx, method, grant.URL, nil)
	if err != nil {
		cancel()
		return nil, nil, errSnapshotArchiveIntegrity
	}
	request.Header = grant.Headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	if host := request.Header.Get("Host"); host != "" && host != u.Host {
		cancel()
		return nil, nil, errSnapshotArchiveIntegrity
	}
	return request, cancel, nil
}

type snapshotProgressReader struct {
	reader   io.Reader
	progress func()
}

func (r snapshotProgressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.progress()
	}
	return n, err
}

func (t *snapshotTransfer) exchange(request *http.Request, consume func(*http.Response) error) error {
	select {
	case t.slots <- struct{}{}:
	case <-request.Context().Done():
		return errSnapshotTransferRetry
	}
	defer func() { <-t.slots }()
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	request = request.Clone(ctx)
	timer := time.AfterFunc(t.idle, cancel)
	defer timer.Stop()
	progress := func() { timer.Reset(t.idle) }
	if request.Body != nil {
		request.Body = io.NopCloser(snapshotProgressReader{reader: request.Body, progress: progress})
	}
	response, err := t.client.Do(request)
	if err != nil {
		return errSnapshotTransferRetry
	}
	defer response.Body.Close()
	response.Body = io.NopCloser(snapshotProgressReader{reader: response.Body, progress: progress})
	return consume(response)
}

func verifySnapshotFile(ctx context.Context, file *os.File, archive wire.Archive) error {
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != archive.Size {
		return errSnapshotArchiveIntegrity
	}
	hash := sha256.New()
	n, err := io.Copy(hash, snapshotContextReader{ctx: ctx, reader: io.NewSectionReader(file, 0, archive.Size)})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || n != archive.Size || hex.EncodeToString(hash.Sum(nil)) != archive.SHA256 {
		return errSnapshotArchiveIntegrity
	}
	return nil
}

func (t *snapshotTransfer) Upload(ctx context.Context, file *os.File, archive wire.Archive, parts []wire.UploadPart) ([]wire.CompletedPart, error) {
	if archive.Validate() != nil || archive.Size > 1<<60 || archive.ExpandedBytes > 1<<60 || len(parts) < 1 || len(parts) > 16 {
		return nil, errSnapshotArchiveIntegrity
	}
	if err := verifySnapshotFile(ctx, file, archive); err != nil {
		return nil, err
	}
	seen := map[int32]bool{}
	for _, part := range parts {
		g := part.Grant
		if seen[part.Number] || part.Number < 1 || part.Number > 10000 || g.Offset < 0 || g.Size <= 0 || g.Offset > archive.Size || g.Size > archive.Size-g.Offset {
			return nil, errSnapshotArchiveIntegrity
		}
		seen[part.Number] = true
	}
	results := make([]wire.CompletedPart, len(parts))
	errs := make([]error, len(parts))
	var wg sync.WaitGroup
	for i, part := range parts {
		wg.Add(1)
		go func(i int, part wire.UploadPart) {
			defer wg.Done()
			request, cancel, err := snapshotRequest(ctx, part.Grant, http.MethodPut)
			if err != nil {
				errs[i] = err
				return
			}
			defer cancel()
			request.Body = io.NopCloser(io.NewSectionReader(file, part.Grant.Offset, part.Grant.Size))
			request.ContentLength = part.Grant.Size
			errs[i] = t.exchange(request, func(response *http.Response) error {
				if response.StatusCode != http.StatusOK {
					return errSnapshotTransferRetry
				}
				etag := response.Header.Get("ETag")
				if etag == "" || len(etag) > 1024 {
					return errSnapshotArchiveIntegrity
				}
				results[i] = wire.CompletedPart{Number: part.Number, Size: part.Grant.Size, ETag: etag}
				return nil
			})
		}(i, part)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

func (t *snapshotTransfer) VerifyRemote(ctx context.Context, grant wire.TransferGrant, archive wire.Archive) error {
	if archive.Validate() != nil || archive.Size > 1<<60 || archive.ExpandedBytes > 1<<60 || grant.Offset != 0 || grant.Size != archive.Size {
		return errSnapshotArchiveIntegrity
	}
	request, cancel, err := snapshotRequest(ctx, grant, http.MethodGet)
	if err != nil {
		return err
	}
	defer cancel()
	return t.exchange(request, func(response *http.Response) error {
		if response.StatusCode != http.StatusOK {
			return errSnapshotTransferRetry
		}
		hash := sha256.New()
		n, err := io.Copy(hash, io.LimitReader(response.Body, archive.Size+1))
		if err != nil {
			return errSnapshotTransferRetry
		}
		if n != archive.Size || hex.EncodeToString(hash.Sum(nil)) != archive.SHA256 {
			return errSnapshotArchiveIntegrity
		}
		return nil
	})
}

// Download resumes only private partial files. Range replies must match the
// exact remainder. A provider that ignores Range is restarted from byte zero.
func (t *snapshotTransfer) Download(ctx context.Context, grant wire.TransferGrant, archive wire.Archive, destination string) error {
	if archive.Validate() != nil || archive.Size > 1<<60 || archive.ExpandedBytes > 1<<60 || grant.Offset != 0 || grant.Size != archive.Size {
		return errSnapshotArchiveIntegrity
	}
	file, err := os.OpenFile(destination+".partial", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errSnapshotJournalUnavailable
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 || stat.Size() > archive.Size {
		return errSnapshotArchiveIntegrity
	}
	offset := stat.Size()
	if offset < archive.Size {
		request, cancel, err := snapshotRequest(ctx, grant, http.MethodGet)
		if err != nil {
			return err
		}
		defer cancel()
		if offset > 0 {
			request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		err = t.exchange(request, func(response *http.Response) error {
			if response.StatusCode == http.StatusOK {
				offset = 0
				if err := file.Truncate(0); err != nil {
					return errSnapshotJournalUnavailable
				}
			} else if response.StatusCode == http.StatusPartialContent && offset > 0 {
				if response.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", offset, archive.Size-1, archive.Size) {
					return errSnapshotArchiveIntegrity
				}
			} else {
				return errSnapshotTransferRetry
			}
			if response.ContentLength >= 0 && response.ContentLength != archive.Size-offset {
				return errSnapshotArchiveIntegrity
			}
			if _, err := file.Seek(offset, io.SeekStart); err != nil {
				return errSnapshotJournalUnavailable
			}
			n, err := io.Copy(file, io.LimitReader(response.Body, archive.Size-offset+1))
			if n > archive.Size-offset {
				return errSnapshotArchiveIntegrity
			}
			if err != nil || n != archive.Size-offset {
				return errSnapshotTransferRetry
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if err := verifySnapshotFile(ctx, file, archive); err != nil {
		_ = os.Remove(destination + ".partial")
		return err
	}
	if err := file.Sync(); err != nil {
		return errSnapshotJournalUnavailable
	}
	if err := file.Close(); err != nil {
		return errSnapshotJournalUnavailable
	}
	if err := os.Rename(destination+".partial", destination); err != nil {
		return errSnapshotJournalUnavailable
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return errSnapshotJournalUnavailable
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return errSnapshotJournalUnavailable
	}
	return nil
}

func parseSnapshotSpace(output string) (int64, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return 0, errSnapshotRuntimeUnsupported
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 6 {
		return 0, errSnapshotRuntimeUnsupported
	}
	available, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || available < 0 || available > 1<<50 {
		return 0, errSnapshotRuntimeUnsupported
	}
	return available * 1024, nil
}
