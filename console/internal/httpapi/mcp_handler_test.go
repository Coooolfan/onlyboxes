package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
)

type fakeMCPDispatcher struct {
	dispatchEcho func(ctx context.Context, message string, timeout time.Duration) (string, error)
	submitTask   func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error)
	getTask      func(taskID string, ownerID string) (grpcserver.TaskSnapshot, bool)
	cancelTask   func(taskID string, ownerID string) (grpcserver.TaskSnapshot, error)
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
	if len(toolsRaw) != 4 {
		t.Fatalf("expected exactly 4 tools, got %d", len(toolsRaw))
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
	if got := asInt(t, pythonTimeoutSchema["maximum"]); got != maxMCPPythonExecTimeoutMS {
		t.Fatalf("expected pythonExec.timeout_ms.maximum=%d, got %d", maxMCPPythonExecTimeoutMS, got)
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

	readImageTool := toolByName["readImage"]
	if got := asString(t, readImageTool["title"]); got != mcpReadImageToolTitle {
		t.Fatalf("expected readImage title %q, got %q", mcpReadImageToolTitle, got)
	}
	if got := asString(t, readImageTool["description"]); got != mcpReadImageToolDescription {
		t.Fatalf("unexpected readImage description: %q", got)
	}
	readImageInputSchema := mustObject(t, readImageTool["inputSchema"], "readImage.inputSchema")
	assertRequiredContains(t, readImageInputSchema["required"], "session_id")
	assertRequiredContains(t, readImageInputSchema["required"], "file_path")
	if _, ok := readImageTool["outputSchema"]; ok {
		t.Fatalf("did not expect readImage.outputSchema")
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
			if req.OwnerID != ownerIDFromToken(testMCPToken) {
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
			if req.OwnerID != ownerIDFromToken(testMCPToken) {
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

func TestMCPToolCallReadImageSuccess(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	router := newMCPTestRouter(t, &fakeMCPDispatcher{
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			if req.Capability != terminalResourceCapabilityName {
				t.Fatalf("expected capability=%q, got %q", terminalResourceCapabilityName, req.Capability)
			}
			if req.OwnerID != ownerIDFromToken(testMCPToken) {
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

	registerBlankSession := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"readImage","arguments":{"session_id":"  ","file_path":"/workspace/a.txt"}}}`)
	assertMCPInvalidParamsError(t, registerBlankSession)

	registerUnknownField := mcpPostJSON(t, router, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"readImage","arguments":{"session_id":"session-1","file_path":"/workspace/a.txt","unknown":"x"}}}`)
	assertMCPInvalidParamsError(t, registerUnknownField)
}

func TestMCPToolCallBackendErrorsAsToolErrors(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	router := newMCPTestRouter(t, &fakeMCPDispatcher{
		dispatchEcho: func(ctx context.Context, message string, timeout time.Duration) (string, error) {
			return "", grpcserver.ErrNoEchoWorker
		},
		submitTask: func(ctx context.Context, req grpcserver.SubmitTaskRequest) (grpcserver.SubmitTaskResult, error) {
			if req.Capability == terminalExecCapabilityName || req.Capability == terminalResourceCapabilityName {
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
}

func TestMCPGetReturnsMethodNotAllowed(t *testing.T) {
	router := newMCPTestRouter(t, &fakeMCPDispatcher{})
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(mcpTokenHeader, testMCPToken)
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

	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, dispatcher, nil, "")
	return NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
}

func mcpPostJSON(t *testing.T, router http.Handler, body string) map[string]any {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(mcpTokenHeader, testMCPToken)
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
