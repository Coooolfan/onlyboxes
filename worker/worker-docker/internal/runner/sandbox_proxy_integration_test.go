package runner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/api/proxytoken"
)

func TestSandboxProxyLinuxDockerIntegration(t *testing.T) {
	if os.Getenv("ONLYBOXES_DOCKER_INTEGRATION") != "1" {
		t.Skip("set ONLYBOXES_DOCKER_INTEGRATION=1 to run the Docker integration test")
	}
	if runtime.GOOS != "linux" {
		t.Skip("public preview Docker integration requires Linux")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if err := ensureTerminalProxyNetwork(ctx); err != nil {
		t.Fatalf("ensure proxy network: %v", err)
	}

	originalRunDockerCommand := runDockerCommand
	var containerInspectCalls atomic.Int32
	runDockerCommand = func(ctx context.Context, args ...string) dockerCommandResult {
		if len(args) > 0 && args[0] == "inspect" {
			containerInspectCalls.Add(1)
		}
		return runDockerCommandCLI(ctx, args...)
	}
	t.Cleanup(func() { runDockerCommand = originalRunDockerCommand })

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      1,
		LeaseMaxSec:      60,
		LeaseDefaultSec:  30,
		OutputLimitBytes: 1024 * 1024,
		DockerImage:      "nginx:1.26-alpine",
		MemoryLimit:      "64m",
		CPULimit:         "0.5",
		PidsLimit:        32,
		DockerNetwork:    terminalProxyDockerNetwork,
	})
	t.Cleanup(manager.Close)

	created, err := manager.Execute(ctx, terminalExecRequest{
		Command: "printf 'docker-preview-ok\\n' >/usr/share/nginx/html/index.html && nginx",
	})
	if err != nil {
		t.Fatalf("create Docker session: %v", err)
	}
	if got := containerInspectCalls.Load(); got != 1 {
		t.Fatalf("expected one container IP inspect after startup, got %d", got)
	}

	proxyServer := httptest.NewServer(newProxyTestHandler(t, manager))
	defer proxyServer.Close()
	for requestNumber := 0; requestNumber < 2; requestNumber++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyServer.URL+"/", nil)
		if err != nil {
			t.Fatalf("create proxy request: %v", err)
		}
		request.Host = "preview.public-preview.example.com"
		request.Header.Set(
			proxytoken.HeaderName,
			signProxyTestToken(t, "http://127.0.0.1:80", created.SessionID, time.Now().Add(15*time.Second)),
		)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("proxy Docker request %d: %v", requestNumber+1, err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatalf("read Docker response %d: %v", requestNumber+1, readErr)
		}
		if response.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "docker-preview-ok" {
			t.Fatalf("unexpected Docker response %d: status=%d body=%q", requestNumber+1, response.StatusCode, body)
		}
	}
	if got := containerInspectCalls.Load(); got != 1 {
		t.Fatalf("proxy resource requests repeated docker inspect: %d calls", got)
	}

	target, err := manager.ResolveProxyTarget(ctx, created.SessionID, time.Now())
	if err != nil {
		t.Fatalf("resolve target for network isolation probe: %v", err)
	}
	isolationProbe := runDockerCommandCLI(
		ctx,
		"run", "--rm",
		"--network", terminalProxyDockerNetwork,
		"--entrypoint", "sh",
		"nginx:1.26-alpine",
		"-c",
		"command -v wget >/dev/null || exit 125; wget -T 2 -qO- http://"+target.IP+":80/",
	)
	if isolationProbe.Err != nil {
		t.Fatalf("run inter-container isolation probe: %v", isolationProbe.Err)
	}
	if isolationProbe.ExitCode == 125 {
		t.Fatalf("isolation probe image does not provide wget")
	}
	if isolationProbe.ExitCode == 0 {
		t.Fatalf("proxy Docker network allowed inter-container access to %s", target.IP)
	}
}
