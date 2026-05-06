package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/onlyboxes/onlyboxes/console/internal/config"
)

func NewMCPHandler(
	dispatcher CommandDispatcher,
	hiddenTools map[string]bool,
	exportStore ExportStore,
	exportPrefix string,
	exportUploadTTL time.Duration,
	exportDownloadTTL time.Duration,
	exportReturnSchema string,
	toolOverrides map[string]config.MCPToolOverride,
) http.Handler {
	logger := slog.Default()
	server := mcp.NewServer(&mcp.Implementation{
		Name:    mcpServerName,
		Version: mcpServerVersion,
	}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{
			Logging: &mcp.LoggingCapabilities{},
		},
	})

	resolveOverride := func(tool string) config.MCPToolOverride {
		if toolOverrides == nil {
			return config.MCPToolOverride{}
		}
		return toolOverrides[tool]
	}

	// builtinDefaultNames captures every tool's source-level Name. It is used to
	// reject overrides that collide with another capability's built-in default,
	// so that e.g. CONSOLE_MCP_TOOL_ECHO_NAME=pythonExec falls back instead of
	// shadowing pythonExec.
	builtinDefaultNames := map[string]string{
		"echo":         "echo",
		"pythonExec":   "pythonExec",
		"terminalExec": "terminalExec",
		"computerUse":  "computerUse",
		"readImage":    "readImage",
		"exportFile":   "exportFile",
	}
	defaultNameToCap := make(map[string]string, len(builtinDefaultNames))
	for capID, def := range builtinDefaultNames {
		defaultNameToCap[def] = capID
	}
	// nameToCapabilityID maps the FINAL exposed tool name (post-override) back
	// to the internal capability ID. Used by the hiddenTools middleware so
	// filtering keeps working when an override renames a tool.
	nameToCapabilityID := make(map[string]string, len(builtinDefaultNames))
	resolveExposedName := func(capabilityID, defaultName string, override *string) string {
		final := applyToolNameOverride(defaultName, override, logger, capabilityID)
		if final != defaultName {
			if other, ok := defaultNameToCap[final]; ok && other != capabilityID {
				logger.Warn("MCP tool name override collides with another tool's built-in name; falling back to default",
					"tool", capabilityID, "override", final, "conflictsWith", other)
				final = defaultName
			} else if owner, ok := nameToCapabilityID[final]; ok && owner != capabilityID {
				logger.Warn("MCP tool name override already in use by another tool; falling back to default",
					"tool", capabilityID, "override", final, "owner", owner)
				final = defaultName
			}
		}
		if owner, ok := nameToCapabilityID[final]; ok && owner != capabilityID {
			// Fallback also conflicts (another capability already grabbed this
			// default name via override). Refuse to register a duplicate.
			logger.Error("MCP tool default name already taken by another tool's override; aborting MCP server registration",
				"tool", capabilityID, "name", final, "owner", owner)
			panic("onlyboxes: duplicate MCP tool name " + final + " for capability " + capabilityID)
		}
		nameToCapabilityID[final] = capabilityID
		return final
	}

	echoOverride := resolveOverride("echo")
	echoName := resolveExposedName("echo", "echo", echoOverride.Name)
	echoTitle := applyToolTitleOverride(mcpEchoToolTitle, echoOverride.Title, logger, "echo")
	mcp.AddTool(server, &mcp.Tool{
		Title:       echoTitle,
		Name:        echoName,
		Description: applyToolDescriptionOverride(mcpEchoToolDescription, echoOverride.Description, logger, "echo"),
		Annotations: &mcp.ToolAnnotations{
			Title:           echoTitle,
			ReadOnlyHint:    true,
			IdempotentHint:  true,
			DestructiveHint: boolPtr(false),
			OpenWorldHint:   boolPtr(false),
		},
		InputSchema:  applyInputSchemaOverride(mcpEchoInputSchema, echoOverride.ParamDescriptions, logger, "echo"),
		OutputSchema: mcpEchoOutputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpEchoToolInput) (*mcp.CallToolResult, mcpEchoToolOutput, error) {
		return handleMCPEchoTool(ctx, dispatcher, input)
	})

	pyOverride := resolveOverride("pythonExec")
	pyTitle := applyToolTitleOverride(mcpPythonExecToolTitle, pyOverride.Title, logger, "pythonExec")
	pyName := resolveExposedName("pythonExec", "pythonExec", pyOverride.Name)
	mcp.AddTool(server, &mcp.Tool{
		Title:       pyTitle,
		Name:        pyName,
		Description: applyToolDescriptionOverride(mcpPythonExecToolDescription, pyOverride.Description, logger, "pythonExec"),
		Annotations: &mcp.ToolAnnotations{
			Title:           pyTitle,
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(true),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(true),
		},
		InputSchema:  applyInputSchemaOverride(mcpPythonExecInputSchema, pyOverride.ParamDescriptions, logger, "pythonExec"),
		OutputSchema: mcpPythonExecOutputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpPythonExecToolInput) (*mcp.CallToolResult, mcpPythonExecToolOutput, error) {
		return handleMCPPythonExecTool(ctx, dispatcher, input)
	})

	termOverride := resolveOverride("terminalExec")
	termTitle := applyToolTitleOverride(mcpTerminalExecToolTitle, termOverride.Title, logger, "terminalExec")
	termName := resolveExposedName("terminalExec", "terminalExec", termOverride.Name)
	mcp.AddTool(server, &mcp.Tool{
		Title:       termTitle,
		Name:        termName,
		Description: applyToolDescriptionOverride(mcpTerminalExecToolDescription, termOverride.Description, logger, "terminalExec"),
		Annotations: &mcp.ToolAnnotations{
			Title:           termTitle,
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(true),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(true),
		},
		InputSchema:  applyInputSchemaOverride(mcpTerminalExecInputSchema, termOverride.ParamDescriptions, logger, "terminalExec"),
		OutputSchema: mcpTerminalExecOutputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpTerminalExecToolInput) (*mcp.CallToolResult, mcpTerminalExecToolOutput, error) {
		return handleMCPTerminalExecTool(ctx, dispatcher, input)
	})

	cuOverride := resolveOverride("computerUse")
	cuTitle := applyToolTitleOverride(mcpComputerUseToolTitle, cuOverride.Title, logger, "computerUse")
	cuName := resolveExposedName("computerUse", "computerUse", cuOverride.Name)
	mcp.AddTool(server, &mcp.Tool{
		Title:       cuTitle,
		Name:        cuName,
		Description: applyToolDescriptionOverride(mcpComputerUseToolDescription, cuOverride.Description, logger, "computerUse"),
		Annotations: &mcp.ToolAnnotations{
			Title:           cuTitle,
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(true),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(true),
		},
		InputSchema:  applyInputSchemaOverride(mcpComputerUseInputSchema, cuOverride.ParamDescriptions, logger, "computerUse"),
		OutputSchema: mcpComputerUseOutputSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpComputerUseToolInput) (*mcp.CallToolResult, mcpComputerUseToolOutput, error) {
		return handleMCPComputerUseTool(ctx, dispatcher, input)
	})

	riOverride := resolveOverride("readImage")
	riTitle := applyToolTitleOverride(mcpReadImageToolTitle, riOverride.Title, logger, "readImage")
	riName := resolveExposedName("readImage", "readImage", riOverride.Name)
	mcp.AddTool(server, &mcp.Tool{
		Title:       riTitle,
		Name:        riName,
		Description: applyToolDescriptionOverride(mcpReadImageToolDescription, riOverride.Description, logger, "readImage"),
		Annotations: &mcp.ToolAnnotations{
			Title:           riTitle,
			ReadOnlyHint:    true,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
		InputSchema: applyInputSchemaOverride(mcpReadImageInputSchema, riOverride.ParamDescriptions, logger, "readImage"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpReadImageToolInput) (*mcp.CallToolResult, any, error) {
		return handleMCPReadImageTool(ctx, dispatcher, input)
	})

	if exportStore != nil && strings.TrimSpace(exportPrefix) != "" {
		efOverride := resolveOverride("exportFile")
		efTitle := applyToolTitleOverride(mcpExportFileToolTitle, efOverride.Title, logger, "exportFile")
		efName := resolveExposedName("exportFile", "exportFile", efOverride.Name)
		mcp.AddTool(server, &mcp.Tool{
			Title:       efTitle,
			Name:        efName,
			Description: applyToolDescriptionOverride(mcpExportFileToolDescription, efOverride.Description, logger, "exportFile"),
			Annotations: &mcp.ToolAnnotations{
				Title:           efTitle,
				ReadOnlyHint:    true,
				DestructiveHint: boolPtr(false),
				IdempotentHint:  false,
				OpenWorldHint:   boolPtr(false),
			},
			InputSchema:  applyInputSchemaOverride(mcpExportFileInputSchema, efOverride.ParamDescriptions, logger, "exportFile"),
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
