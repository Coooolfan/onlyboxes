package runtimelease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shortDir(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "obx-lease-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func TestDockerRequiresFullIdentityAndNeverRemovesOrUnpauses(t *testing.T) {
	for _, mode := range []string{"owned", "old-api", "foreign-worker", "foreign-scope", "foreign-id", "foreign-generation", "auto-restart", "redirect", "daemon-error"} {
		t.Run(mode, func(t *testing.T) {
			e := lease("a")
			scope := sha256.Sum256([]byte(e.Scope))
			calls := []string{}
			running := true
			socket := filepath.Join(shortDir(t), "docker.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.RequestURI())
				if r.URL.Path == "/version" {
					version := "1.54"
					if mode == "old-api" {
						version = "1.41"
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"ApiVersion": version})
					return
				}
				if mode == "redirect" {
					http.Redirect(w, r, "http://127.0.0.1:1/private", 307)
					return
				}
				if mode == "daemon-error" {
					http.Error(w, "private daemon/environment diagnostics", 500)
					return
				}
				if r.Method == http.MethodPost {
					if r.URL.Path != "/v1.54/containers/"+e.ContainerID+"/stop" || (r.URL.Query().Get("t") != "0" || r.URL.Query().Get("signal") != "SIGKILL") {
						t.Errorf("unsafe native operation %s", r.URL)
					}
					running = false
					w.WriteHeader(204)
					return
				}
				labels := map[string]string{"onlyboxes.managed": "true", "onlyboxes.worker_id": "worker-test", "onlyboxes.capability": "terminalExec", "onlyboxes.session_id_hash": hex.EncodeToString(scope[:])}
				id := e.ContainerID
				restart := "no"
				switch mode {
				case "foreign-worker":
					labels["onlyboxes.worker_id"] = "other"
				case "foreign-scope":
					labels["onlyboxes.session_id_hash"] = strings.Repeat("f", 64)
				case "foreign-generation":
					labels["onlyboxes.generation"] = "2"
				case "foreign-id":
					id = strings.Repeat("b", 64)
				case "auto-restart":
					restart = "always"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "State": map[string]any{"Running": running, "Paused": running}, "Config": map[string]any{"Labels": labels}, "HostConfig": map[string]any{"RestartPolicy": map[string]any{"Name": restart}}})
			}))
			server.Listener.Close()
			server.Listener = listener
			server.Start()
			defer server.Close()
			backend, err := NewDocker(context.Background(), socket, "worker-test")
			if mode == "old-api" {
				if err == nil {
					t.Fatal("legacy API cannot guarantee signal override")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			err = backend.Stop(context.Background(), e)
			if mode == "owned" {
				if err != nil || running {
					t.Fatal("owned native execution was not stopped", err)
				}
			} else {
				if err == nil || !running {
					t.Fatal("unproven identity reached native stop", err)
				}
				if strings.Contains(err.Error(), "private") {
					t.Fatal("daemon secret leaked")
				}
			}
			for _, call := range calls {
				if strings.HasPrefix(call, "DELETE") || strings.Contains(call, "unpause") {
					t.Fatal("guard changed or removed retained writable layer", call)
				}
			}
		})
	}
}

func TestUnixControlsRejectDuplicateListenerAndShutdownStopsRetainedRoots(t *testing.T) {
	b := backend()
	g := openTest(t, b)
	socket := filepath.Join(shortDir(t), "control.sock")
	listener, err := Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Listen(socket); err == nil {
		other.Close()
		t.Fatal("second listener hijacked active control socket")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Serve(ctx, listener) }()
	e := lease("a")
	b.mu.Lock()
	b.running[e.ContainerID] = true
	b.mu.Unlock()
	if _, err := Call(context.Background(), socket, Request{Action: "ping"}); err != nil {
		cancel()
		t.Fatal("readiness did not respond", err)
	}
	if got, err := Call(context.Background(), socket, Request{Action: "arm", Entry: e}); err != nil || got != e {
		cancel()
		t.Fatal("control acknowledgement lost identity", got, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control server did not join shutdown")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running[e.ContainerID] || b.stops[e.ContainerID] == 0 {
		t.Fatal("guardian clean shutdown left root writable")
	}
}
