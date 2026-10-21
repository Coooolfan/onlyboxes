package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func snapshotTestDaemonArch(t *testing.T, ctx context.Context) string {
	t.Helper()
	var server struct{ Arch string }
	if err := dockerSnapshotJSON(ctx, &server, "version", "--format", "{{json .Server}}"); err != nil {
		t.Fatal(err)
	}
	return server.Arch
}

func snapshotCleanupLaunchedGuard(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		data, err := os.ReadFile(filepath.Join(root, "journal.json"))
		if err != nil {
			return
		}
		var state snapshotJournalState
		if json.Unmarshal(data, &state) != nil || state.LeaseGuard == nil || !state.LeaseGuard.valid(state.WorkerID) {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "rm", "-f", state.LeaseGuard.HelperName)
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "volume", "rm", state.LeaseGuard.VolumeName)
	})
}

// Builds only the static auxiliary helper and imports a uniquely tagged image
// containing that trusted binary. No Worker service is started by this fixture.
func snapshotGuardImageFixture(t *testing.T, ctx context.Context, platform string) string {
	t.Helper()
	parts := strings.Split(platform, "/")
	if len(parts) != 2 || parts[0] != "linux" {
		t.Fatal("Linux Docker required")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "runtime-lease-guard")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/runtime-lease-guard")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+parts[1])
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("auxiliary build failed: %s %v", out, err)
	}
	file, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.Create(filepath.Join(root, "image.tar"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	writer := tar.NewWriter(archive)
	if writer.WriteHeader(&tar.Header{Name: "runtime-lease-guard", Mode: 0755, Size: stat.Size(), Typeflag: tar.TypeReg}) != nil {
		t.Fatal("image header")
	}
	if _, err := io.Copy(writer, file); err != nil {
		t.Fatal(err)
	}
	if writer.Close() != nil {
		t.Fatal("image archive")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	tag := "onlyboxes-lease-startup-fixture:" + uuid.NewString()
	if err := dockerSnapshotCLI(ctx, archive, &output, "import", "--change", `ENTRYPOINT ["/runtime-lease-guard"]`, "--change", "USER 0:0", "--change", "LABEL onlyboxes.component=runtime-lease-guard onlyboxes.runtime-lease-guard.schema=2", "-", tag); err != nil {
		t.Fatal(err)
	}
	image := strings.TrimSpace(output.String())
	if !dockerSnapshotDigest.MatchString(image) {
		t.Fatal("invalid helper image identity")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "image", "rm", tag)
	})
	return image
}

func TestIntegrationSnapshotLeaseGuardStartupPinsResourcesAndRejectsLoss(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var server struct{ Os, Arch string }
	if err := dockerSnapshotJSON(ctx, &server, "version", "--format", "{{json .Server}}"); err != nil {
		t.Fatal(err)
	}
	platform := server.Os + "/" + server.Arch
	image := snapshotGuardImageFixture(t, ctx, platform)
	j := testSnapshotJournal(t)
	defer func() {
		state, _ := j.Snapshot()
		if state.LeaseGuard != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "rm", "-f", state.LeaseGuard.HelperName)
			_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "volume", "rm", state.LeaseGuard.VolumeName)
		}
	}()
	client, err := startSnapshotLeaseGuard(ctx, j, image, "/var/run/docker.sock", platform)
	if err != nil {
		state, _ := j.Snapshot()
		if state.LeaseGuard != nil {
			var n leaseGuardNative
			_ = dockerSnapshotJSON(ctx, &n, "inspect", "--format", "{{json .}}", state.LeaseGuard.HelperName)
			t.Logf("fixture settings: init=%v readonly=%t caps=%v security=%v tmpfs=%v mounts=%v", n.HostConfig.Init, n.HostConfig.ReadonlyRootfs, n.HostConfig.CapDrop, n.HostConfig.SecurityOpt, n.HostConfig.Tmpfs, n.Mounts)
		}
		t.Fatal("launcher", err)
	}
	state, _ := j.Snapshot()
	binding := *state.LeaseGuard
	if client.helperID != binding.HelperID || binding.VolumeCreated == "" {
		t.Fatal("launcher returned before binding identities")
	}
	if binding.StateID == "" {
		t.Fatal("launcher did not bind the store identity")
	}
	// A matching restart reuses the native ID and state volume, never allocates
	// an empty replacement or silently changes the image/daemon binding.
	second, err := startSnapshotLeaseGuard(ctx, j, image, "/var/run/docker.sock", platform)
	if err != nil || second.helperID != client.helperID {
		t.Fatal("warm helper reuse failed", err)
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "update", "--restart", "no", binding.HelperID); err != nil {
		t.Fatal(err)
	}
	if _, err := startSnapshotLeaseGuard(ctx, j, image, "/var/run/docker.sock", platform); err == nil {
		t.Fatal("helper without restart supervision was accepted")
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "update", "--restart", "always", binding.HelperID); err != nil {
		t.Fatal(err)
	}
	// Simulate a journal referring to the former contents of the same volume
	// object. Readiness must not treat a fresh store ID as the old lease ledger.
	if err := j.transaction(func(s *snapshotJournalState) error { s.LeaseGuard.StateID = strings.Repeat("f", 32); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := startSnapshotLeaseGuard(ctx, j, image, "/var/run/docker.sock", platform); err == nil {
		t.Fatal("same volume object hid a different lease store")
	}
	if err := j.transaction(func(s *snapshotJournalState) error { s.LeaseGuard.StateID = binding.StateID; return nil }); err != nil {
		t.Fatal(err)
	}
	foreignRoot := t.TempDir()
	if os.Chmod(foreignRoot, 0700) != nil {
		t.Fatal("private fixture")
	}
	foreign, err := openSnapshotJournal(foreignRoot, state.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if _, err := startSnapshotLeaseGuard(ctx, foreign, image, "/var/run/docker.sock", platform); err == nil {
		t.Fatal("lost journal silently adopted existing guardian resources")
	}
	other, _ := foreign.Snapshot()
	if other.LeaseGuard != nil {
		t.Fatal("unknown resources created a new binding")
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "rm", "-f", binding.HelperID); err != nil {
		t.Fatal(err)
	}
	if _, err := startSnapshotLeaseGuard(ctx, j, image, "/var/run/docker.sock", platform); err == nil {
		t.Fatal("missing pinned guardian was silently replaced")
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "volume", "rm", binding.VolumeName); err != nil {
		t.Fatal(err)
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "volume", "create", "--label", "onlyboxes.component="+leaseGuardComponent, "--label", "onlyboxes.worker_id="+state.WorkerID, "--label", "onlyboxes.lease_binding="+binding.Token, binding.VolumeName); err != nil {
		t.Fatal(err)
	}
	if _, err := startSnapshotLeaseGuard(ctx, j, image, "/var/run/docker.sock", platform); err == nil {
		t.Fatal("same-name replacement volume replaced durable lease state")
	}
	after, _ := j.Snapshot()
	if *after.LeaseGuard != binding {
		t.Fatal("failed startup rewrote pinned ownership")
	}
	t.Log("private read-only helper startup, daemon identity, continuous supervision readiness, warm reuse and resource-loss rejection verified")
}
