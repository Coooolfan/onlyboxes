package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func NewMCPHandler(
	dispatcher CommandDispatcher,
	workerSysCounter WorkerSysCounter,
	hiddenTools map[string]bool,
	exportStore ExportStore,
	exportPrefix string,
	exportUploadTTL time.Duration,
	exportDownloadTTL time.Duration,
	exportReturnSchema string,
) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    mcpServerName,
		Version: mcpServerVersion,
	}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{
			Logging: &mcp.LoggingCapabilities{},
		},
	})

	nameToCapabilityID := map[string]string{
		"echo":         "echo",
		"pythonExec":   "pythonExec",
		"terminalExec": "terminalExec",
		"computerUse":  "computerUse",
		"readImage":    "readImage",
		"exportFile":   "exportFile",
	}
	mcp.AddTool(server, &mcp.Tool{
		Title:       mcpEchoToolTitle,
		Name:        "echo",
		Description: mcpEchoToolDescription,
		Annotations: &mcp.ToolAnnotations{
			Title:           mcpEchoToolTitle,
			ReadOnlyHint:    true,
			IdempotentHint:  true,
			DestructiveHint: boolPtr(false),
			OpenWorldHint:   boolPtr(false),
		},
		InputSchema:  mcpEchoInputSchema,
		OutputSchema: mcpEchoOutputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpEchoToolInput) (*mcp.CallToolResult, mcpEchoToolOutput, error) {
		return handleMCPEchoTool(ctx, dispatcher, input)
	})

	mcp.AddTool(server, &mcp.Tool{
		Title:       mcpPythonExecToolTitle,
		Name:        "pythonExec",
		Description: mcpPythonExecToolDescription,
		Annotations: &mcp.ToolAnnotations{
			Title:           mcpPythonExecToolTitle,
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(true),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(true),
		},
		InputSchema:  mcpPythonExecInputSchema,
		OutputSchema: mcpPythonExecOutputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpPythonExecToolInput) (*mcp.CallToolResult, mcpPythonExecToolOutput, error) {
		return handleMCPPythonExecTool(ctx, dispatcher, input)
	})

	mcp.AddTool(server, &mcp.Tool{
		Title:       mcpTerminalExecToolTitle,
		Name:        "terminalExec",
		Description: mcpTerminalExecToolDescription,
		Annotations: &mcp.ToolAnnotations{
			Title:           mcpTerminalExecToolTitle,
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(true),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(true),
		},
		InputSchema:  mcpTerminalExecInputSchema,
		OutputSchema: mcpTerminalExecOutputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpTerminalExecToolInput) (*mcp.CallToolResult, mcpTerminalExecToolOutput, error) {
		return handleMCPTerminalExecTool(ctx, dispatcher, input)
	})

	mcp.AddTool(server, &mcp.Tool{
		Title:       mcpComputerUseToolTitle,
		Name:        "computerUse",
		Description: mcpComputerUseToolDescription,
		Annotations: &mcp.ToolAnnotations{
			Title:           mcpComputerUseToolTitle,
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(true),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(true),
		},
		InputSchema:  mcpComputerUseInputSchema,
		OutputSchema: mcpComputerUseOutputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpComputerUseToolInput) (*mcp.CallToolResult, mcpComputerUseToolOutput, error) {
		return handleMCPComputerUseTool(ctx, dispatcher, workerSysCounter, input)
	})

	mcp.AddTool(server, &mcp.Tool{
		Title:       mcpReadImageToolTitle,
		Name:        "readImage",
		Description: mcpReadImageToolDescription,
		Annotations: &mcp.ToolAnnotations{
			Title:           mcpReadImageToolTitle,
			ReadOnlyHint:    true,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
		InputSchema: mcpReadImageInputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpReadImageToolInput) (*mcp.CallToolResult, any, error) {
		return handleMCPReadImageTool(ctx, dispatcher, input)
	})

	if exportStore != nil && strings.TrimSpace(exportPrefix) != "" {
		mcp.AddTool(server, &mcp.Tool{
			Title:       mcpExportFileToolTitle,
			Name:        "exportFile",
			Description: mcpExportFileToolDescription,
			Annotations: &mcp.ToolAnnotations{
				Title:           mcpExportFileToolTitle,
				ReadOnlyHint:    true,
				DestructiveHint: boolPtr(false),
				IdempotentHint:  false,
				OpenWorldHint:   boolPtr(false),
			},
			InputSchema:  mcpExportFileInputSchema,
			OutputSchema: exportFileOutputSchemaForMode(exportReturnSchema),
		}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpExportFileToolInput) (*mcp.CallToolResult, mcpExportFileToolOutput, error) {
			return handleMCPExportFileTool(ctx, dispatcher, exportStore, exportPrefix, exportUploadTTL, exportDownloadTTL, exportReturnSchema, input)
		})
	}

	if len(hiddenTools) > 0 {
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				result, err := next(ctx, method, req)
				if err != nil || method != "tools/list" {
					return result, err
				}
				listResult, ok := result.(*mcp.ListToolsResult)
				if !ok || listResult == nil {
					return result, nil
				}
				filtered := make([]*mcp.Tool, 0, len(listResult.Tools))
				for _, tool := range listResult.Tools {
					if tool == nil {
						continue
					}
					capID, ok := nameToCapabilityID[tool.Name]
					if !ok {
						capID = tool.Name
					}
					if isCapabilityHidden(hiddenTools, capID) {
						continue
					}
					filtered = append(filtered, tool)
				}
				listResult.Tools = filtered
				return listResult, nil
			}
		})
	}

	return mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
}
