package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

func TestReadImageExecutorValidateAndReadSuccessWithDirectoryAllowRule(t *testing.T) {
	tmpDir := t.TempDir()
	allowedDir := filepath.Join(tmpDir, "allowed")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("create allowed dir failed: %v", err)
	}
	imagePath := filepath.Join(allowedDir, "sample.png")
	content := []byte("hello-image")
	if err := os.WriteFile(imagePath, content, 0o600); err != nil {
		t.Fatalf("write test image failed: %v", err)
	}

	executor := newReadImageExecutor([]string{allowedDir})
	validateResult, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  imagePath,
		Action:    readImageActionValidate,
	})
	if err != nil {
		t.Fatalf("validate failed: %v", err)
	}
	if validateResult.SessionID != readImageSessionComputerUse {
		t.Fatalf("expected session_id=%q, got %q", readImageSessionComputerUse, validateResult.SessionID)
	}
	if validateResult.FilePath != filepath.Clean(imagePath) {
		t.Fatalf("expected file_path=%q, got %q", filepath.Clean(imagePath), validateResult.FilePath)
	}
	if validateResult.MIMEType != "image/png" {
		t.Fatalf("expected mime_type=image/png, got %q", validateResult.MIMEType)
	}
	if validateResult.SizeBytes != int64(len(content)) {
		t.Fatalf("expected size=%d, got %d", len(content), validateResult.SizeBytes)
	}
	if len(validateResult.Blob) != 0 {
		t.Fatalf("expected validate blob to be empty")
	}

	readResult, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  imagePath,
		Action:    readImageActionRead,
	})
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(readResult.Blob) != string(content) {
		t.Fatalf("unexpected blob content: %q", string(readResult.Blob))
	}
	if readResult.SizeBytes != int64(len(content)) {
		t.Fatalf("expected read size=%d, got %d", len(content), readResult.SizeBytes)
	}
}

func TestReadImageExecutorValidateAndReadSuccessWithFileAllowRule(t *testing.T) {
	tmpDir := t.TempDir()
	imagePath := filepath.Join(tmpDir, "sample.png")
	content := []byte("hello-image")
	if err := os.WriteFile(imagePath, content, 0o600); err != nil {
		t.Fatalf("write test image failed: %v", err)
	}

	executor := newReadImageExecutor([]string{imagePath})
	validateResult, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  imagePath,
		Action:    readImageActionValidate,
	})
	if err != nil {
		t.Fatalf("validate failed: %v", err)
	}
	if validateResult.MIMEType != "image/png" {
		t.Fatalf("expected mime_type=image/png, got %q", validateResult.MIMEType)
	}

	readResult, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  imagePath,
		Action:    readImageActionRead,
	})
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(readResult.Blob) != string(content) {
		t.Fatalf("unexpected blob content: %q", string(readResult.Blob))
	}
}

func TestReadImageExecutorExportSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sample.png")
	content := []byte("hello-export")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatalf("write test image failed: %v", err)
	}

	var uploadedMethod string
	var uploadedLength int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploadedMethod = r.Method
		uploadedLength = r.ContentLength
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upload body failed: %v", err)
		}
		if string(body) != string(content) {
			t.Fatalf("unexpected upload body: %q", string(body))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := newReadImageExecutor([]string{filePath})
	result, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  filePath,
		Action:    readImageActionExport,
		SignedURL: server.URL,
	})
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	if uploadedMethod != http.MethodPut {
		t.Fatalf("expected PUT upload, got %q", uploadedMethod)
	}
	if uploadedLength != int64(len(content)) {
		t.Fatalf("expected content-length=%d, got %d", len(content), uploadedLength)
	}
	if result.MIMEType != "image/png" {
		t.Fatalf("expected mime_type=image/png, got %q", result.MIMEType)
	}
	if result.SizeBytes != int64(len(content)) {
		t.Fatalf("expected size=%d, got %d", len(content), result.SizeBytes)
	}
	if len(result.Blob) != 0 {
		t.Fatalf("expected export blob to be empty")
	}
}

func TestReadImageExecutorRejectsNonComputerUseSession(t *testing.T) {
	executor := newReadImageExecutor([]string{t.TempDir()})
	_, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: "session-1",
		FilePath:  "/tmp/a.png",
		Action:    readImageActionValidate,
	})
	assertReadImageErrorCode(t, err, readImageCodeSessionNotFound)
}

func TestReadImageExecutorRejectsWhenAllowlistEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	imagePath := filepath.Join(tmpDir, "sample.png")
	if err := os.WriteFile(imagePath, []byte("12345"), 0o600); err != nil {
		t.Fatalf("write test image failed: %v", err)
	}

	executor := newReadImageExecutor(nil)
	_, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  imagePath,
		Action:    readImageActionValidate,
	})
	assertReadImageErrorCode(t, err, readImageCodePathNotAllowed)
}

func TestReadImageExecutorRejectsPathOutsideAllowlist(t *testing.T) {
	tmpDir := t.TempDir()
	allowedDir := filepath.Join(tmpDir, "allowed")
	blockedDir := filepath.Join(tmpDir, "blocked")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("create allowed dir failed: %v", err)
	}
	if err := os.MkdirAll(blockedDir, 0o755); err != nil {
		t.Fatalf("create blocked dir failed: %v", err)
	}
	blockedFile := filepath.Join(blockedDir, "sample.png")
	if err := os.WriteFile(blockedFile, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("write blocked file failed: %v", err)
	}

	executor := newReadImageExecutor([]string{allowedDir})
	_, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  blockedFile,
		Action:    readImageActionValidate,
	})
	assertReadImageErrorCode(t, err, readImageCodePathNotAllowed)
}

func TestReadImageExecutorRejectsTraversalOutsideAllowlist(t *testing.T) {
	tmpDir := t.TempDir()
	allowedDir := filepath.Join(tmpDir, "allowed")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("create allowed dir failed: %v", err)
	}
	outsideFile := filepath.Join(tmpDir, "outside.png")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside file failed: %v", err)
	}

	traversalPath := filepath.Join(allowedDir, "..", "outside.png")
	executor := newReadImageExecutor([]string{allowedDir})
	_, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  traversalPath,
		Action:    readImageActionValidate,
	})
	assertReadImageErrorCode(t, err, readImageCodePathNotAllowed)
}

func TestReadImageExecutorExportRejectsPathOutsideAllowlist(t *testing.T) {
	tmpDir := t.TempDir()
	allowedDir := filepath.Join(tmpDir, "allowed")
	blockedDir := filepath.Join(tmpDir, "blocked")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("create allowed dir failed: %v", err)
	}
	if err := os.MkdirAll(blockedDir, 0o755); err != nil {
		t.Fatalf("create blocked dir failed: %v", err)
	}
	blockedFile := filepath.Join(blockedDir, "sample.png")
	if err := os.WriteFile(blockedFile, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("write blocked file failed: %v", err)
	}

	executor := newReadImageExecutor([]string{allowedDir})
	_, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  blockedFile,
		Action:    readImageActionExport,
		SignedURL: "https://uploads.example.com/put",
	})
	assertReadImageErrorCode(t, err, readImageCodePathNotAllowed)
}

func TestReadImageExecutorRejectsSymlinkEscape(t *testing.T) {
	tmpDir := t.TempDir()
	allowedDir := filepath.Join(tmpDir, "allowed")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("create allowed dir failed: %v", err)
	}
	outsideFile := filepath.Join(tmpDir, "outside.png")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside file failed: %v", err)
	}
	linkPath := filepath.Join(allowedDir, "escape.png")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	executor := newReadImageExecutor([]string{allowedDir})
	_, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  linkPath,
		Action:    readImageActionRead,
	})
	assertReadImageErrorCode(t, err, readImageCodePathNotAllowed)
}

func TestReadImageExecutorRejectsDirectoryPath(t *testing.T) {
	tmpDir := t.TempDir()
	allowedDir := filepath.Join(tmpDir, "allowed")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("create allowed dir failed: %v", err)
	}

	executor := newReadImageExecutor([]string{allowedDir})
	_, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  allowedDir,
		Action:    readImageActionValidate,
	})
	assertReadImageErrorCode(t, err, readImageCodePathIsDirectory)
}

func TestReadImageExecutorMissingFileReturnsFileNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	missingPath := filepath.Join(tmpDir, "missing.png")

	executor := newReadImageExecutor([]string{missingPath})
	_, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  missingPath,
		Action:    readImageActionRead,
	})
	assertReadImageErrorCode(t, err, readImageCodeFileNotFound)
}

func TestReadImageEnsurePathBindingRejectsReplacedFileAfterOpen(t *testing.T) {
	tmpDir := t.TempDir()
	allowedDir := filepath.Join(tmpDir, "allowed")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("create allowed dir failed: %v", err)
	}
	originalPath := filepath.Join(allowedDir, "original.png")
	replacementPath := filepath.Join(allowedDir, "replacement.png")
	if err := os.WriteFile(originalPath, []byte("original"), 0o600); err != nil {
		t.Fatalf("write original file failed: %v", err)
	}
	if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("write replacement file failed: %v", err)
	}

	executor := newReadImageExecutor([]string{allowedDir})
	file, openedInfo, err := openReadImageFile(originalPath)
	if err != nil {
		t.Fatalf("open original file failed: %v", err)
	}
	defer file.Close()

	if err := os.Remove(originalPath); err != nil {
		t.Fatalf("remove original path failed: %v", err)
	}
	if err := os.Rename(replacementPath, originalPath); err != nil {
		t.Fatalf("replace path with new file failed: %v", err)
	}

	err = executor.ensureReadImagePathBinding(originalPath, openedInfo)
	assertReadImageErrorCode(t, err, readImageCodePathNotAllowed)
}

func TestReadImageEnsurePathBindingRejectsSymlinkSwapAfterOpen(t *testing.T) {
	tmpDir := t.TempDir()
	allowedDir := filepath.Join(tmpDir, "allowed")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("create allowed dir failed: %v", err)
	}
	originalPath := filepath.Join(allowedDir, "original.png")
	outsidePath := filepath.Join(tmpDir, "outside.png")
	if err := os.WriteFile(originalPath, []byte("original"), 0o600); err != nil {
		t.Fatalf("write original file failed: %v", err)
	}
	if err := os.WriteFile(outsidePath, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside file failed: %v", err)
	}

	executor := newReadImageExecutor([]string{allowedDir})
	file, openedInfo, err := openReadImageFile(originalPath)
	if err != nil {
		t.Fatalf("open original file failed: %v", err)
	}
	defer file.Close()

	if err := os.Remove(originalPath); err != nil {
		t.Fatalf("remove original path failed: %v", err)
	}
	if err := os.Symlink(outsidePath, originalPath); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	err = executor.ensureReadImagePathBinding(originalPath, openedInfo)
	assertReadImageErrorCode(t, err, readImageCodePathNotAllowed)
}

func TestReadImageExecutorMIMEExtensionPriority(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sample.png")
	if err := os.WriteFile(filePath, []byte("plain-text"), 0o600); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	executor := newReadImageExecutor([]string{filePath})
	result, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  filePath,
		Action:    readImageActionValidate,
	})
	if err != nil {
		t.Fatalf("validate failed: %v", err)
	}
	if result.MIMEType != "image/png" {
		t.Fatalf("expected extension-priority mime_type=image/png, got %q", result.MIMEType)
	}
}

func TestReadImageExecutorMIMESniffFallback(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "image-no-ext")
	pngHeader := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if err := os.WriteFile(filePath, pngHeader, 0o600); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	executor := newReadImageExecutor([]string{filePath})
	result, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  filePath,
		Action:    readImageActionValidate,
	})
	if err != nil {
		t.Fatalf("validate failed: %v", err)
	}
	if result.MIMEType != "image/png" {
		t.Fatalf("expected sniff mime_type=image/png, got %q", result.MIMEType)
	}
}

func TestDetectReadImageMIMEFromFileUsesFileHandle(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "image-no-ext")
	pngHeader := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if err := os.WriteFile(filePath, pngHeader, 0o600); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	file, _, err := openReadImageFile(filePath)
	if err != nil {
		t.Fatalf("open file failed: %v", err)
	}
	defer file.Close()

	if err := os.Remove(filePath); err != nil {
		t.Fatalf("remove path failed: %v", err)
	}

	mimeType, err := detectReadImageMIMEFromFile(filePath, file)
	if err != nil {
		t.Fatalf("detect mime failed: %v", err)
	}
	if mimeType != "image/png" {
		t.Fatalf("expected sniff mime_type=image/png, got %q", mimeType)
	}

	content, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read from opened fd failed: %v", err)
	}
	if string(content) != string(pngHeader) {
		t.Fatalf("unexpected content from opened fd: %x", content)
	}
}

func TestReadImageExecutorMIMEFallbackToOctetStream(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "empty-no-ext")
	if err := os.WriteFile(filePath, []byte{}, 0o600); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	executor := newReadImageExecutor([]string{filePath})
	result, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  filePath,
		Action:    readImageActionValidate,
	})
	if err != nil {
		t.Fatalf("validate failed: %v", err)
	}
	if result.MIMEType != "application/octet-stream" {
		t.Fatalf("expected fallback mime_type=application/octet-stream, got %q", result.MIMEType)
	}
}

func TestReadImageExecutorExportRequiresSignedURL(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sample.png")
	if err := os.WriteFile(filePath, []byte("hello-export"), 0o600); err != nil {
		t.Fatalf("write test file failed: %v", err)
	}

	executor := newReadImageExecutor([]string{filePath})
	_, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  filePath,
		Action:    readImageActionExport,
	})
	assertReadImageErrorCode(t, err, readImageCodeInvalidPayload)
}

func TestReadImageExecutorExportPropagatesUploadFailure(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sample.png")
	if err := os.WriteFile(filePath, []byte("hello-export"), 0o600); err != nil {
		t.Fatalf("write test file failed: %v", err)
	}

	originalHTTPPutFile := httpPutFile
	httpPutFile = func(_ context.Context, signedURL string, uploadPath string) error {
		if signedURL != "https://uploads.example.com/put" {
			t.Fatalf("unexpected signed url: %q", signedURL)
		}
		if uploadPath != filepath.Clean(filePath) {
			t.Fatalf("unexpected upload path: %q", uploadPath)
		}
		return errors.New("upload export file failed: boom")
	}
	t.Cleanup(func() {
		httpPutFile = originalHTTPPutFile
	})

	executor := newReadImageExecutor([]string{filePath})
	_, err := executor.Execute(context.Background(), readImageRequest{
		SessionID: readImageSessionComputerUse,
		FilePath:  filePath,
		Action:    readImageActionExport,
		SignedURL: "https://uploads.example.com/put",
	})
	if err == nil {
		t.Fatalf("expected upload failure")
	}
	if !strings.Contains(err.Error(), "upload export file failed: boom") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildCommandResultReadImageMapsDomainError(t *testing.T) {
	originalRunReadImage := runReadImage
	t.Cleanup(func() {
		runReadImage = originalRunReadImage
	})

	runReadImage = func(_ context.Context, _ readImageRequest) (readImageRunResult, error) {
		return readImageRunResult{}, newReadImageError(readImageCodeFileNotFound, "file not found")
	}

	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:   "cmd-read-image-domain",
		Capability:  readImageCapabilityDeclared,
		PayloadJson: []byte(`{"session_id":"computerUse","file_path":"/tmp/a.png","action":"read"}`),
	})
	result := req.GetCommandResult()
	if result == nil {
		t.Fatalf("expected command_result payload")
	}
	if result.GetError() == nil {
		t.Fatalf("expected command error")
	}
	if result.GetError().GetCode() != readImageCodeFileNotFound {
		t.Fatalf("expected error code %q, got %q", readImageCodeFileNotFound, result.GetError().GetCode())
	}
}

func TestBuildCommandResultReadImageSuccess(t *testing.T) {
	originalRunReadImage := runReadImage
	t.Cleanup(func() {
		runReadImage = originalRunReadImage
	})

	runReadImage = func(_ context.Context, req readImageRequest) (readImageRunResult, error) {
		if req.SessionID != readImageSessionComputerUse || req.FilePath != "/tmp/a.png" || req.Action != readImageActionRead {
			t.Fatalf("unexpected readImage request: %#v", req)
		}
		return readImageRunResult{
			SessionID: readImageSessionComputerUse,
			FilePath:  "/tmp/a.png",
			MIMEType:  "image/png",
			SizeBytes: 3,
			Blob:      []byte("abc"),
		}, nil
	}

	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:   "cmd-read-image-ok",
		Capability:  readImageCapabilityDeclared,
		PayloadJson: []byte(`{"session_id":"computerUse","file_path":"/tmp/a.png","action":"read"}`),
	})
	result := req.GetCommandResult()
	if result == nil {
		t.Fatalf("expected command_result payload")
	}
	if result.GetError() != nil {
		t.Fatalf("expected success, got error %#v", result.GetError())
	}

	decoded := readImageRunResult{}
	if err := json.Unmarshal(result.GetPayloadJson(), &decoded); err != nil {
		t.Fatalf("invalid payload: %v", err)
	}
	if decoded.MIMEType != "image/png" || string(decoded.Blob) != "abc" {
		t.Fatalf("unexpected readImage payload: %#v", decoded)
	}
}

func TestBuildCommandResultReadImageExportPassesSignedURL(t *testing.T) {
	originalRunReadImage := runReadImage
	t.Cleanup(func() {
		runReadImage = originalRunReadImage
	})

	runReadImage = func(_ context.Context, req readImageRequest) (readImageRunResult, error) {
		if req.SessionID != readImageSessionComputerUse ||
			req.FilePath != "/tmp/a.png" ||
			req.Action != readImageActionExport ||
			req.SignedURL != "https://uploads.example.com/put" {
			t.Fatalf("unexpected readImage request: %#v", req)
		}
		return readImageRunResult{
			SessionID: readImageSessionComputerUse,
			FilePath:  "/tmp/a.png",
			MIMEType:  "image/png",
			SizeBytes: 3,
		}, nil
	}

	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:   "cmd-read-image-export",
		Capability:  readImageCapabilityDeclared,
		PayloadJson: []byte(`{"session_id":"computerUse","file_path":"/tmp/a.png","action":"export","signed_url":"https://uploads.example.com/put"}`),
	})
	result := req.GetCommandResult()
	if result == nil {
		t.Fatalf("expected command_result payload")
	}
	if result.GetError() != nil {
		t.Fatalf("expected success, got error %#v", result.GetError())
	}
}

func assertReadImageErrorCode(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %q, got nil", wantCode)
	}
	var readErr *readImageError
	if !errors.As(err, &readErr) {
		t.Fatalf("expected readImageError, got %T", err)
	}
	if readErr.Code() != wantCode {
		t.Fatalf("expected error code %q, got %q", wantCode, readErr.Code())
	}
}
