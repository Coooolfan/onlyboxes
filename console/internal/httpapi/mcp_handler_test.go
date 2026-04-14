package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/config"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

type fakeMCPDispatcher struct {
	dispatchEcho func(ctx context.Context, message string, timeout time.Duration) (string, error)
	submitTask   func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error)
	getTask      func(taskID string, ownerID string) (grpcserver.TaskSnapshot, bool)
	cancelTask   func(taskID string, ownerID string) (grpcserver.TaskSnapshot, error)
}

type fakeExportStore struct {
	presignUpload   func(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error)
	presignDownload func(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error)
}

func (f *fakeMCPDispatcher) DispatchEcho(ctx context.Context, message string, timeout time.Duration) (string, error) {
	if f.dispatchEcho != nil {
		return f.dispatchEcho(ctx, message, timeout)
	}
	return message, nil
}

func (f *fakeMCPDispatcher) SubmitTask(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
	if f.submitTask != nil {
		return f.submitTask(ctx, req)
	}
	return grpcserver.SubmitTaskResult{}, grpcserver.ErrNoCapabilityWorker
}

func (f *fakeMCPDispatcher) GetTask(taskID string, ownerID string) (grpcserver.TaskSnapshot, bool) {
	if f.getTask != nil {
		return f.getTask(taskID, ownerID)
	}
	return grpcserver.TaskSnapshot{}, false
}

func (f *fakeMCPDispatcher) CancelTask(taskID string, ownerID string) (grpcserver.TaskSnapshot, error) {
	if f.cancelTask != nil {
		return f.cancelTask(taskID, ownerID)
	}
	return grpcserver.TaskSnapshot{}, grpcserver.ErrTaskNotFound
}

func (f *fakeExportStore) PresignUpload(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error) {
	if f.presignUpload != nil {
		return f.presignUpload(ctx, objectKey, expiresIn)
	}
	return "https://uploads.example.com/" + objectKey, nil
}

func (f *fakeExportStore) PresignDownload(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error) {
	if f.presignDownload != nil {
		return f.presignDownload(ctx, objectKey, expiresIn)
	}
	return "https://downloads.example.com/" + objectKey, nil
}

func TestMCPInitialize(t *testing.T) {
	router := newMCPTestRouter(t, &fakeMCPDispatcher{})
	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`)

	result := mustMapField(t, payload, "result")
	serverInfo := mustMapField(t, result, "serverInfo")
	if got := asString(t, serverInfo["name"]); got != mcpServerName {
		t.Fatalf("expected serverInfo.name=%q, got %q", mcpServerName, got)
	}
	if got := asString(t, serverInfo["version"]); got != mcpServerVersion {
		t.Fatalf("expected serverInfo.version=%q, got %q", mcpServerVersion, got)
	}
	if asString(t, result["protocolVersion"]) == "" {
		t.Fatalf("expected protocolVersion in initialize result")
	}
	capabilities := mustObject(t, result["capabilities"], "initialize.capabilities")
	if _, ok := capabilities["resources"]; ok {
		t.Fatalf("did not expect resources capability in initialize response")
	}
}

func TestMCPToolsList(t *testing.T) {
	router := newMCPTestRouter(t, &fakeMCPDispatcher{})
	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)

	result := mustMapField(t, payload, "result")
	toolsRaw, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("expected tools array, got %#v", result["tools"])
	}
	if len(toolsRaw) != 5 {
		t.Fatalf("expected exactly 5 tools, got %d", len(toolsRaw))
	}

	toolByName := map[string]map[string]any{}
	for _, toolRaw := range toolsRaw {
		tool, ok := toolRaw.(map[string]any)
		if !ok {
			t.Fatalf("expected tool object, got %#v", toolRaw)
		}
		toolByName[asString(t, tool["name"])] = tool
	}
	if _, ok := toolByName["echo"]; !ok {
		t.Fatalf("expected tool echo in tools/list")
	}
	if _, ok := toolByName["pythonExec"]; !ok {
		t.Fatalf("expected tool pythonExec in tools/list")
	}
	if _, ok := toolByName["terminalExec"]; !ok {
		t.Fatalf("expected tool terminalExec in tools/list")
	}
	if _, ok := toolByName["computerUse"]; !ok {
		t.Fatalf("expected tool computerUse in tools/list")
	}
	if _, ok := toolByName["readImage"]; !ok {
		t.Fatalf("expected tool readImage in tools/list")
	}

	echoTool := toolByName["echo"]
	if got := asString(t, echoTool["title"]); got != mcpEchoToolTitle {
		t.Fatalf("expected echo title %q, got %q", mcpEchoToolTitle, got)
	}
	if got := asString(t, echoTool["description"]); got != mcpEchoToolDescription {
		t.Fatalf("unexpected echo description: %q", got)
	}
	echoAnnotations := mustObject(t, echoTool["annotations"], "echo.annotations")
	if !asBool(echoAnnotations["readOnlyHint"]) {
		t.Fatalf("expected echo.annotations.readOnlyHint=true")
	}
	if !asBool(echoAnnotations["idempotentHint"]) {
		t.Fatalf("expected echo.annotations.idempotentHint=true")
	}
	if asBool(echoAnnotations["destructiveHint"]) {
		t.Fatalf("expected echo.annotations.destructiveHint=false")
	}
	if asBool(echoAnnotations["openWorldHint"]) {
		t.Fatalf("expected echo.annotations.openWorldHint=false")
	}

	echoInputSchema := mustObject(t, echoTool["inputSchema"], "echo.inputSchema")
	if got := asString(t, echoInputSchema["type"]); got != "object" {
		t.Fatalf("expected echo.inputSchema.type=object, got %q", got)
	}
	if asBool(echoInputSchema["additionalProperties"]) {
		t.Fatalf("expected echo.inputSchema.additionalProperties=false")
	}
	assertRequiredContains(t, echoInputSchema["required"], "message")
	echoInputProperties := mustObject(t, echoInputSchema["properties"], "echo.inputSchema.properties")
	echoMessageSchema := mustObject(t, echoInputProperties["message"], "echo.inputSchema.properties.message")
	if got := asString(t, echoMessageSchema["type"]); got != "string" {
		t.Fatalf("expected echo.message.type=string, got %q", got)
	}
	if got := asString(t, echoMessageSchema["description"]); !strings.Contains(got, "whitespace-only") {
		t.Fatalf("expected echo.message description to mention whitespace handling, got %q", got)
	}
	echoTimeoutSchema := mustObject(t, echoInputProperties["timeout_ms"], "echo.inputSchema.properties.timeout_ms")
	if got := asString(t, echoTimeoutSchema["type"]); got != "integer" {
		t.Fatalf("expected echo.timeout_ms.type=integer, got %q", got)
	}
	if got := asInt(t, echoTimeoutSchema["minimum"]); got != minEchoTimeoutMS {
		t.Fatalf("expected echo.timeout_ms.minimum=%d, got %d", minEchoTimeoutMS, got)
	}
	if got := asInt(t, echoTimeoutSchema["maximum"]); got != maxEchoTimeoutMS {
		t.Fatalf("expected echo.timeout_ms.maximum=%d, got %d", maxEchoTimeoutMS, got)
	}
	if got := asInt(t, echoTimeoutSchema["default"]); got != defaultMCPEchoTimeoutMS {
		t.Fatalf("expected echo.timeout_ms.default=%d, got %d", defaultMCPEchoTimeoutMS, got)
	}

	echoOutputSchema := mustObject(t, echoTool["outputSchema"], "echo.outputSchema")
	if got := asString(t, echoOutputSchema["type"]); got != "object" {
		t.Fatalf("expected echo.outputSchema.type=object, got %q", got)
	}
	if asBool(echoOutputSchema["additionalProperties"]) {
		t.Fatalf("expected echo.outputSchema.additionalProperties=false")
	}
	assertRequiredContains(t, echoOutputSchema["required"], "message")
	echoOutputProperties := mustObject(t, echoOutputSchema["properties"], "echo.outputSchema.properties")
	echoOutputMessage := mustObject(t, echoOutputProperties["message"], "echo.outputSchema.properties.message")
	if got := asString(t, echoOutputMessage["type"]); got != "string" {
		t.Fatalf("expected echo.output.message.type=string, got %q", got)
	}

	pythonTool := toolByName["pythonExec"]
	if got := asString(t, pythonTool["title"]); got != mcpPythonExecToolTitle {
		t.Fatalf("expected pythonExec title %q, got %q", mcpPythonExecToolTitle, got)
	}
	if got := asString(t, pythonTool["description"]); got != mcpPythonExecToolDescription {
		t.Fatalf("unexpected pythonExec description: %q", got)
	}
	pythonAnnotations := mustObject(t, pythonTool["annotations"], "pythonExec.annotations")
	if !asBool(pythonAnnotations["destructiveHint"]) {
		t.Fatalf("expected pythonExec.annotations.destructiveHint=true")
	}
	if !asBool(pythonAnnotations["openWorldHint"]) {
		t.Fatalf("expected pythonExec.annotations.openWorldHint=true")
	}
	if _, exists := pythonAnnotations["readOnlyHint"]; exists {
		t.Fatalf("expected pythonExec.annotations.readOnlyHint to be omitted when false")
	}
	if _, exists := pythonAnnotations["idempotentHint"]; exists {
		t.Fatalf("expected pythonExec.annotations.idempotentHint to be omitted when false")
	}

	pythonInputSchema := mustObject(t, pythonTool["inputSchema"], "pythonExec.inputSchema")
	if got := asString(t, pythonInputSchema["type"]); got != "object" {
		t.Fatalf("expected pythonExec.inputSchema.type=object, got %q", got)
	}
	if asBool(pythonInputSchema["additionalProperties"]) {
		t.Fatalf("expected pythonExec.inputSchema.additionalProperties=false")
	}
	assertRequiredContains(t, pythonInputSchema["required"], "code")
	pythonInputProperties := mustObject(t, pythonInputSchema["properties"], "pythonExec.inputSchema.properties")
	pythonCodeSchema := mustObject(t, pythonInputProperties["code"], "pythonExec.inputSchema.properties.code")
	if got := asString(t, pythonCodeSchema["type"]); got != "string" {
		t.Fatalf("expected pythonExec.code.type=string, got %q", got)
	}
	pythonTimeoutSchema := mustObject(t, pythonInputProperties["timeout_ms"], "pythonExec.inputSchema.properties.timeout_ms")
	if got := asString(t, pythonTimeoutSchema["type"]); got != "integer" {
		t.Fatalf("expected pythonExec.timeout_ms.type=integer, got %q", got)
	}
	if got := asInt(t, pythonTimeoutSchema["minimum"]); got != minMCPTaskTimeoutMS {
		t.Fatalf("expected pythonExec.timeout_ms.minimum=%d, got %d", minMCPTaskTimeoutMS, got)
	}
	if got := asInt(t, pythonTimeoutSchema["maximum"]); got != maxMCPTaskTimeoutMS {
		t.Fatalf("expected pythonExec.timeout_ms.maximum=%d, got %d", maxMCPTaskTimeoutMS, got)
	}
	if got := asInt(t, pythonTimeoutSchema["default"]); got != defaultMCPTaskTimeoutMS {
		t.Fatalf("expected pythonExec.timeout_ms.default=%d, got %d", defaultMCPTaskTimeoutMS, got)
	}

	pythonOutputSchema := mustObject(t, pythonTool["outputSchema"], "pythonExec.outputSchema")
	if got := asString(t, pythonOutputSchema["type"]); got != "object" {
		t.Fatalf("expected pythonExec.outputSchema.type=object, got %q", got)
	}
	if asBool(pythonOutputSchema["additionalProperties"]) {
		t.Fatalf("expected pythonExec.outputSchema.additionalProperties=false")
	}
	assertRequiredContains(t, pythonOutputSchema["required"], "output")
	assertRequiredContains(t, pythonOutputSchema["required"], "stderr")
	assertRequiredContains(t, pythonOutputSchema["required"], "exit_code")
	pythonOutputProperties := mustObject(t, pythonOutputSchema["properties"], "pythonExec.outputSchema.properties")
	pythonExitCodeSchema := mustObject(t, pythonOutputProperties["exit_code"], "pythonExec.outputSchema.properties.exit_code")
	if got := asString(t, pythonExitCodeSchema["type"]); got != "integer" {
		t.Fatalf("expected pythonExec.exit_code.type=integer, got %q", got)
	}

	terminalTool := toolByName["terminalExec"]
	if got := asString(t, terminalTool["title"]); got != mcpTerminalExecToolTitle {
		t.Fatalf("expected terminalExec title %q, got %q", mcpTerminalExecToolTitle, got)
	}
	if got := asString(t, terminalTool["description"]); got != mcpTerminalExecToolDescription {
		t.Fatalf("unexpected terminalExec description: %q", got)
	}
	terminalInputSchema := mustObject(t, terminalTool["inputSchema"], "terminalExec.inputSchema")
	if got := asString(t, terminalInputSchema["type"]); got != "object" {
		t.Fatalf("expected terminalExec.inputSchema.type=object, got %q", got)
	}
	assertRequiredContains(t, terminalInputSchema["required"], "command")
	terminalInputProperties := mustObject(t, terminalInputSchema["properties"], "terminalExec.inputSchema.properties")
	terminalCommandSchema := mustObject(t, terminalInputProperties["command"], "terminalExec.inputSchema.properties.command")
	if got := asString(t, terminalCommandSchema["type"]); got != "string" {
		t.Fatalf("expected terminalExec.command.type=string, got %q", got)
	}
	terminalOutputSchema := mustObject(t, terminalTool["outputSchema"], "terminalExec.outputSchema")
	if got := asString(t, terminalOutputSchema["type"]); got != "object" {
		t.Fatalf("expected terminalExec.outputSchema.type=object, got %q", got)
	}
	terminalOutputProperties := mustObject(t, terminalOutputSchema["properties"], "terminalExec.outputSchema.properties")
	leaseSchema := mustObject(t, terminalOutputProperties["lease_expires_unix_ms"], "terminalExec.outputSchema.properties.lease_expires_unix_ms")
	if got := asString(t, leaseSchema["type"]); got != "integer" {
		t.Fatalf("expected terminalExec.lease_expires_unix_ms.type=integer, got %q", got)
	}

	computerUseTool := toolByName["computerUse"]
	if got := asString(t, computerUseTool["title"]); got != mcpComputerUseToolTitle {
		t.Fatalf("expected computerUse title %q, got %q", mcpComputerUseToolTitle, got)
	}
	if got := asString(t, computerUseTool["description"]); got != mcpComputerUseToolDescription {
		t.Fatalf("unexpected computerUse description: %q", got)
	}
	computerUseInputSchema := mustObject(t, computerUseTool["inputSchema"], "computerUse.inputSchema")
	assertRequiredContains(t, computerUseInputSchema["required"], "command")
	computerUseInputProperties := mustObject(t, computerUseInputSchema["properties"], "computerUse.inputSchema.properties")
	if _, ok := computerUseInputProperties["lease_ttl_sec"]; ok {
		t.Fatalf("did not expect computerUse.inputSchema.properties.lease_ttl_sec")
	}
	computerUseOutputSchema := mustObject(t, computerUseTool["outputSchema"], "computerUse.outputSchema")
	computerUseOutputProperties := mustObject(t, computerUseOutputSchema["properties"], "computerUse.outputSchema.properties")
	if _, ok := computerUseOutputProperties["lease_expires_unix_ms"]; ok {
		t.Fatalf("did not expect computerUse.outputSchema.properties.lease_expires_unix_ms")
	}
	computerUseExitCode := mustObject(t, computerUseOutputProperties["exit_code"], "computerUse.outputSchema.properties.exit_code")
	if got := asString(t, computerUseExitCode["type"]); got != "integer" {
		t.Fatalf("expected computerUse.exit_code.type=integer, got %q", got)
	}

	readImageTool := toolByName["readImage"]
	if got := asString(t, readImageTool["title"]); got != mcpReadImageToolTitle {
		t.Fatalf("expected readImage title %q, got %q", mcpReadImageToolTitle, got)
	}
	if got := asString(t, readImageTool["description"]); got != mcpReadImageToolDescription {
		t.Fatalf("unexpected readImage description: %q", got)
	}
	readImageAnnotations := mustObject(t, readImageTool["annotations"], "readImage.annotations")
	if !asBool(readImageAnnotations["readOnlyHint"]) {
		t.Fatalf("expected readImage.annotations.readOnlyHint=true")
	}
	if !asBool(readImageAnnotations["idempotentHint"]) {
		t.Fatalf("expected readImage.annotations.idempotentHint=true")
	}
	if asBool(readImageAnnotations["destructiveHint"]) {
		t.Fatalf("expected readImage.annotations.destructiveHint=false")
	}
	readImageInputSchema := mustObject(t, readImageTool["inputSchema"], "readImage.inputSchema")
	assertRequiredContains(t, readImageInputSchema["required"], "session_id")
	assertRequiredContains(t, readImageInputSchema["required"], "file_path")
	if _, ok := readImageTool["outputSchema"]; ok {
		t.Fatalf("did not expect readImage.outputSchema")
	}
}

func TestMCPToolsListWithHiddenTools(t *testing.T) {
	hidden := map[string]bool{"echo": true, "computeruse": true, "readimage": true}
	router := newMCPTestRouterWithHiddenTools(t, &fakeMCPDispatcher{}, hidden)
	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)

	result := mustMapField(t, payload, "result")
	toolsRaw, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("expected tools array, got %#v", result["tools"])
	}
	if len(toolsRaw) != 2 {
		t.Fatalf("expected exactly 2 tools (5 - 3 hidden), got %d", len(toolsRaw))
	}

	toolByName := map[string]bool{}
	for _, toolRaw := range toolsRaw {
		tool, ok := toolRaw.(map[string]any)
		if !ok {
			t.Fatalf("expected tool object, got %#v", toolRaw)
		}
		toolByName[asString(t, tool["name"])] = true
	}
	if toolByName["echo"] {
		t.Fatalf("expected echo to be excluded from tools/list when hidden")
	}
	if toolByName["computerUse"] {
		t.Fatalf("expected computerUse to be excluded from tools/list when hidden")
	}
	if toolByName["readImage"] {
		t.Fatalf("expected readImage to be excluded from tools/list when hidden")
	}
	if !toolByName["pythonExec"] {
		t.Fatalf("expected pythonExec in tools/list")
	}
	if !toolByName["terminalExec"] {
		t.Fatalf("expected terminalExec in tools/list")
	}
}

func TestMCPToolsListWithExportFile(t *testing.T) {
	router := newMCPTestRouterWithObjectStore(t, &fakeMCPDispatcher{}, &fakeExportStore{}, "exports")
	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)

	result := mustMapField(t, payload, "result")
	toolsRaw, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("expected tools array, got %#v", result["tools"])
	}
	if len(toolsRaw) != 6 {
		t.Fatalf("expected exactly 6 tools, got %d", len(toolsRaw))
	}

	toolByName := map[string]map[string]any{}
	for _, toolRaw := range toolsRaw {
		tool, ok := toolRaw.(map[string]any)
		if !ok {
			t.Fatalf("expected tool object, got %#v", toolRaw)
		}
		toolByName[asString(t, tool["name"])] = tool
	}

	exportTool := toolByName[exportFileToolName]
	if got := asString(t, exportTool["title"]); got != mcpExportFileToolTitle {
		t.Fatalf("expected exportFile title %q, got %q", mcpExportFileToolTitle, got)
	}
	if got := asString(t, exportTool["description"]); got != mcpExportFileToolDescription {
		t.Fatalf("unexpected exportFile description: %q", got)
	}
	exportInputSchema := mustObject(t, exportTool["inputSchema"], "exportFile.inputSchema")
	assertRequiredContains(t, exportInputSchema["required"], "session_id")
	assertRequiredContains(t, exportInputSchema["required"], "file_path")
	exportOutputSchema := mustObject(t, exportTool["outputSchema"], "exportFile.outputSchema")
	assertRequiredContains(t, exportOutputSchema["required"], "signed_url")
	assertRequiredContains(t, exportOutputSchema["required"], "object_key")
	assertRequiredContains(t, exportOutputSchema["required"], "filename")
}

func TestMCPToolsList_WithDescriptionOverride(t *testing.T) {
	custom := "custom echo description"
	customTitle := "Custom Echo"
	overrides := map[string]config.MCPToolOverride{
		"echo": {
			Title:       &customTitle,
			Description: &custom,
		},
	}
	router := newMCPTestRouterWithOverrides(t, &fakeMCPDispatcher{}, overrides, nil)
	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	result := mustMapField(t, payload, "result")
	toolsRaw, _ := result["tools"].([]any)
	var echoTool map[string]any
	for _, t := range toolsRaw {
		m, _ := t.(map[string]any)
		if asString := func() string {
			s, _ := m["name"].(string)
			return s
		}(); asString == "echo" {
			echoTool = m
		}
	}
	if echoTool == nil {
		t.Fatalf("echo tool not found")
	}
	if got, _ := echoTool["description"].(string); got != custom {
		t.Fatalf("expected description=%q, got %q", custom, got)
	}
	if got, _ := echoTool["title"].(string); got != customTitle {
		t.Fatalf("expected title=%q, got %q", customTitle, got)
	}
}

func TestMCPToolsList_EmptyDescriptionOverride_FallsBack(t *testing.T) {
	empty := ""
	overrides := map[string]config.MCPToolOverride{
		"echo": {Description: &empty, Title: &empty},
	}
	logger, cap := newCaptureLogger()
	router := newMCPTestRouterWithOverrides(t, &fakeMCPDispatcher{}, overrides, logger)
	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	result := mustMapField(t, payload, "result")
	toolsRaw, _ := result["tools"].([]any)
	var echoTool map[string]any
	for _, tl := range toolsRaw {
		m, _ := tl.(map[string]any)
		if n, _ := m["name"].(string); n == "echo" {
			echoTool = m
		}
	}
	if got, _ := echoTool["description"].(string); got != mcpEchoToolDescription {
		t.Fatalf("expected fallback to default description, got %q", got)
	}
	if got, _ := echoTool["title"].(string); got != mcpEchoToolTitle {
		t.Fatalf("expected fallback to default title, got %q", got)
	}
	logs := cap.String()
	if !strings.Contains(logs, "empty MCP tool description override") {
		t.Fatalf("expected warn about empty description; logs=%q", logs)
	}
	if !strings.Contains(logs, "empty MCP tool title override") {
		t.Fatalf("expected warn about empty title; logs=%q", logs)
	}
}

func TestMCPToolsList_WithParamDescriptionOverride(t *testing.T) {
	customDesc := "overridden message description"
	overrides := map[string]config.MCPToolOverride{
		"echo": {ParamDescriptions: map[string]*string{"message": &customDesc}},
	}
	router := newMCPTestRouterWithOverrides(t, &fakeMCPDispatcher{}, overrides, nil)
	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	result := mustMapField(t, payload, "result")
	toolsRaw, _ := result["tools"].([]any)
	var echoTool map[string]any
	for _, tl := range toolsRaw {
		m, _ := tl.(map[string]any)
		if n, _ := m["name"].(string); n == "echo" {
			echoTool = m
		}
	}
	schema := mustObject(t, echoTool["inputSchema"], "echo.inputSchema")
	if asBool(schema["additionalProperties"]) {
		t.Fatalf("additionalProperties should still be false when only overriding description")
	}
	props := mustObject(t, schema["properties"], "echo.inputSchema.properties")
	msg := mustObject(t, props["message"], "echo.inputSchema.properties.message")
	if got := asString(t, msg["description"]); got != customDesc {
		t.Fatalf("expected message description=%q, got %q", customDesc, got)
	}
	// ensure type/other fields preserved
	if got := asString(t, msg["type"]); got != "string" {
		t.Fatalf("expected message.type=string, got %q", got)
	}
}

func TestMCPToolsList_WithParamHidden(t *testing.T) {
	empty := ""
	overrides := map[string]config.MCPToolOverride{
		"terminalExec": {ParamDescriptions: map[string]*string{"session_id": &empty, "timeout_ms": &empty}},
	}
	router := newMCPTestRouterWithOverrides(t, &fakeMCPDispatcher{}, overrides, nil)
	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	result := mustMapField(t, payload, "result")
	toolsRaw, _ := result["tools"].([]any)
	var termTool map[string]any
	for _, tl := range toolsRaw {
		m, _ := tl.(map[string]any)
		if n, _ := m["name"].(string); n == "terminalExec" {
			termTool = m
		}
	}
	schema := mustObject(t, termTool["inputSchema"], "terminalExec.inputSchema")
	if !asBool(schema["additionalProperties"]) {
		t.Fatalf("expected additionalProperties=true after hiding params")
	}
	props := mustObject(t, schema["properties"], "terminalExec.inputSchema.properties")
	if _, has := props["session_id"]; has {
		t.Fatalf("session_id should be hidden from properties")
	}
	if _, has := props["timeout_ms"]; has {
		t.Fatalf("timeout_ms should be hidden from properties")
	}
	// command still required
	assertRequiredContains(t, schema["required"], "command")
}

func TestMCPToolCall_HiddenParamStillAccepted(t *testing.T) {
	empty := ""
	overrides := map[string]config.MCPToolOverride{
		"echo": {ParamDescriptions: map[string]*string{"timeout_ms": &empty}},
	}
	gotTimeout := time.Duration(0)
	dispatcher := &fakeMCPDispatcher{
		dispatchEcho: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			gotTimeout = timeout
			return message, nil
		},
	}
	router := newMCPTestRouterWithOverrides(t, dispatcher, overrides, nil)
	// Still pass timeout_ms even though hidden. Schema must not reject it.
	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"message":"hi","timeout_ms":12345}}}`)
	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected success, got %s", mustJSON(t, result))
	}
	if gotTimeout != 12345*time.Millisecond {
		t.Fatalf("expected dispatcher to receive 12345ms timeout, got %s", gotTimeout)
	}
}

func TestMCPToolCall_HiddenRequiredParamWarns(t *testing.T) {
	empty := ""
	overrides := map[string]config.MCPToolOverride{
		"pythonExec": {ParamDescriptions: map[string]*string{"code": &empty}},
	}
	logger, cap := newCaptureLogger()
	_ = newMCPTestRouterWithOverrides(t, &fakeMCPDispatcher{}, overrides, logger)
	logs := cap.String()
	if !strings.Contains(logs, "hiding MCP tool parameter") {
		t.Fatalf("expected warn about hidden param; logs=%q", logs)
	}
	if !strings.Contains(logs, "pythonExec") || !strings.Contains(logs, "code") {
		t.Fatalf("expected warn to mention pythonExec/code; logs=%q", logs)
	}
	if !strings.Contains(logs, "required=true") {
		t.Fatalf("expected warn to flag required=true; logs=%q", logs)
	}
}

func TestMCPToolCall_HiddenOptionalParamAlsoWarns(t *testing.T) {
	empty := ""
	overrides := map[string]config.MCPToolOverride{
		"echo": {ParamDescriptions: map[string]*string{"timeout_ms": &empty}},
	}
	logger, cap := newCaptureLogger()
	_ = newMCPTestRouterWithOverrides(t, &fakeMCPDispatcher{}, overrides, logger)
	logs := cap.String()
	if !strings.Contains(logs, "hiding MCP tool parameter") {
		t.Fatalf("expected warn about hidden optional param; logs=%q", logs)
	}
	if !strings.Contains(logs, "echo") || !strings.Contains(logs, "timeout_ms") {
		t.Fatalf("expected warn to mention echo/timeout_ms; logs=%q", logs)
	}
	if !strings.Contains(logs, "required=false") {
		t.Fatalf("expected warn to flag required=false; logs=%q", logs)
	}
}

func TestMCPToolCallHiddenToolStillWorks(t *testing.T) {
	hidden := map[string]bool{"echo": true, "pythonexec": true}
	router := newMCPTestRouterWithHiddenTools(t, &fakeMCPDispatcher{
		dispatchEcho: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			return "hidden:" + message, nil
		},
	}, hidden)

	echoPayload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"message":"hello"}}}`)
	result := mustMapField(t, echoPayload, "result")
	contentRaw, ok := result["content"].([]any)
	if !ok || len(contentRaw) == 0 {
		t.Fatalf("expected content array for hidden echo tool, got %#v", result["content"])
	}
	first := mustObject(t, contentRaw[0], "echo.content[0]")
	if got := asString(t, first["text"]); got != `{"message":"hidden:hello"}` {
		t.Fatalf("unexpected hidden echo response: %q", got)
	}
}

func TestMCPToolCallEchoSuccess(t *testing.T) {
	router := newMCPTestRouter(t, &fakeMCPDispatcher{
		dispatchEcho: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			if timeout != 5*time.Second {
				t.Fatalf("expected default timeout 5s, got %s", timeout)
			}
			return message, nil
		},
	})
	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"message":"hello mcp"}}}`)

	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected tool call success, got error payload=%s", mustJSON(t, result))
	}
	structured := mustMapField(t, result, "structuredContent")
	if got := asString(t, structured["message"]); got != "hello mcp" {
		t.Fatalf("expected message=hello mcp, got %q", got)
	}
}

func TestMCPToolCallPythonExecSuccess(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	router := newMCPTestRouter(t, &fakeMCPDispatcher{
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			if req.Capability != pythonExecCapabilityName {
				t.Fatalf("expected capability=%q, got %q", pythonExecCapabilityName, req.Capability)
			}
			if req.OwnerID != testDashboardAccountID {
				t.Fatalf("expected owner_id from token, got %q", req.OwnerID)
			}
			var payload pythonExecPayload
			if err := json.Unmarshal(req.InputJSON, &payload); err != nil {
				t.Fatalf("expected valid pythonExec input json, got %s", string(req.InputJSON))
			}
			if payload.Code != "print('ok')" {
				t.Fatalf("unexpected code payload: %q", payload.Code)
			}
			if req.Mode != grpcserver.TaskModeSync {
				t.Fatalf("expected sync mode, got %q", req.Mode)
			}
			if req.Timeout != 60*time.Second {
				t.Fatalf("expected default timeout 60s, got %s", req.Timeout)
			}
			return grpcserver.SubmitTaskResult{
				Task: grpcserver.TaskSnapshot{
					TaskID:     "task-1",
					Capability: pythonExecCapabilityName,
					Status:     grpcserver.TaskStatusSucceeded,
					ResultJSON: []byte(`{"output":"ok\n","stderr":"","exit_code":7}`),
					CreatedAt:  now,
					UpdatedAt:  now,
					DeadlineAt: now.Add(60 * time.Second),
				},
				Completed: true,
			}, nil
		},
	})

	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pythonExec","arguments":{"code":"print('ok')"}}}`)
	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected tool call success, got error payload=%s", mustJSON(t, result))
	}
	structured := mustMapField(t, result, "structuredContent")
	if got := asString(t, structured["output"]); got != "ok\n" {
		t.Fatalf("expected output=ok\\n, got %q", got)
	}
	if got := asString(t, structured["stderr"]); got != "" {
		t.Fatalf("expected empty stderr, got %q", got)
	}
	if got := asInt(t, structured["exit_code"]); got != 7 {
		t.Fatalf("expected exit_code=7, got %d", got)
	}
}

func TestMCPToolCallTerminalExecSuccess(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	router := newMCPTestRouter(t, &fakeMCPDispatcher{
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			if req.Capability != terminalExecCapabilityName {
				t.Fatalf("expected capability=%q, got %q", terminalExecCapabilityName, req.Capability)
			}
			if req.OwnerID != testDashboardAccountID {
				t.Fatalf("expected owner_id from token, got %q", req.OwnerID)
			}

			payload := terminalExecPayload{}
			if err := json.Unmarshal(req.InputJSON, &payload); err != nil {
				t.Fatalf("expected valid terminalExec input json, got %s", string(req.InputJSON))
			}
			if payload.Command != "pwd" {
				t.Fatalf("unexpected command payload: %#v", payload)
			}

			resultJSON, _ := json.Marshal(mcpTerminalExecToolOutput{
				SessionID:          "session-1",
				Created:            true,
				Stdout:             "/workspace\n",
				Stderr:             "",
				ExitCode:           0,
				StdoutTruncated:    false,
				StderrTruncated:    false,
				LeaseExpiresUnixMS: 12345,
			})
			return grpcserver.SubmitTaskResult{
				Task: grpcserver.TaskSnapshot{
					TaskID:     "task-term-1",
					Capability: terminalExecCapabilityName,
					Status:     grpcserver.TaskStatusSucceeded,
					ResultJSON: resultJSON,
					CreatedAt:  now,
					UpdatedAt:  now,
					DeadlineAt: now.Add(60 * time.Second),
				},
				Completed: true,
			}, nil
		},
	})

	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"terminalExec","arguments":{"command":"pwd","create_if_missing":true}}}`)
	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected tool call success, got error payload=%s", mustJSON(t, result))
	}
	structured := mustMapField(t, result, "structuredContent")
	if got := asString(t, structured["session_id"]); got != "session-1" {
		t.Fatalf("expected session_id=session-1, got %q", got)
	}
}

func TestMCPToolCallComputerUseSuccess(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	router := newMCPTestRouter(t, &fakeMCPDispatcher{
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			if req.Capability != computerUseCapabilityName {
				t.Fatalf("expected capability=%q, got %q", computerUseCapabilityName, req.Capability)
			}
			if req.OwnerID != testDashboardAccountID {
				t.Fatalf("expected owner_id from token, got %q", req.OwnerID)
			}
			payload := computerUsePayload{}
			if err := json.Unmarshal(req.InputJSON, &payload); err != nil {
				t.Fatalf("expected valid computerUse payload, got %s", string(req.InputJSON))
			}
			if payload.Command != "pwd" {
				t.Fatalf("unexpected command payload: %#v", payload)
			}
			resultJSON, _ := json.Marshal(mcpComputerUseToolOutput{
				Stdout:          "/workspace\n",
				Stderr:          "",
				ExitCode:        0,
				StdoutTruncated: false,
				StderrTruncated: false,
			})
			return grpcserver.SubmitTaskResult{
				Task: grpcserver.TaskSnapshot{
					TaskID:     "task-cu-1",
					Capability: computerUseCapabilityName,
					Status:     grpcserver.TaskStatusSucceeded,
					ResultJSON: resultJSON,
					CreatedAt:  now,
					UpdatedAt:  now,
					DeadlineAt: now.Add(60 * time.Second),
				},
				Completed: true,
			}, nil
		},
	})

	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"computerUse","arguments":{"command":"pwd","request_id":"req-1"}}}`)
	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected tool call success, got error payload=%s", mustJSON(t, result))
	}
	structured := mustMapField(t, result, "structuredContent")
	if got := asString(t, structured["stdout"]); got != "/workspace\n" {
		t.Fatalf("expected stdout=/workspace\\n, got %q", got)
	}
	if got := asInt(t, structured["exit_code"]); got != 0 {
		t.Fatalf("expected exit_code=0, got %d", got)
	}
}

func TestMCPToolCallReadImageSuccess(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	router := newMCPTestRouter(t, &fakeMCPDispatcher{
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			if req.Capability != terminalResourceCapabilityName {
				t.Fatalf("expected capability=%q, got %q", terminalResourceCapabilityName, req.Capability)
			}
			if req.OwnerID != testDashboardAccountID {
				t.Fatalf("expected owner_id from token, got %q", req.OwnerID)
			}
			payload := mcpTerminalResourcePayload{}
			if err := json.Unmarshal(req.InputJSON, &payload); err != nil {
				t.Fatalf("expected valid terminalResource payload, got %s", string(req.InputJSON))
			}
			switch payload.Action {
			case "validate":
				resultJSON, _ := json.Marshal(mcpTerminalResourceResult{
					SessionID: payload.SessionID,
					FilePath:  payload.FilePath,
					MIMEType:  "image/png",
					SizeBytes: 4,
				})
				return grpcserver.SubmitTaskResult{
					Task: grpcserver.TaskSnapshot{
						TaskID:     "task-read-image-validate",
						Capability: terminalResourceCapabilityName,
						Status:     grpcserver.TaskStatusSucceeded,
						ResultJSON: resultJSON,
						CreatedAt:  now,
						UpdatedAt:  now,
						DeadlineAt: now.Add(60 * time.Second),
					},
					Completed: true,
				}, nil
			case "read":
				resultJSON, _ := json.Marshal(mcpTerminalResourceResult{
					SessionID: payload.SessionID,
					FilePath:  payload.FilePath,
					MIMEType:  "image/png",
					SizeBytes: 4,
					Blob:      []byte{0x89, 0x50, 0x4e, 0x47},
				})
				return grpcserver.SubmitTaskResult{
					Task: grpcserver.TaskSnapshot{
						TaskID:     "task-read-image-read",
						Capability: terminalResourceCapabilityName,
						Status:     grpcserver.TaskStatusSucceeded,
						ResultJSON: resultJSON,
						CreatedAt:  now,
						UpdatedAt:  now,
						DeadlineAt: now.Add(60 * time.Second),
					},
					Completed: true,
				}, nil
			default:
				t.Fatalf("unexpected action: %q", payload.Action)
				return grpcserver.SubmitTaskResult{}, nil
			}
		},
	})

	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"readImage","arguments":{"session_id":"session-1","file_path":"/workspace/image.png"}}}`)
	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected tool call success, got error payload=%s", mustJSON(t, result))
	}
	if _, ok := result["structuredContent"]; ok {
		t.Fatalf("did not expect structuredContent in readImage result")
	}

	contentRaw, ok := result["content"].([]any)
	if !ok || len(contentRaw) != 1 {
		t.Fatalf("expected [image] content, got %s", mustJSON(t, result))
	}
	first := mustObject(t, contentRaw[0], "readImage.content[0]")
	if got := asString(t, first["type"]); got != "image" {
		t.Fatalf("expected content type image, got %q", got)
	}
	if got := asString(t, first["mimeType"]); got != "image/png" {
		t.Fatalf("expected image mimeType=image/png, got %q", got)
	}
	if got := asString(t, first["data"]); got == "" {
		t.Fatalf("expected inline image data")
	}
}

func TestMCPToolCallExportFileSuccess(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	originalExportObjectID := newExportObjectID
	newExportObjectID = func() string { return "fixed-id" }
	t.Cleanup(func() {
		newExportObjectID = originalExportObjectID
	})

	store := &fakeExportStore{
		presignUpload: func(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error) {
			if objectKey != "exports/session_1/fixed-id-report.png" {
				t.Fatalf("unexpected upload object key: %q", objectKey)
			}
			if expiresIn != exportFileUploadPresignTTL {
				t.Fatalf("unexpected upload ttl: %s", expiresIn)
			}
			return "https://uploads.example.com/put", nil
		},
		presignDownload: func(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error) {
			if objectKey != "exports/session_1/fixed-id-report.png" {
				t.Fatalf("unexpected download object key: %q", objectKey)
			}
			if expiresIn != exportFileDownloadPresignTTL {
				t.Fatalf("unexpected download ttl: %s", expiresIn)
			}
			return "https://downloads.example.com/get", nil
		},
	}

	router := newMCPTestRouterWithObjectStore(t, &fakeMCPDispatcher{
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			if req.Capability != terminalResourceCapabilityName {
				t.Fatalf("expected capability=%q, got %q", terminalResourceCapabilityName, req.Capability)
			}
			payload := mcpTerminalResourcePayload{}
			if err := json.Unmarshal(req.InputJSON, &payload); err != nil {
				t.Fatalf("expected valid terminalResource payload, got %s", string(req.InputJSON))
			}
			if payload.Action != "export" {
				t.Fatalf("expected export action, got %q", payload.Action)
			}
			if payload.SignedURL != "https://uploads.example.com/put" {
				t.Fatalf("unexpected signed_url: %q", payload.SignedURL)
			}
			resultJSON, _ := json.Marshal(mcpTerminalResourceResult{
				SessionID: payload.SessionID,
				FilePath:  payload.FilePath,
				MIMEType:  "image/png",
				SizeBytes: 4,
			})
			return grpcserver.SubmitTaskResult{
				Task: grpcserver.TaskSnapshot{
					TaskID:     "task-export-file",
					Capability: terminalResourceCapabilityName,
					Status:     grpcserver.TaskStatusSucceeded,
					ResultJSON: resultJSON,
					CreatedAt:  now,
					UpdatedAt:  now,
					DeadlineAt: now.Add(60 * time.Second),
				},
				Completed: true,
			}, nil
		},
	}, store, "exports")

	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exportFile","arguments":{"session_id":"session/1","file_path":"/workspace/report.png"}}}`)
	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected tool call success, got error payload=%s", mustJSON(t, result))
	}
	structured := mustMapField(t, result, "structuredContent")
	if got := asString(t, structured["signed_url"]); got != "https://downloads.example.com/get" {
		t.Fatalf("expected signed_url in response, got %q", got)
	}
	if got := asString(t, structured["object_key"]); got != "exports/session_1/fixed-id-report.png" {
		t.Fatalf("expected object_key in response, got %q", got)
	}
	if got := asString(t, structured["filename"]); got != "report.png" {
		t.Fatalf("expected filename in response, got %q", got)
	}
}

func TestMCPToolCallExportFileUsesConfiguredPresignTTLs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	originalExportObjectID := newExportObjectID
	newExportObjectID = func() string { return "fixed-id" }
	t.Cleanup(func() {
		newExportObjectID = originalExportObjectID
	})

	customUploadTTL := 2 * time.Minute
	customDownloadTTL := 12 * time.Minute
	store := &fakeExportStore{
		presignUpload: func(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error) {
			if expiresIn != customUploadTTL {
				t.Fatalf("unexpected upload ttl: %s", expiresIn)
			}
			return "https://uploads.example.com/put", nil
		},
		presignDownload: func(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error) {
			if expiresIn != customDownloadTTL {
				t.Fatalf("unexpected download ttl: %s", expiresIn)
			}
			return "https://downloads.example.com/get", nil
		},
	}

	router := newMCPTestRouterWithObjectStoreAndTTLs(t, &fakeMCPDispatcher{
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			resultJSON, _ := json.Marshal(mcpTerminalResourceResult{
				SessionID: "session-1",
				FilePath:  "/workspace/report.png",
				MIMEType:  "image/png",
				SizeBytes: 4,
			})
			return grpcserver.SubmitTaskResult{
				Task: grpcserver.TaskSnapshot{
					TaskID:     "task-export-file-custom-ttl",
					Capability: terminalResourceCapabilityName,
					Status:     grpcserver.TaskStatusSucceeded,
					ResultJSON: resultJSON,
					CreatedAt:  now,
					UpdatedAt:  now,
					DeadlineAt: now.Add(60 * time.Second),
				},
				Completed: true,
			}, nil
		},
	}, store, "exports", customUploadTTL, customDownloadTTL, ExportReturnSchemaAll)

	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exportFile","arguments":{"session_id":"session-1","file_path":"/workspace/report.png"}}}`)
	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected tool call success, got error payload=%s", mustJSON(t, result))
	}
	structured := mustMapField(t, result, "structuredContent")
	if got := asString(t, structured["object_key"]); got != "exports/session-1/fixed-id-report.png" {
		t.Fatalf("expected object_key in response, got %q", got)
	}
	if got := asString(t, structured["filename"]); got != "report.png" {
		t.Fatalf("expected filename in response, got %q", got)
	}
}

func TestMCPToolCallExportFileComputerUseSessionRoutesToReadImageCapability(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	originalExportObjectID := newExportObjectID
	newExportObjectID = func() string { return "fixed-id" }
	t.Cleanup(func() {
		newExportObjectID = originalExportObjectID
	})

	store := &fakeExportStore{
		presignUpload: func(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error) {
			return "https://uploads.example.com/put", nil
		},
		presignDownload: func(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error) {
			return "https://downloads.example.com/get", nil
		},
	}

	router := newMCPTestRouterWithObjectStore(t, &fakeMCPDispatcher{
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			if req.Capability != readImageCapabilityName {
				t.Fatalf("expected capability=%q, got %q", readImageCapabilityName, req.Capability)
			}
			payload := mcpTerminalResourcePayload{}
			if err := json.Unmarshal(req.InputJSON, &payload); err != nil {
				t.Fatalf("expected valid readImage payload, got %s", string(req.InputJSON))
			}
			if payload.SessionID != computerUseSessionID {
				t.Fatalf("expected session_id=%q, got %q", computerUseSessionID, payload.SessionID)
			}
			if payload.Action != "export" {
				t.Fatalf("expected export action, got %q", payload.Action)
			}
			if payload.SignedURL != "https://uploads.example.com/put" {
				t.Fatalf("unexpected signed_url: %q", payload.SignedURL)
			}
			resultJSON, _ := json.Marshal(mcpTerminalResourceResult{
				SessionID: payload.SessionID,
				FilePath:  payload.FilePath,
				MIMEType:  "image/png",
				SizeBytes: 4,
			})
			return grpcserver.SubmitTaskResult{
				Task: grpcserver.TaskSnapshot{
					TaskID:     "task-export-file-computer-use",
					Capability: readImageCapabilityName,
					Status:     grpcserver.TaskStatusSucceeded,
					ResultJSON: resultJSON,
					CreatedAt:  now,
					UpdatedAt:  now,
					DeadlineAt: now.Add(60 * time.Second),
				},
				Completed: true,
			}, nil
		},
	}, store, "exports")

	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exportFile","arguments":{"session_id":"computerUse","file_path":"/workspace/report.png"}}}`)
	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected tool call success, got error payload=%s", mustJSON(t, result))
	}
	structured := mustMapField(t, result, "structuredContent")
	if got := asString(t, structured["signed_url"]); got != "https://downloads.example.com/get" {
		t.Fatalf("expected signed_url in response, got %q", got)
	}
	if got := asString(t, structured["object_key"]); got != "exports/computerUse/fixed-id-report.png" {
		t.Fatalf("expected object_key in response, got %q", got)
	}
	if got := asString(t, structured["filename"]); got != "report.png" {
		t.Fatalf("expected filename in response, got %q", got)
	}
}

func TestMCPToolCallExportFileSignedURLOnly(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	originalExportObjectID := newExportObjectID
	newExportObjectID = func() string { return "fixed-id" }
	t.Cleanup(func() {
		newExportObjectID = originalExportObjectID
	})

	store := &fakeExportStore{
		presignUpload: func(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error) {
			return "https://uploads.example.com/put", nil
		},
		presignDownload: func(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error) {
			return "https://downloads.example.com/get", nil
		},
	}

	router := newMCPTestRouterWithObjectStoreAndTTLs(t, &fakeMCPDispatcher{
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			resultJSON, _ := json.Marshal(mcpTerminalResourceResult{
				SessionID: "session-1",
				FilePath:  "/workspace/report.png",
				MIMEType:  "image/png",
				SizeBytes: 4,
			})
			return grpcserver.SubmitTaskResult{
				Task: grpcserver.TaskSnapshot{
					TaskID:     "task-export-signed",
					Capability: terminalResourceCapabilityName,
					Status:     grpcserver.TaskStatusSucceeded,
					ResultJSON: resultJSON,
					CreatedAt:  now,
					UpdatedAt:  now,
					DeadlineAt: now.Add(60 * time.Second),
				},
				Completed: true,
			}, nil
		},
	}, store, "exports", exportFileUploadPresignTTL, exportFileDownloadPresignTTL, ExportReturnSchemaSignedURL)

	// Verify output schema only declares signed_url
	listPayload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	listResult := mustMapField(t, listPayload, "result")
	toolsRaw := listResult["tools"].([]any)
	for _, toolRaw := range toolsRaw {
		tool := toolRaw.(map[string]any)
		if asString(t, tool["name"]) != exportFileToolName {
			continue
		}
		outputSchema := mustObject(t, tool["outputSchema"], "exportFile.outputSchema")
		assertRequiredContains(t, outputSchema["required"], "signed_url")
		assertRequiredNotContains(t, outputSchema["required"], "object_key")
		assertRequiredNotContains(t, outputSchema["required"], "filename")
	}

	// Verify tool call returns only signed_url
	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exportFile","arguments":{"session_id":"session-1","file_path":"/workspace/report.png"}}}`)
	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected tool call success, got error payload=%s", mustJSON(t, result))
	}
	structured := mustMapField(t, result, "structuredContent")
	if got := asString(t, structured["signed_url"]); got != "https://downloads.example.com/get" {
		t.Fatalf("expected signed_url, got %q", got)
	}
	if _, ok := structured["object_key"]; ok {
		t.Fatalf("expected no object_key in SIGNED_URL mode, got %v", structured["object_key"])
	}
	if _, ok := structured["filename"]; ok {
		t.Fatalf("expected no filename in SIGNED_URL mode, got %v", structured["filename"])
	}
}

func TestMCPToolCallExportFileObjectKeyOnly(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	originalExportObjectID := newExportObjectID
	newExportObjectID = func() string { return "fixed-id" }
	t.Cleanup(func() {
		newExportObjectID = originalExportObjectID
	})

	presignDownloadCalled := false
	store := &fakeExportStore{
		presignUpload: func(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error) {
			return "https://uploads.example.com/put", nil
		},
		presignDownload: func(ctx context.Context, objectKey string, expiresIn time.Duration) (string, error) {
			presignDownloadCalled = true
			return "https://downloads.example.com/get", nil
		},
	}

	router := newMCPTestRouterWithObjectStoreAndTTLs(t, &fakeMCPDispatcher{
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			resultJSON, _ := json.Marshal(mcpTerminalResourceResult{
				SessionID: "session-1",
				FilePath:  "/workspace/report.png",
				MIMEType:  "image/png",
				SizeBytes: 4,
			})
			return grpcserver.SubmitTaskResult{
				Task: grpcserver.TaskSnapshot{
					TaskID:     "task-export-objectkey",
					Capability: terminalResourceCapabilityName,
					Status:     grpcserver.TaskStatusSucceeded,
					ResultJSON: resultJSON,
					CreatedAt:  now,
					UpdatedAt:  now,
					DeadlineAt: now.Add(60 * time.Second),
				},
				Completed: true,
			}, nil
		},
	}, store, "exports", exportFileUploadPresignTTL, exportFileDownloadPresignTTL, ExportReturnSchemaObjectKey)

	// Verify output schema declares object_key and filename only
	listPayload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	listResult := mustMapField(t, listPayload, "result")
	toolsRaw := listResult["tools"].([]any)
	for _, toolRaw := range toolsRaw {
		tool := toolRaw.(map[string]any)
		if asString(t, tool["name"]) != exportFileToolName {
			continue
		}
		outputSchema := mustObject(t, tool["outputSchema"], "exportFile.outputSchema")
		assertRequiredContains(t, outputSchema["required"], "object_key")
		assertRequiredContains(t, outputSchema["required"], "filename")
		assertRequiredNotContains(t, outputSchema["required"], "signed_url")
	}

	// Verify tool call returns only object_key and filename
	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exportFile","arguments":{"session_id":"session-1","file_path":"/workspace/report.png"}}}`)
	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected tool call success, got error payload=%s", mustJSON(t, result))
	}
	structured := mustMapField(t, result, "structuredContent")
	if got := asString(t, structured["object_key"]); got != "exports/session-1/fixed-id-report.png" {
		t.Fatalf("expected object_key, got %q", got)
	}
	if got := asString(t, structured["filename"]); got != "report.png" {
		t.Fatalf("expected filename, got %q", got)
	}
	if _, ok := structured["signed_url"]; ok {
		t.Fatalf("expected no signed_url in OBJECTKEY mode, got %v", structured["signed_url"])
	}
	if presignDownloadCalled {
		t.Fatalf("expected PresignDownload to NOT be called in OBJECTKEY mode")
	}
}

func TestMCPToolCallReadImageComputerUseSessionRoutesToReadImageCapability(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	actions := make([]string, 0, 2)
	router := newMCPTestRouter(t, &fakeMCPDispatcher{
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			if req.Capability != readImageCapabilityName {
				t.Fatalf("expected capability=%q, got %q", readImageCapabilityName, req.Capability)
			}
			if req.OwnerID != testDashboardAccountID {
				t.Fatalf("expected owner_id from token, got %q", req.OwnerID)
			}
			payload := mcpTerminalResourcePayload{}
			if err := json.Unmarshal(req.InputJSON, &payload); err != nil {
				t.Fatalf("expected valid readImage payload, got %s", string(req.InputJSON))
			}
			actions = append(actions, payload.Action)
			switch payload.Action {
			case "validate":
				resultJSON, _ := json.Marshal(mcpTerminalResourceResult{
					SessionID: payload.SessionID,
					FilePath:  payload.FilePath,
					MIMEType:  "image/png",
					SizeBytes: 4,
				})
				return grpcserver.SubmitTaskResult{
					Task: grpcserver.TaskSnapshot{
						TaskID:     "task-read-image-sys-validate",
						Capability: readImageCapabilityName,
						Status:     grpcserver.TaskStatusSucceeded,
						ResultJSON: resultJSON,
						CreatedAt:  now,
						UpdatedAt:  now,
						DeadlineAt: now.Add(60 * time.Second),
					},
					Completed: true,
				}, nil
			case "read":
				resultJSON, _ := json.Marshal(mcpTerminalResourceResult{
					SessionID: payload.SessionID,
					FilePath:  payload.FilePath,
					MIMEType:  "image/png",
					SizeBytes: 4,
					Blob:      []byte{0x89, 0x50, 0x4e, 0x47},
				})
				return grpcserver.SubmitTaskResult{
					Task: grpcserver.TaskSnapshot{
						TaskID:     "task-read-image-sys-read",
						Capability: readImageCapabilityName,
						Status:     grpcserver.TaskStatusSucceeded,
						ResultJSON: resultJSON,
						CreatedAt:  now,
						UpdatedAt:  now,
						DeadlineAt: now.Add(60 * time.Second),
					},
					Completed: true,
				}, nil
			default:
				t.Fatalf("unexpected action: %q", payload.Action)
				return grpcserver.SubmitTaskResult{}, nil
			}
		},
	})

	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"readImage","arguments":{"session_id":"computerUse","file_path":"/workspace/image.png"}}}`)
	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected tool call success, got error payload=%s", mustJSON(t, result))
	}
	contentRaw, ok := result["content"].([]any)
	if !ok || len(contentRaw) != 1 {
		t.Fatalf("expected [image] content, got %s", mustJSON(t, result))
	}
	first := mustObject(t, contentRaw[0], "readImage.content[0]")
	if got := asString(t, first["type"]); got != "image" {
		t.Fatalf("expected content type image, got %q", got)
	}
	if got := asString(t, first["mimeType"]); got != "image/png" {
		t.Fatalf("expected image mimeType=image/png, got %q", got)
	}
	if len(actions) != 2 || actions[0] != "validate" || actions[1] != "read" {
		t.Fatalf("expected validate/read actions, got %#v", actions)
	}
}

func TestMCPToolCallReadImageComputerUseSessionIsCaseSensitive(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	router := newMCPTestRouter(t, &fakeMCPDispatcher{
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			if req.Capability != terminalResourceCapabilityName {
				t.Fatalf("expected capability=%q for lowercase session_id, got %q", terminalResourceCapabilityName, req.Capability)
			}
			payload := mcpTerminalResourcePayload{}
			if err := json.Unmarshal(req.InputJSON, &payload); err != nil {
				t.Fatalf("expected valid terminalResource payload, got %s", string(req.InputJSON))
			}
			resultJSON, _ := json.Marshal(mcpTerminalResourceResult{
				SessionID: payload.SessionID,
				FilePath:  payload.FilePath,
				MIMEType:  "image/png",
				SizeBytes: 4,
				Blob:      []byte{0x89, 0x50, 0x4e, 0x47},
			})
			return grpcserver.SubmitTaskResult{
				Task: grpcserver.TaskSnapshot{
					TaskID:     "task-read-image-lowercase",
					Capability: terminalResourceCapabilityName,
					Status:     grpcserver.TaskStatusSucceeded,
					ResultJSON: resultJSON,
					CreatedAt:  now,
					UpdatedAt:  now,
					DeadlineAt: now.Add(60 * time.Second),
				},
				Completed: true,
			}, nil
		},
	})

	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"readImage","arguments":{"session_id":"computeruse","file_path":"/workspace/image.png"}}}`)
	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected tool call success, got error payload=%s", mustJSON(t, result))
	}
}

func TestMCPToolCallReadImageUnsupportedMIME(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	readCalled := false
	router := newMCPTestRouter(t, &fakeMCPDispatcher{
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			if req.Capability != terminalResourceCapabilityName {
				t.Fatalf("expected capability=%q, got %q", terminalResourceCapabilityName, req.Capability)
			}
			payload := mcpTerminalResourcePayload{}
			if err := json.Unmarshal(req.InputJSON, &payload); err != nil {
				t.Fatalf("expected valid terminalResource payload, got %s", string(req.InputJSON))
			}
			if payload.Action == "read" {
				readCalled = true
				t.Fatalf("read should not be called for unsupported mime type")
			}

			resultJSON, _ := json.Marshal(mcpTerminalResourceResult{
				SessionID: payload.SessionID,
				FilePath:  payload.FilePath,
				MIMEType:  "application/pdf",
				SizeBytes: 4,
			})
			return grpcserver.SubmitTaskResult{
				Task: grpcserver.TaskSnapshot{
					TaskID:     "task-read-image-pdf-validate",
					Capability: terminalResourceCapabilityName,
					Status:     grpcserver.TaskStatusSucceeded,
					ResultJSON: resultJSON,
					CreatedAt:  now,
					UpdatedAt:  now,
					DeadlineAt: now.Add(60 * time.Second),
				},
				Completed: true,
			}, nil
		},
	})

	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"readImage","arguments":{"session_id":"session-1","file_path":"/workspace/file.pdf"}}}`)
	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected tool call success, got error payload=%s", mustJSON(t, result))
	}
	contentRaw, ok := result["content"].([]any)
	if !ok || len(contentRaw) != 1 {
		t.Fatalf("expected [text] content, got %s", mustJSON(t, result))
	}
	first := mustObject(t, contentRaw[0], "readImage.content[0]")
	if got := asString(t, first["type"]); got != "text" {
		t.Fatalf("expected content type text, got %q", got)
	}
	if got := asString(t, first["text"]); got != "unsupported mime type: application/pdf; expected image/*" {
		t.Fatalf("unexpected unsupported mime message: %q", got)
	}
	if readCalled {
		t.Fatalf("expected unsupported mime to skip read")
	}
}

func TestMCPToolCallReadImageReadReturnsUnsupportedMIME(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	router := newMCPTestRouter(t, &fakeMCPDispatcher{
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			if req.Capability != terminalResourceCapabilityName {
				t.Fatalf("expected capability=%q, got %q", terminalResourceCapabilityName, req.Capability)
			}
			payload := mcpTerminalResourcePayload{}
			if err := json.Unmarshal(req.InputJSON, &payload); err != nil {
				t.Fatalf("expected valid terminalResource payload, got %s", string(req.InputJSON))
			}

			switch payload.Action {
			case "validate":
				resultJSON, _ := json.Marshal(mcpTerminalResourceResult{
					SessionID: payload.SessionID,
					FilePath:  payload.FilePath,
					MIMEType:  "image/png",
					SizeBytes: 4,
				})
				return grpcserver.SubmitTaskResult{
					Task: grpcserver.TaskSnapshot{
						TaskID:     "task-read-image-mismatch-validate",
						Capability: terminalResourceCapabilityName,
						Status:     grpcserver.TaskStatusSucceeded,
						ResultJSON: resultJSON,
						CreatedAt:  now,
						UpdatedAt:  now,
						DeadlineAt: now.Add(60 * time.Second),
					},
					Completed: true,
				}, nil
			case "read":
				resultJSON, _ := json.Marshal(mcpTerminalResourceResult{
					SessionID: payload.SessionID,
					FilePath:  payload.FilePath,
					MIMEType:  "text/plain",
					SizeBytes: 3,
					Blob:      []byte("abc"),
				})
				return grpcserver.SubmitTaskResult{
					Task: grpcserver.TaskSnapshot{
						TaskID:     "task-read-image-mismatch-read",
						Capability: terminalResourceCapabilityName,
						Status:     grpcserver.TaskStatusSucceeded,
						ResultJSON: resultJSON,
						CreatedAt:  now,
						UpdatedAt:  now,
						DeadlineAt: now.Add(60 * time.Second),
					},
					Completed: true,
				}, nil
			default:
				t.Fatalf("unexpected action: %q", payload.Action)
				return grpcserver.SubmitTaskResult{}, nil
			}
		},
	})

	payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"readImage","arguments":{"session_id":"session-1","file_path":"/workspace/image.png"}}}`)
	result := mustMapField(t, payload, "result")
	if asBool(result["isError"]) {
		t.Fatalf("expected tool call success, got error payload=%s", mustJSON(t, result))
	}
	contentRaw, ok := result["content"].([]any)
	if !ok || len(contentRaw) != 1 {
		t.Fatalf("expected [text] content, got %s", mustJSON(t, result))
	}
	first := mustObject(t, contentRaw[0], "readImage.content[0]")
	if got := asString(t, first["type"]); got != "text" {
		t.Fatalf("expected content type text, got %q", got)
	}
	if got := asString(t, first["text"]); got != "unsupported mime type: text/plain; expected image/*" {
		t.Fatalf("unexpected unsupported mime message: %q", got)
	}
}

func TestMCPToolCallReadImageReadFailuresAreToolErrors(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name      string
		status    grpcserver.TaskStatus
		errorCode string
		errorMsg  string
		wantText  string
	}{
		{
			name:      "file_too_large",
			status:    grpcserver.TaskStatusFailed,
			errorCode: "file_too_large",
			errorMsg:  "file too large",
			wantText:  "file_too_large: file too large",
		},
		{
			name:      "task_timeout",
			status:    grpcserver.TaskStatusTimeout,
			errorCode: "",
			errorMsg:  "",
			wantText:  "task timed out",
		},
		{
			name:      "session_busy",
			status:    grpcserver.TaskStatusFailed,
			errorCode: terminalExecSessionBusyCode,
			errorMsg:  "session busy",
			wantText:  terminalExecSessionBusyCode + ": session busy",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := newMCPTestRouter(t, &fakeMCPDispatcher{
				submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
					if req.Capability != terminalResourceCapabilityName {
						t.Fatalf("expected capability=%q, got %q", terminalResourceCapabilityName, req.Capability)
					}
					payload := mcpTerminalResourcePayload{}
					if err := json.Unmarshal(req.InputJSON, &payload); err != nil {
						t.Fatalf("expected valid terminalResource payload, got %s", string(req.InputJSON))
					}

					if payload.Action == "validate" {
						resultJSON, _ := json.Marshal(mcpTerminalResourceResult{
							SessionID: payload.SessionID,
							FilePath:  payload.FilePath,
							MIMEType:  "image/png",
							SizeBytes: 4,
						})
						return grpcserver.SubmitTaskResult{
							Task: grpcserver.TaskSnapshot{
								TaskID:     "task-read-image-validate",
								Capability: terminalResourceCapabilityName,
								Status:     grpcserver.TaskStatusSucceeded,
								ResultJSON: resultJSON,
								CreatedAt:  now,
								UpdatedAt:  now,
								DeadlineAt: now.Add(60 * time.Second),
							},
							Completed: true,
						}, nil
					}
					return grpcserver.SubmitTaskResult{
						Task: grpcserver.TaskSnapshot{
							TaskID:       "task-read-image-read",
							Capability:   terminalResourceCapabilityName,
							Status:       tc.status,
							ErrorCode:    tc.errorCode,
							ErrorMessage: tc.errorMsg,
							CreatedAt:    now,
							UpdatedAt:    now,
							DeadlineAt:   now.Add(60 * time.Second),
						},
						Completed: true,
					}, nil
				},
			})

			payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"readImage","arguments":{"session_id":"session-1","file_path":"/workspace/image.png"}}}`)
			assertMCPToolError(t, payload, tc.wantText)
		})
	}
}

func TestMCPToolCallReadImageValidationErrors(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name      string
		errorCode string
		errorMsg  string
	}{
		{
			name:      "file_not_found",
			errorCode: "file_not_found",
			errorMsg:  "file not found",
		},
		{
			name:      "path_is_directory",
			errorCode: "path_is_directory",
			errorMsg:  "path is directory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := newMCPTestRouter(t, &fakeMCPDispatcher{
				submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
					if req.Capability != terminalResourceCapabilityName {
						t.Fatalf("expected capability=%q, got %q", terminalResourceCapabilityName, req.Capability)
					}
					return grpcserver.SubmitTaskResult{
						Task: grpcserver.TaskSnapshot{
							TaskID:       "task-read-image-error",
							Capability:   terminalResourceCapabilityName,
							Status:       grpcserver.TaskStatusFailed,
							ErrorCode:    tc.errorCode,
							ErrorMessage: tc.errorMsg,
							CreatedAt:    now,
							UpdatedAt:    now,
							DeadlineAt:   now.Add(60 * time.Second),
						},
						Completed: true,
					}, nil
				},
			})

			payload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"readImage","arguments":{"session_id":"session-1","file_path":"/workspace/a.txt"}}}`)
			assertMCPToolError(t, payload, tc.errorCode+": "+tc.errorMsg)
		})
	}
}

func TestMCPToolCallInvalidParams(t *testing.T) {
	router := newMCPTestRouter(t, &fakeMCPDispatcher{})

	echoPayload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"message":"   "}}}`)
	assertMCPInvalidParamsError(t, echoPayload)

	pythonPayload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"pythonExec","arguments":{"code":"  "}}}`)
	assertMCPInvalidParamsError(t, pythonPayload)

	echoUnknownField := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"message":"hello","unknown":"x"}}}`)
	assertMCPInvalidParamsError(t, echoUnknownField)

	pythonUnknownField := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"pythonExec","arguments":{"code":"print(1)","unknown":"x"}}}`)
	assertMCPInvalidParamsError(t, pythonUnknownField)

	terminalBlank := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"terminalExec","arguments":{"command":"  "}}}`)
	assertMCPInvalidParamsError(t, terminalBlank)

	terminalUnknownField := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"terminalExec","arguments":{"command":"pwd","unknown":"x"}}}`)
	assertMCPInvalidParamsError(t, terminalUnknownField)

	computerUseBlank := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"computerUse","arguments":{"command":"  "}}}`)
	assertMCPInvalidParamsError(t, computerUseBlank)

	computerUseUnknownField := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"computerUse","arguments":{"command":"pwd","unknown":"x"}}}`)
	assertMCPInvalidParamsError(t, computerUseUnknownField)

	computerUseLeaseTTLField := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"computerUse","arguments":{"command":"pwd","lease_ttl_sec":60}}}`)
	assertMCPInvalidParamsError(t, computerUseLeaseTTLField)

	registerBlankSession := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"readImage","arguments":{"session_id":"  ","file_path":"/workspace/a.txt"}}}`)
	assertMCPInvalidParamsError(t, registerBlankSession)

	registerUnknownField := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"readImage","arguments":{"session_id":"session-1","file_path":"/workspace/a.txt","unknown":"x"}}}`)
	assertMCPInvalidParamsError(t, registerUnknownField)
}

func TestMCPToolCallBackendErrorsAsToolErrors(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	router := newMCPTestRouter(t, &fakeMCPDispatcher{
		dispatchEcho: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			return "", grpcserver.ErrNoEchoWorker
		},
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			if req.Capability == terminalExecCapabilityName || req.Capability == terminalResourceCapabilityName || req.Capability == computerUseCapabilityName {
				return grpcserver.SubmitTaskResult{
					Task: grpcserver.TaskSnapshot{
						TaskID:       "task-3",
						Capability:   req.Capability,
						Status:       grpcserver.TaskStatusFailed,
						ErrorCode:    terminalExecSessionNotFoundCode,
						ErrorMessage: "session not found",
						CreatedAt:    now,
						UpdatedAt:    now,
						DeadlineAt:   now.Add(60 * time.Second),
					},
					Completed: true,
				}, nil
			}
			return grpcserver.SubmitTaskResult{
				Task: grpcserver.TaskSnapshot{
					TaskID:       "task-2",
					Capability:   pythonExecCapabilityName,
					Status:       grpcserver.TaskStatusFailed,
					ErrorCode:    "execution_failed",
					ErrorMessage: "pythonExec execution failed: docker is unavailable",
					CreatedAt:    now,
					UpdatedAt:    now,
					DeadlineAt:   now.Add(60 * time.Second),
				},
				Completed: true,
			}, nil
		},
	})

	echoPayload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"message":"hello"}}}`)
	assertMCPToolError(t, echoPayload, "no online worker supports echo")

	pythonPayload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"pythonExec","arguments":{"code":"print(1)"}}}`)
	assertMCPToolError(t, pythonPayload, "execution_failed: pythonExec execution failed: docker is unavailable")

	terminalPayload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"terminalExec","arguments":{"command":"pwd","session_id":"missing"}}}`)
	assertMCPToolError(t, terminalPayload, terminalExecSessionNotFoundCode+": session not found")

	resourcePayload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"readImage","arguments":{"session_id":"missing","file_path":"/workspace/a.txt"}}}`)
	assertMCPToolError(t, resourcePayload, terminalExecSessionNotFoundCode+": session not found")

	computerUsePayload := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"computerUse","arguments":{"command":"pwd"}}}`)
	assertMCPToolError(t, computerUsePayload, terminalExecSessionNotFoundCode+": session not found")
}

func TestMCPGetReturnsMethodNotAllowed(t *testing.T) {
	router := newMCPTestRouter(t, &fakeMCPDispatcher{})
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(trustedTokenHeader, "Bearer "+testMCPToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d body=%s", rec.Code, rec.Body.String())
	}
	if allow := strings.TrimSpace(rec.Header().Get("Allow")); allow != "POST" {
		t.Fatalf("expected Allow=POST, got %q", allow)
	}
}

func TestMCPPostRequiresToken(t *testing.T) {
	router := newMCPTestRouter(t, &fakeMCPDispatcher{})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func newMCPTestRouter(t *testing.T, dispatcher CommandDispatcher) http.Handler {
	t.Helper()

	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, dispatcher, nil, nil, "")
	return mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
}

func newMCPTestRouterWithObjectStore(t *testing.T, dispatcher CommandDispatcher, store ExportStore, exportPrefix string) http.Handler {
	return newMCPTestRouterWithObjectStoreAndTTLs(
		t,
		dispatcher,
		store,
		exportPrefix,
		exportFileUploadPresignTTL,
		exportFileDownloadPresignTTL,
		ExportReturnSchemaAll,
	)
}

func newMCPTestRouterWithObjectStoreAndTTLs(
	t *testing.T,
	dispatcher CommandDispatcher,
	store ExportStore,
	exportPrefix string,
	uploadTTL time.Duration,
	downloadTTL time.Duration,
	returnSchema string,
) http.Handler {
	t.Helper()

	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, dispatcher, nil, nil, "")
	handler.SetExportStore(store, exportPrefix, uploadTTL, downloadTTL, returnSchema)
	return mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
}

func newMCPTestRouterWithHiddenTools(t *testing.T, dispatcher CommandDispatcher, hiddenTools map[string]bool) http.Handler {
	t.Helper()

	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, dispatcher, nil, nil, "")
	return mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil, hiddenTools)
}

// newMCPTestRouterWithOverrides builds a test router wiring MCP tool
// overrides directly to NewRouter, using the provided slog.Logger as the
// process-wide default for the duration of the test. Passing logger=nil
// leaves slog.Default() untouched.
func newMCPTestRouterWithOverrides(t *testing.T, dispatcher CommandDispatcher, overrides map[string]config.MCPToolOverride, logger *slog.Logger) http.Handler {
	t.Helper()

	if logger != nil {
		prev := slog.Default()
		slog.SetDefault(logger)
		t.Cleanup(func() { slog.SetDefault(prev) })
	}

	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, dispatcher, nil, nil, "")
	router, err := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil, nil, overrides)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return router
}

// captureLog is a slog.Handler that stores each record in a thread-safe buffer
// so that tests can assert warn-level diagnostics emitted during handler
// construction.
type captureLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
	h   slog.Handler
}

func newCaptureLogger() (*slog.Logger, *captureLog) {
	c := &captureLog{}
	c.h = slog.NewTextHandler(&c.buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(c), c
}

func (c *captureLog) Enabled(ctx context.Context, l slog.Level) bool {
	return c.h.Enabled(ctx, l)
}

func (c *captureLog) Handle(ctx context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.h.Handle(ctx, r)
}

func (c *captureLog) WithAttrs(attrs []slog.Attr) slog.Handler { return c.h.WithAttrs(attrs) }
func (c *captureLog) WithGroup(name string) slog.Handler       { return c.h.WithGroup(name) }

func (c *captureLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func mcpPostJSON(t *testing.T, router http.Handler, body string) map[string]any {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(trustedTokenHeader, "Bearer "+testMCPToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode MCP response: %v body=%s", err, rec.Body.String())
	}
	return payload
}

func assertMCPInvalidParamsError(t *testing.T, payload map[string]any) {
	t.Helper()

	errorBody := mustMapField(t, payload, "error")
	if code := asInt(t, errorBody["code"]); code != -32602 {
		t.Fatalf("expected JSON-RPC invalid params -32602, got %d body=%s", code, mustJSON(t, payload))
	}
}

func assertMCPToolError(t *testing.T, payload map[string]any, contains string) {
	t.Helper()

	result := mustMapField(t, payload, "result")
	if !asBool(result["isError"]) {
		t.Fatalf("expected tool error result, got %s", mustJSON(t, result))
	}

	contentRaw, ok := result["content"].([]any)
	if !ok || len(contentRaw) == 0 {
		t.Fatalf("expected non-empty content in tool error, got %s", mustJSON(t, result))
	}
	first, ok := contentRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("expected content object, got %#v", contentRaw[0])
	}
	text := asString(t, first["text"])
	if !strings.Contains(text, contains) {
		t.Fatalf("expected tool error text containing %q, got %q", contains, text)
	}
}

func mustMapField(t *testing.T, payload map[string]any, field string) map[string]any {
	t.Helper()

	raw, ok := payload[field]
	if !ok {
		t.Fatalf("missing field %q in payload=%s", field, mustJSON(t, payload))
	}
	result, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("field %q must be object, got %#v", field, raw)
	}
	return result
}

func mustObject(t *testing.T, value any, label string) map[string]any {
	t.Helper()
	obj, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected %s to be object, got %#v", label, value)
	}
	return obj
}

func assertRequiredContains(t *testing.T, raw any, expected string) {
	t.Helper()
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("expected required to be an array, got %#v", raw)
	}
	for _, item := range items {
		value, ok := item.(string)
		if ok && value == expected {
			return
		}
	}
	t.Fatalf("required array %#v does not contain %q", items, expected)
}

func assertRequiredNotContains(t *testing.T, raw any, unexpected string) {
	t.Helper()
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("expected required to be an array, got %#v", raw)
	}
	for _, item := range items {
		value, ok := item.(string)
		if ok && value == unexpected {
			t.Fatalf("required array %#v should not contain %q", items, unexpected)
		}
	}
}

func asBool(value any) bool {
	parsed, _ := value.(bool)
	return parsed
}

func asString(t *testing.T, value any) string {
	t.Helper()
	result, ok := value.(string)
	if !ok {
		t.Fatalf("expected string, got %#v", value)
	}
	return result
}

func asInt(t *testing.T, value any) int {
	t.Helper()
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	default:
		t.Fatalf("expected number, got %#v", value)
		return 0
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to encode json: %v", err)
	}
	return string(encoded)
}
