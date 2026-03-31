package runner

import (
	"fmt"
	"testing"
)

func TestCommandDispatchSummaryForLog(t *testing.T) {
	t.Parallel()

	leaseTTL := 90
	invalidJSONPayload := []byte("not-json")
	missingRequiredFieldsPayload := []byte(`{"session_id":"s1","file_path":"   "}`)
	unsupportedPayload := []byte(`{"x":1}`)
	tests := []struct {
		name       string
		capability string
		payload    []byte
		want       string
	}{
		{
			name:       "echo_payload_logs_message_length",
			capability: echoCapabilityName,
			payload:    []byte(`{"message":"hello"}`),
			want:       "message_len=5",
		},
		{
			name:       "python_exec_payload_logs_code_length",
			capability: pythonExecCapabilityName,
			payload:    []byte(`{"code":"abc"}`),
			want:       "code_len=3",
		},
		{
			name:       "terminal_exec_payload_logs_fields_with_default_lease",
			capability: terminalExecCapabilityName,
			payload:    []byte(`{"command":"pwd","session_id":"sess-1"}`),
			want:       "command_len=3 session_id_present=true create_if_missing=false lease_ttl_sec=default",
		},
		{
			name:       "terminal_exec_payload_logs_fields_with_explicit_lease",
			capability: terminalExecCapabilityName,
			payload:    []byte(fmt.Sprintf(`{"command":"ls","create_if_missing":true,"lease_ttl_sec":%d}`, leaseTTL)),
			want:       "command_len=2 session_id_present=false create_if_missing=true lease_ttl_sec=90",
		},
		{
			name:       "terminal_resource_payload_logs_read_action",
			capability: terminalResourceCapabilityName,
			payload:    []byte(`{"session_id":"s1","file_path":"/tmp/a","action":"read"}`),
			want:       "action=read session_id_present=true file_path_len=6 signed_url_present=false",
		},
		{
			name:       "terminal_resource_payload_logs_validate_action",
			capability: terminalResourceCapabilityName,
			payload:    []byte(`{"session_id":"s1","file_path":"/tmp/a","action":"validate"}`),
			want:       "action=validate session_id_present=true file_path_len=6 signed_url_present=false",
		},
		{
			name:       "terminal_resource_payload_logs_default_action",
			capability: terminalResourceCapabilityName,
			payload:    []byte(`{"session_id":"s1","file_path":"/tmp/a"}`),
			want:       "action=default session_id_present=true file_path_len=6 signed_url_present=false",
		},
		{
			name:       "terminal_resource_payload_logs_export_action",
			capability: terminalResourceCapabilityName,
			payload:    []byte(`{"session_id":"s1","file_path":"/tmp/a","action":"export","signed_url":"https://uploads.example.com/put"}`),
			want:       "action=export session_id_present=true file_path_len=6 signed_url_present=true",
		},
		{
			name:       "terminal_resource_payload_logs_invalid_action",
			capability: terminalResourceCapabilityName,
			payload:    []byte(`{"session_id":"s1","file_path":"/tmp/a","action":"download"}`),
			want:       "action=invalid session_id_present=true file_path_len=6 signed_url_present=false",
		},
		{
			name:       "invalid_json_falls_back_to_parse_failed",
			capability: pythonExecCapabilityName,
			payload:    invalidJSONPayload,
			want:       fmt.Sprintf("payload_len=%d summary=parse_failed", len(invalidJSONPayload)),
		},
		{
			name:       "missing_required_fields_falls_back_to_parse_failed",
			capability: terminalResourceCapabilityName,
			payload:    missingRequiredFieldsPayload,
			want:       fmt.Sprintf("payload_len=%d summary=parse_failed", len(missingRequiredFieldsPayload)),
		},
		{
			name:       "unsupported_capability_logs_fallback_summary",
			capability: "unknown",
			payload:    unsupportedPayload,
			want:       fmt.Sprintf("payload_len=%d summary=unsupported_capability", len(unsupportedPayload)),
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := commandDispatchSummaryForLog(tc.capability, tc.payload)
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}
