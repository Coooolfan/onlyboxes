package httpapi

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func NewMCPHandler(dispatcher CommandDispatcher) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    mcpServerName,
		Version: mcpServerVersion,
	}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{
			Logging: &mcp.LoggingCapabilities{},
		},
	})

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
			DestructiveHint: boolPtr(true),
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
			DestructiveHint: boolPtr(true),
			OpenWorldHint:   boolPtr(true),
		},
		InputSchema:  mcpTerminalExecInputSchema,
		OutputSchema: mcpTerminalExecOutputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpTerminalExecToolInput) (*mcp.CallToolResult, mcpTerminalExecToolOutput, error) {
		return handleMCPTerminalExecTool(ctx, dispatcher, input)
	})

	mcp.AddTool(server, &mcp.Tool{
		Title:       mcpReadImageToolTitle,
		Name:        "readImage",
		Description: mcpReadImageToolDescription,
		Annotations: &mcp.ToolAnnotations{
			Title:           mcpReadImageToolTitle,
			DestructiveHint: boolPtr(false),
			OpenWorldHint:   boolPtr(true),
		},
		InputSchema: mcpReadImageInputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpReadImageToolInput) (*mcp.CallToolResult, any, error) {
		return handleMCPReadImageTool(ctx, dispatcher, input)
	})

	return mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
}
