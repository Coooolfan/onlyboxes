package runner

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/runtimelease"
)

// Creates only an auxiliary fixture. It does not start a Worker or advertise
// persistence capability; production helper ownership/startup is a separate
// launcher contract. The controller tests use the actual private RPC client.
func snapshotNativeLeaseFixture(t *testing.T, ctx context.Context, image, platform, worker string) *dockerSnapshotLeaseClient {
	t.Helper()
	parts := strings.Split(platform, "/")
	if len(parts) != 2 || parts[0] != "linux" {
		t.Fatal("fixture requires Linux Docker")
	}
	binary := filepath.Join(t.TempDir(), "runtime-lease-guard")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/runtime-lease-guard")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+parts[1])
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build auxiliary guard: %s %v", out, err)
	}
	name := "onlyboxes-controller-lease-" + uuid.NewString()
	volume := name + "-state"
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "rm", "-f", name)
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "volume", "rm", volume)
	})
	var id bytes.Buffer
	if err := dockerSnapshotCLI(ctx, nil, &id, "create", "--name", name, "--init", "--restart", "always", "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock", "--mount", "type=volume,source="+volume+",target=/state", "--entrypoint", "/runtime-lease-guard", image, "-worker-id", worker); err != nil {
		t.Fatal(err)
	}
	helper := strings.TrimSpace(id.String())
	client, err := newDockerSnapshotLeaseClient(helper)
	if err != nil {
		t.Fatal(err)
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "cp", binary, helper+":/runtime-lease-guard"); err != nil {
		t.Fatal(err)
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "start", helper); err != nil {
		t.Fatal(err)
	}
	// Do not arm any sandbox until restart supervision has stabilized.
	timer := time.NewTimer(11 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var reply runtimelease.Identity
	if client.rpc(ctx, runtimelease.Request{Action: "ping"}, &reply) != nil || !reply.Ready {
		t.Fatal("guard control readiness failed")
	}
	return client
}
