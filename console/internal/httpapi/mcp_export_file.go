package httpapi

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	exportFileUploadPresignTTL   = 15 * time.Minute
	exportFileDownloadPresignTTL = 1 * time.Hour
)

var newExportObjectID = func() string {
	return uuid.NewString()
}

type ExportStore interface {
	PresignUpload(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error)
	PresignDownload(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error)
}

func handleMCPExportFileTool(
	ctx context.Context,
	dispatcher CommandDispatcher,
	exportStore ExportStore,
	exportPrefix string,
	uploadPresignTTL time.Duration,
	downloadPresignTTL time.Duration,
	returnSchema string,
	input mcpExportFileToolInput,
) (*mcp.CallToolResult, mcpExportFileToolOutput, error) {
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return nil, mcpExportFileToolOutput{}, invalidParamsError("session_id is required")
	}
	filePath := strings.TrimSpace(input.FilePath)
	if filePath == "" {
		return nil, mcpExportFileToolOutput{}, invalidParamsError("file_path is required")
	}

	timeoutMS := defaultMCPTaskTimeoutMS
	if input.TimeoutMS != nil {
		timeoutMS = *input.TimeoutMS
	}
	if timeoutMS < minMCPTaskTimeoutMS || timeoutMS > maxMCPTaskTimeoutMS {
		return nil, mcpExportFileToolOutput{}, invalidParamsError("timeout_ms must be between 1 and 600000")
	}
	if dispatcher == nil {
		return nil, mcpExportFileToolOutput{}, errors.New("task dispatcher is unavailable")
	}
	if exportStore == nil {
		return nil, mcpExportFileToolOutput{}, errors.New("exportFile is unavailable")
	}

	uploadTTL := normalizeExportPresignTTL(uploadPresignTTL, exportFileUploadPresignTTL)
	downloadTTL := normalizeExportPresignTTL(downloadPresignTTL, exportFileDownloadPresignTTL)
	objectKey := buildExportObjectKey(exportPrefix, sessionID, filePath)
	uploadURL, err := exportStore.PresignUpload(ctx, objectKey, uploadTTL)
	if err != nil {
		return nil, mcpExportFileToolOutput{}, errors.New("failed to generate upload URL")
	}

	timeout := time.Duration(timeoutMS) * time.Millisecond
	resourceCapability := terminalResourceCapabilityName
	if sessionID == computerUseSessionID {
		resourceCapability = readImageCapabilityName
	}
	resourceResult, err := callResourceCapability(ctx, dispatcher, resourceCapability, mcpTerminalResourcePayload{
		SessionID: sessionID,
		FilePath:  filePath,
		Action:    "export",
		SignedURL: uploadURL,
		Headers:   exportStoreUploadHeaders(uploadURL),
	}, timeout)
	if err != nil {
		return nil, mcpExportFileToolOutput{}, err
	}
	if strings.TrimSpace(resourceResult.SessionID) == "" {
		return nil, mcpExportFileToolOutput{}, errors.New("invalid exportFile result payload")
	}

	fileName := exportFileName(filePath)

	if returnSchema == ExportReturnSchemaObjectKey {
		return nil, mcpExportFileToolOutput{
			ObjectKey: objectKey,
			FileName:  fileName,
		}, nil
	}

	downloadURL, err := exportStore.PresignDownload(ctx, objectKey, downloadTTL)
	if err != nil {
		return nil, mcpExportFileToolOutput{}, errors.New("failed to generate download URL")
	}

	if returnSchema == ExportReturnSchemaSignedURL {
		return nil, mcpExportFileToolOutput{
			SignedURL: downloadURL,
		}, nil
	}

	return nil, mcpExportFileToolOutput{
		SignedURL: downloadURL,
		ObjectKey: objectKey,
		FileName:  fileName,
	}, nil
}

func exportStoreUploadHeaders(uploadURL string) map[string]string {
	parsed, err := url.Parse(strings.TrimSpace(uploadURL))
	if err != nil {
		return nil
	}
	signedHeaders := strings.Split(parsed.Query().Get("X-Amz-SignedHeaders"), ";")
	headers := make(map[string]string, len(signedHeaders))
	for _, header := range signedHeaders {
		switch strings.ToLower(strings.TrimSpace(header)) {
		case "x-amz-acl":
			headers["x-amz-acl"] = "public-read"
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func normalizeExportPresignTTL(value time.Duration, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func buildExportObjectKey(exportPrefix string, sessionID string, filePath string) string {
	normalizedPrefix := strings.Trim(strings.TrimSpace(exportPrefix), "/")
	sessionSegment := sanitizeExportObjectKeySegment(sessionID, "session")
	fileName := sanitizeExportObjectKeySegment(exportFileName(filePath), "file")
	objectID := sanitizeExportObjectKeySegment(newExportObjectID(), "object")

	parts := make([]string, 0, 3)
	if normalizedPrefix != "" {
		parts = append(parts, normalizedPrefix)
	}
	parts = append(parts, sessionSegment, objectID+"-"+fileName)
	return strings.Join(parts, "/")
}

func exportFileName(filePath string) string {
	base := strings.TrimSpace(filepath.Base(strings.TrimSpace(filePath)))
	switch base {
	case "", ".", string(filepath.Separator):
		return "file"
	default:
		return base
	}
}

func sanitizeExportObjectKeySegment(value string, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}

	var builder strings.Builder
	for _, r := range trimmed {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			builder.WriteByte('_')
			continue
		}
		builder.WriteRune(r)
	}

	sanitized := strings.Trim(strings.TrimSpace(builder.String()), "/")
	if sanitized == "" {
		return fallback
	}
	return sanitized
}
