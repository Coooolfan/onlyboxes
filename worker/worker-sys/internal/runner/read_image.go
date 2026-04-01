package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	readImageNotReadyMessage      = "readImage executor is unavailable"
	readImageCodeInvalidPayload   = "invalid_payload"
	readImageCodeSessionNotFound  = "session_not_found"
	readImageCodeFileNotFound     = "file_not_found"
	readImageCodePathIsDirectory  = "path_is_directory"
	readImageCodePathNotAllowed   = "path_not_allowed"
	readImageSessionComputerUse   = "computerUse"
	readImageActionValidate       = "validate"
	readImageActionRead           = "read"
	readImageActionExport         = "export"
	readImageDetectSniffByteLimit = 512
)

type readImagePayload struct {
	SessionID string `json:"session_id"`
	FilePath  string `json:"file_path"`
	Action    string `json:"action,omitempty"`
	SignedURL string `json:"signed_url,omitempty"`
}

type readImageRequest struct {
	SessionID string
	FilePath  string
	Action    string
	SignedURL string
}

type readImageRunResult struct {
	SessionID string `json:"session_id"`
	FilePath  string `json:"file_path"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	Blob      []byte `json:"blob,omitempty"`
}

type readImageError struct {
	code    string
	message string
}

type readImagePathRule struct {
	path  string
	isDir bool
}

var httpPutFile = putFileToSignedURL

func (e *readImageError) Error() string {
	if e == nil {
		return "readImage execution failed"
	}
	return e.message
}

func (e *readImageError) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func newReadImageError(code string, message string) *readImageError {
	return &readImageError{
		code:    strings.TrimSpace(code),
		message: strings.TrimSpace(message),
	}
}

type readImageExecutor struct {
	pathRules []readImagePathRule
}

func newReadImageExecutor(allowedPaths []string) *readImageExecutor {
	return &readImageExecutor{pathRules: compileReadImagePathRules(allowedPaths)}
}

func (e *readImageExecutor) Execute(ctx context.Context, req readImageRequest) (readImageRunResult, error) {
	if e == nil {
		return readImageRunResult{}, newReadImageError("execution_failed", readImageNotReadyMessage)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
		return readImageRunResult{}, ctx.Err()
	}

	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID != readImageSessionComputerUse {
		return readImageRunResult{}, newReadImageError(readImageCodeSessionNotFound, "session not found")
	}
	filePath := strings.TrimSpace(req.FilePath)
	if filePath == "" {
		return readImageRunResult{}, newReadImageError(readImageCodeInvalidPayload, "session_id and file_path are required")
	}
	action := normalizeReadImageAction(req.Action)
	if action == "" {
		return readImageRunResult{}, newReadImageError(readImageCodeInvalidPayload, "action must be validate or read")
	}

	normalizedPath, err := normalizeReadImagePath(filePath)
	if err != nil {
		return readImageRunResult{}, newReadImageError(readImageCodePathNotAllowed, "file path is not allowed")
	}
	if !e.isPathLexicallyAllowed(normalizedPath) {
		return readImageRunResult{}, newReadImageError(readImageCodePathNotAllowed, "file path is not allowed")
	}

	file, openedInfo, err := openReadImageFile(normalizedPath)
	if err != nil {
		var readImageErr *readImageError
		if errors.As(err, &readImageErr) {
			return readImageRunResult{}, readImageErr
		}
		return readImageRunResult{}, fmt.Errorf("open file failed: %w", err)
	}
	defer file.Close()

	if err := e.ensureReadImagePathBinding(normalizedPath, openedInfo); err != nil {
		var readImageErr *readImageError
		if errors.As(err, &readImageErr) {
			return readImageRunResult{}, readImageErr
		}
		return readImageRunResult{}, fmt.Errorf("validate file binding failed: %w", err)
	}

	mimeType, err := detectReadImageMIMEFromFile(normalizedPath, file)
	if err != nil {
		return readImageRunResult{}, fmt.Errorf("detect mime type from file failed: %w", err)
	}

	result := readImageRunResult{
		SessionID: readImageSessionComputerUse,
		FilePath:  normalizedPath,
		MIMEType:  mimeType,
		SizeBytes: openedInfo.Size(),
	}
	if action == readImageActionExport {
		signedURL := strings.TrimSpace(req.SignedURL)
		if signedURL == "" {
			return readImageRunResult{}, newReadImageError(readImageCodeInvalidPayload, "signed_url is required for export")
		}
		if err := httpPutFile(ctx, signedURL, normalizedPath); err != nil {
			return readImageRunResult{}, err
		}
		return result, nil
	}
	if action != readImageActionRead {
		return result, nil
	}

	blob, err := io.ReadAll(file)
	if err != nil {
		return readImageRunResult{}, fmt.Errorf("read file failed: %w", err)
	}
	result.Blob = blob
	result.SizeBytes = int64(len(blob))
	return result, nil
}

func openReadImageFile(filePath string) (*os.File, os.FileInfo, error) {
	file, err := os.Open(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, newReadImageError(readImageCodeFileNotFound, "file not found")
		}
		return nil, nil, err
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if info.IsDir() {
		file.Close()
		return nil, nil, newReadImageError(readImageCodePathIsDirectory, "path is directory")
	}
	return file, info, nil
}

func normalizeReadImageAction(action string) string {
	switch strings.TrimSpace(strings.ToLower(action)) {
	case "":
		return readImageActionValidate
	case readImageActionValidate:
		return readImageActionValidate
	case readImageActionRead:
		return readImageActionRead
	case readImageActionExport:
		return readImageActionExport
	default:
		return ""
	}
}

func putFileToSignedURL(ctx context.Context, signedURL string, filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open export file: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat export file: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPut, signedURL, file)
	if err != nil {
		return fmt.Errorf("build upload request: %w", err)
	}
	request.ContentLength = stat.Size()

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("upload export file: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = response.Status
		}
		return fmt.Errorf("upload export file failed: %s", message)
	}
	return nil
}

func normalizeReadImagePath(rawPath string) (string, error) {
	pathValue := strings.TrimSpace(rawPath)
	if pathValue == "" {
		return "", errors.New("path is required")
	}
	cleaned := filepath.Clean(pathValue)
	absPath, err := filepath.Abs(cleaned)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absPath), nil
}

func compileReadImagePathRules(allowedPaths []string) []readImagePathRule {
	if len(allowedPaths) == 0 {
		return []readImagePathRule{}
	}
	rules := make([]readImagePathRule, 0, len(allowedPaths))
	seen := make(map[string]struct{}, len(allowedPaths))
	for _, entry := range allowedPaths {
		rule, ok := compileReadImagePathRule(entry)
		if !ok {
			continue
		}
		key := rule.path + "|file"
		if rule.isDir {
			key = rule.path + "|dir"
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		rules = append(rules, rule)
	}
	return rules
}

func compileReadImagePathRule(rawPath string) (readImagePathRule, bool) {
	pathValue := strings.TrimSpace(rawPath)
	if pathValue == "" {
		return readImagePathRule{}, false
	}
	normalizedPath, err := normalizeReadImagePath(pathValue)
	if err != nil {
		return readImagePathRule{}, false
	}
	isDir := hasTrailingPathSeparator(pathValue)
	if info, err := os.Stat(normalizedPath); err == nil {
		isDir = info.IsDir()
	}
	return readImagePathRule{path: normalizedPath, isDir: isDir}, true
}

func hasTrailingPathSeparator(pathValue string) bool {
	return strings.HasSuffix(pathValue, "/") || strings.HasSuffix(pathValue, "\\")
}

func (e *readImageExecutor) isPathLexicallyAllowed(pathValue string) bool {
	if e == nil || len(e.pathRules) == 0 {
		return false
	}
	for _, rule := range e.pathRules {
		if readImagePathRuleMatches(rule, pathValue) {
			return true
		}
	}
	return false
}

func (e *readImageExecutor) isPathReallyAllowed(pathValue string) (bool, error) {
	if e == nil || len(e.pathRules) == 0 {
		return false, nil
	}

	resolvedPath, err := evalReadImageRealPath(pathValue)
	if err != nil {
		return false, err
	}

	for _, rule := range e.pathRules {
		resolvedRulePath, err := evalReadImageRealPath(rule.path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, err
		}
		if rule.isDir {
			if readImagePathWithin(resolvedRulePath, resolvedPath) {
				return true, nil
			}
			continue
		}
		if readImagePathsEqual(resolvedRulePath, resolvedPath) {
			return true, nil
		}
	}

	return false, nil
}

func (e *readImageExecutor) ensureReadImagePathBinding(pathValue string, openedInfo os.FileInfo) error {
	if openedInfo == nil {
		return errors.New("opened file info is required")
	}

	reallyAllowed, err := e.isPathReallyAllowed(pathValue)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newReadImageError(readImageCodePathNotAllowed, "file path is not allowed")
		}
		return err
	}
	if !reallyAllowed {
		return newReadImageError(readImageCodePathNotAllowed, "file path is not allowed")
	}

	currentInfo, err := os.Stat(pathValue)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newReadImageError(readImageCodePathNotAllowed, "file path is not allowed")
		}
		return err
	}
	if currentInfo.IsDir() {
		return newReadImageError(readImageCodePathNotAllowed, "file path is not allowed")
	}
	if !os.SameFile(openedInfo, currentInfo) {
		return newReadImageError(readImageCodePathNotAllowed, "file path is not allowed")
	}
	return nil
}

func evalReadImageRealPath(pathValue string) (string, error) {
	resolved, err := filepath.EvalSymlinks(pathValue)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absPath), nil
}

func readImagePathRuleMatches(rule readImagePathRule, pathValue string) bool {
	if rule.isDir {
		return readImagePathWithin(rule.path, pathValue)
	}
	return readImagePathsEqual(rule.path, pathValue)
}

func readImagePathWithin(basePath string, targetPath string) bool {
	rel, err := filepath.Rel(basePath, targetPath)
	if err != nil {
		return false
	}
	cleanRel := filepath.Clean(rel)
	if cleanRel == "." {
		return true
	}
	if cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(os.PathSeparator)) {
		return false
	}
	return !filepath.IsAbs(cleanRel)
}

func readImagePathsEqual(left string, right string) bool {
	rel, err := filepath.Rel(left, right)
	if err != nil {
		return false
	}
	return filepath.Clean(rel) == "."
}

func detectReadImageMIMEFromFile(filePath string, file *os.File) (string, error) {
	ext := strings.ToLower(strings.TrimSpace(filepath.Ext(filePath)))
	if ext != "" {
		if mimeType := strings.TrimSpace(mime.TypeByExtension(ext)); mimeType != "" {
			if parsed, _, err := mime.ParseMediaType(mimeType); err == nil && strings.TrimSpace(parsed) != "" {
				return parsed, nil
			}
			return mimeType, nil
		}
	}

	if file == nil {
		return "", errors.New("file handle is required")
	}

	buf := make([]byte, readImageDetectSniffByteLimit)
	n, err := file.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if n <= 0 {
		return "application/octet-stream", nil
	}
	detected := strings.TrimSpace(http.DetectContentType(buf[:n]))
	if detected == "" {
		return "application/octet-stream", nil
	}
	if parsed, _, err := mime.ParseMediaType(detected); err == nil && strings.TrimSpace(parsed) != "" {
		return parsed, nil
	}
	return detected, nil
}
