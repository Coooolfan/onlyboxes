package e2b

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	processv1 "github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b/process/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b/process/v1/processv1connect"
)

type processTestHandler struct {
	processv1connect.UnimplementedProcessHandler
	t *testing.T
}

type processSignalRecord struct {
	pid       uint32
	tag       string
	signal    processv1.Signal
	sandboxID string
	token     string
}

type interruptedProcessTestHandler struct {
	processv1connect.UnimplementedProcessHandler
	sendStart bool
	startTags chan string
	signals   chan processSignalRecord
	notFound  *atomic.Int32
}

func TestRequestTimeoutBoundsEnvdSetupWithoutBoundingStreams(t *testing.T) {
	t.Parallel()
	client, err := NewClient(Config{
		APIKey:         "test-key",
		RequestTimeout: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.controlHTTP.Timeout != 250*time.Millisecond {
		t.Fatalf("unexpected Control API timeout: %s", client.controlHTTP.Timeout)
	}
	if client.sandboxHTTP.Timeout != 0 {
		t.Fatalf("envd must use the command context deadline, got client timeout %s", client.sandboxHTTP.Timeout)
	}
	transport, ok := client.sandboxHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected envd transport %T", client.sandboxHTTP.Transport)
	}
	if transport.TLSHandshakeTimeout != 250*time.Millisecond ||
		transport.ResponseHeaderTimeout != 250*time.Millisecond {
		t.Fatalf(
			"unexpected envd setup timeouts: tls=%s response_headers=%s",
			transport.TLSHandshakeTimeout,
			transport.ResponseHeaderTimeout,
		)
	}
}

func (h processTestHandler) Start(
	_ context.Context,
	req *connect.Request[processv1.StartRequest],
	stream *connect.ServerStream[processv1.StartResponse],
) error {
	if req.Header().Get("E2b-Sandbox-Id") != "sb-command" ||
		req.Header().Get("X-Access-Token") != "command-token" {
		h.t.Errorf("missing E2B routing headers: %#v", req.Header())
	}
	process := req.Msg.GetProcess()
	if process.GetCmd() != "/bin/bash" || strings.Join(process.GetArgs(), " ") != "-l -c printf ok" {
		h.t.Errorf("unexpected process request: %#v", process)
	}
	if strings.TrimSpace(req.Msg.GetTag()) == "" {
		h.t.Error("process request is missing a cleanup tag")
	}
	if err := stream.Send(&processv1.StartResponse{Event: &processv1.ProcessEvent{
		Event: &processv1.ProcessEvent_Start{Start: &processv1.ProcessEvent_StartEvent{Pid: 42}},
	}}); err != nil {
		return err
	}
	if err := stream.Send(&processv1.StartResponse{Event: &processv1.ProcessEvent{
		Event: &processv1.ProcessEvent_Data{Data: &processv1.ProcessEvent_DataEvent{
			Output: &processv1.ProcessEvent_DataEvent_Stdout{Stdout: []byte("ok")},
		}},
	}}); err != nil {
		return err
	}
	return stream.Send(&processv1.StartResponse{Event: &processv1.ProcessEvent{
		Event: &processv1.ProcessEvent_End{End: &processv1.ProcessEvent_EndEvent{ExitCode: 0, Exited: true}},
	}})
}

func (h interruptedProcessTestHandler) Start(
	_ context.Context,
	req *connect.Request[processv1.StartRequest],
	stream *connect.ServerStream[processv1.StartResponse],
) error {
	h.startTags <- req.Msg.GetTag()
	if h.sendStart {
		if err := stream.Send(&processv1.StartResponse{Event: &processv1.ProcessEvent{
			Event: &processv1.ProcessEvent_Start{
				Start: &processv1.ProcessEvent_StartEvent{Pid: 42},
			},
		}}); err != nil {
			return err
		}
	}
	return connect.NewError(connect.CodeUnavailable, errors.New("output stream interrupted"))
}

func (h interruptedProcessTestHandler) SendSignal(
	_ context.Context,
	req *connect.Request[processv1.SendSignalRequest],
) (*connect.Response[processv1.SendSignalResponse], error) {
	if h.notFound != nil && h.notFound.Add(-1) >= 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("process is not registered yet"))
	}
	h.signals <- processSignalRecord{
		pid:       req.Msg.GetProcess().GetPid(),
		tag:       req.Msg.GetProcess().GetTag(),
		signal:    req.Msg.GetSignal(),
		sandboxID: req.Header().Get("E2b-Sandbox-Id"),
		token:     req.Header().Get("X-Access-Token"),
	}
	return connect.NewResponse(&processv1.SendSignalResponse{}), nil
}

func TestControlPlaneLifecycle(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var requests []struct {
		Method string
		Path   string
		Body   map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("missing API key")
		}
		body := map[string]any{}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		mu.Lock()
		requests = append(requests, struct {
			Method string
			Path   string
			Body   map[string]any
		}{r.Method, r.URL.Path, body})
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sandboxID":       "sb-1",
				"envdVersion":     "0.6.2",
				"envdAccessToken": "envd-token",
				"domain":          "example.test",
			})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{
		APIKey:         "test-key",
		APIURL:         server.URL,
		RequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := client.Create(context.Background(), "template-1", 90)
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.ID != "sb-1" || sandbox.Domain != "example.test" || sandbox.AccessToken != "envd-token" {
		t.Fatalf("unexpected sandbox: %#v", sandbox)
	}
	if err := client.SetTimeout(context.Background(), sandbox.ID, 120); err != nil {
		t.Fatal(err)
	}
	if err := client.Kill(context.Background(), sandbox.ID); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(requests))
	}
	if requests[0].Method != http.MethodPost || requests[0].Path != "/sandboxes" {
		t.Fatalf("unexpected create request: %#v", requests[0])
	}
	if requests[0].Body["templateID"] != "template-1" || requests[0].Body["timeout"] != float64(90) {
		t.Fatalf("unexpected create body: %#v", requests[0].Body)
	}
	if requests[0].Body["allow_internet_access"] != true || requests[0].Body["secure"] != true {
		t.Fatalf("missing create security/network fields: %#v", requests[0].Body)
	}
	if requests[1].Path != "/sandboxes/sb-1/timeout" || requests[1].Body["timeout"] != float64(120) {
		t.Fatalf("unexpected timeout request: %#v", requests[1])
	}
	if requests[2].Method != http.MethodDelete || requests[2].Path != "/sandboxes/sb-1" {
		t.Fatalf("unexpected delete request: %#v", requests[2])
	}
}

func TestRecoveryControlPlaneMetadataListAndConnect(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("missing API key")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sandboxes":
			if got := r.URL.Query().Get("metadata"); got != "onlyboxes.schema_version=1&onlyboxes.session_id_hash=abc+123" {
				t.Errorf("unexpected metadata query: %q", got)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"sandboxID":   "sb-recover",
				"state":       "running",
				"envdVersion": "0.6.2",
				"metadata": map[string]string{
					"onlyboxes.schema_version":  "1",
					"onlyboxes.session_id_hash": "abc 123",
				},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/sb-recover/connect":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["timeout"] != float64(90) {
				t.Errorf("unexpected connect body: %#v err=%v", body, err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sandboxID":       "sb-recover",
				"envdVersion":     "0.6.2",
				"envdAccessToken": "fresh-token",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{APIKey: "test-key", APIURL: server.URL, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{
		"onlyboxes.session_id_hash": "abc 123",
		"onlyboxes.schema_version":  "1",
	}
	infos, err := client.List(context.Background(), metadata)
	if err != nil || len(infos) != 1 || infos[0].ID != "sb-recover" {
		t.Fatalf("unexpected list result: infos=%#v err=%v", infos, err)
	}
	sandbox, err := client.Connect(context.Background(), "sb-recover", 90)
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.ID != "sb-recover" || sandbox.AccessToken != "fresh-token" {
		t.Fatalf("unexpected connected sandbox: %#v", sandbox)
	}
}

func TestReadFileUsesSandboxRoutingHeadersAndLimit(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/files" || r.URL.Query().Get("path") != "/tmp/a b.txt" {
			t.Errorf("unexpected file URL: %s", r.URL.String())
		}
		if r.Header.Get("E2b-Sandbox-Id") != "sb-2" ||
			r.Header.Get("E2b-Sandbox-Port") != "49983" ||
			r.Header.Get("X-Access-Token") != "token-2" {
			t.Errorf("missing routing headers: %#v", r.Header)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "5")
		_, _ = io.WriteString(w, "hello")
	}))
	defer server.Close()
	client, err := NewClient(Config{
		APIKey:     "test-key",
		SandboxURL: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	sandbox := &Sandbox{ID: "sb-2", AccessToken: "token-2", Domain: "e2b.app"}
	file, err := client.ReadFile(context.Background(), sandbox, "/tmp/a b.txt", 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(file.Content) != "hello" || file.MIMEType != "text/plain" || file.Size != 5 {
		t.Fatalf("unexpected file: %#v", file)
	}
	_, err = client.ReadFile(context.Background(), sandbox, "/tmp/a b.txt", 4)
	if err == nil || !strings.Contains(err.Error(), ErrFileTooLarge.Error()) {
		t.Fatalf("expected file-too-large error, got %v", err)
	}
}

func TestEnvdResponseHeaderTimeout(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, "late")
	}))
	defer server.Close()
	client, err := NewClient(Config{
		APIKey:         "test-key",
		SandboxURL:     server.URL,
		RequestTimeout: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	startedAt := time.Now()
	_, err = client.ReadFile(ctx, &Sandbox{ID: "slow-headers"}, "/tmp/file", 1024)
	if err == nil {
		t.Fatal("expected envd response-header timeout")
	}
	if elapsed := time.Since(startedAt); elapsed > 120*time.Millisecond {
		t.Fatalf("envd response-header timeout took too long: %s", elapsed)
	}
}

func TestEnvdStreamCanOutliveResponseHeaderTimeout(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "5")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, "hello")
	}))
	defer server.Close()
	client, err := NewClient(Config{
		APIKey:         "test-key",
		SandboxURL:     server.URL,
		RequestTimeout: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	file, err := client.ReadFile(ctx, &Sandbox{ID: "slow-body"}, "/tmp/file", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(file.Content) != "hello" {
		t.Fatalf("unexpected streamed content %q", file.Content)
	}
}

func TestCreateSurfacesAPIErrorWithoutLeakingKey(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"invalid credentials"}`)
	}))
	defer server.Close()
	client, err := NewClient(Config{APIKey: "secret-key", APIURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Create(context.Background(), "template", 60)
	if err == nil || !strings.Contains(err.Error(), "invalid credentials") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("error leaked API key: %v", err)
	}
}

func TestRunUsesEnvdConnectJSONStream(t *testing.T) {
	t.Parallel()
	path, handler := processv1connect.NewProcessHandler(processTestHandler{t: t})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := NewClient(Config{APIKey: "test-key", SandboxURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Run(
		context.Background(),
		&Sandbox{ID: "sb-command", AccessToken: "command-token"},
		"printf ok",
		1024,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "ok" || result.Stderr != "" || result.ExitCode != 0 {
		t.Fatalf("unexpected command result: %#v", result)
	}
}

func TestRunStopsInterruptedProcess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		sendStart bool
		wantPID   uint32
		notFound  int32
	}{
		{
			name:      "by PID after start event",
			sendStart: true,
			wantPID:   42,
		},
		{
			name:      "by tag before start event",
			sendStart: false,
			notFound:  1,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			startTags := make(chan string, 1)
			signals := make(chan processSignalRecord, 1)
			var notFound atomic.Int32
			notFound.Store(tt.notFound)
			path, handler := processv1connect.NewProcessHandler(interruptedProcessTestHandler{
				sendStart: tt.sendStart,
				startTags: startTags,
				signals:   signals,
				notFound:  &notFound,
			})
			mux := http.NewServeMux()
			mux.Handle(path, handler)
			server := httptest.NewServer(mux)
			defer server.Close()

			client, err := NewClient(Config{APIKey: "test-key", SandboxURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			sandbox := &Sandbox{ID: "sb-interrupted", AccessToken: "interrupted-token"}
			_, err = client.Run(context.Background(), sandbox, "sleep 60", 1024)
			if err == nil || !strings.Contains(err.Error(), "output stream interrupted") {
				t.Fatalf("expected interrupted stream error, got %v", err)
			}

			startTag := <-startTags
			if strings.TrimSpace(startTag) == "" {
				t.Fatal("start request is missing a cleanup tag")
			}
			signal := <-signals
			if signal.signal != processv1.Signal_SIGNAL_SIGKILL {
				t.Fatalf("unexpected cleanup signal: %s", signal.signal)
			}
			if signal.pid != tt.wantPID {
				t.Fatalf("unexpected cleanup pid: got %d want %d", signal.pid, tt.wantPID)
			}
			if tt.wantPID == 0 && signal.tag != startTag {
				t.Fatalf("cleanup tag %q does not match start tag %q", signal.tag, startTag)
			}
			if tt.wantPID != 0 && signal.tag != "" {
				t.Fatalf("PID cleanup unexpectedly used tag %q", signal.tag)
			}
			if signal.sandboxID != sandbox.ID || signal.token != sandbox.AccessToken {
				t.Fatalf("cleanup is missing E2B routing headers: %#v", signal)
			}
		})
	}
}
