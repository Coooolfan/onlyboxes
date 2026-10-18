package runner

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

//go:embed testdata/snapshot-probe.py
var snapshotProbeScript string

func TestIntegrationDockerSnapshotRootFSMetadataAndColdImport(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1 and DOCKER_SNAPSHOT_HELPER_IMAGE to a pinned GNU tar image")
	}
	image := os.Getenv("DOCKER_SNAPSHOT_HELPER_IMAGE")
	if image == "" {
		t.Fatal("DOCKER_SNAPSHOT_HELPER_IMAGE is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	runtime, err := newDockerSnapshotRuntime(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	sourceID, targetID := "snapshot-session:"+id, "snapshot-session:"+id
	sourceName, targetName := terminalSessionResourceName(sourceID), terminalSessionResourceName(targetID)
	imageTag := "onlyboxes-snapshot-integration:" + id
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "rm", "-f", sourceName, targetName)
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "image", "rm", imageTag)
	}()
	create := terminalExecDockerCreateArgs(sourceName, image, "256m", "0.5", 128)
	// Use a custom environment and workdir to verify that import reconstructs
	// container configuration rather than silently using the target defaults.
	create = append(create[:1], append([]string{"--user", "1234:2345", "--env", "SNAPSHOT_SETTING=retained", "--workdir", "/workspace"}, create[1:]...)...)
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, create...); err != nil {
		t.Fatal(err)
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "start", sourceName); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"seed", "mutate"} {
		if err := dockerSnapshotCLI(ctx, nil, io.Discard, "exec", "--user", "0", sourceName, "python3", "-c", snapshotProbeScript, stage); err != nil {
			t.Fatal(stage, err)
		}
	}
	before := snapshotProbe(t, ctx, sourceName)
	delete(before, "tmp_present")
	instance, err := runtime.Describe(ctx, sourceName, sourceID)
	if err != nil {
		t.Fatal("describe", err)
	}
	if instance.Spec.CPUs != .5 || instance.Spec.Env["SNAPSHOT_SETTING"] != "retained" || instance.Spec.WorkingDir != "/workspace" || instance.Spec.User != "1234:2345" {
		t.Fatalf("incorrect resources/environment/cwd: CPUs=%v cwd=%q", instance.Spec.CPUs, instance.Spec.WorkingDir)
	}
	instance, err = runtime.Freeze(ctx, instance)
	if err != nil {
		t.Fatal("freeze", err)
	}
	exportStarted := time.Now()
	archive, fileName, err := runtime.Export(ctx, instance, id, t.TempDir(), 512<<20, 1<<30)
	if err != nil {
		t.Fatal("export", err)
	}
	exportElapsed := time.Since(exportStarted)
	file, err := os.Open(fileName)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	// Once the archive is durable, hide/remove all source runtime resources.
	// Import receives only that file and the recorded restore specification.
	if err := runtime.Remove(ctx, instance.ID); err != nil {
		t.Fatal(err)
	}
	importStarted := time.Now()
	imageID, err := runtime.Import(ctx, file, archive, instance.Spec, imageTag)
	if err != nil {
		t.Fatal("import", err)
	}
	importElapsed := time.Since(importStarted)
	args, err := dockerSnapshotCreateArgs(targetName, targetID, imageID, "restore-"+id, 2, instance.Spec, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, args...); err != nil {
		t.Fatal(err)
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "start", targetName); err != nil {
		t.Fatal(err)
	}
	// A stale cleanup for the old immutable ID must not delete the new instance
	// even though it reuses the exact same stable session/container name.
	if err := runtime.Remove(ctx, instance.ID); err != nil {
		t.Fatal("idempotent old-generation cleanup", err)
	}
	after := snapshotProbe(t, ctx, targetName)
	if after["tmp_present"] != false {
		t.Fatal("temporary filesystem content was persisted")
	}
	delete(after, "tmp_present")
	sourceSparseBlocks, restoredSparseBlocks := before["sparse_blocks"].(float64), after["sparse_blocks"].(float64)
	if restoredSparseBlocks*512 > float64(archive.ExpandedBytes) {
		t.Fatal("sparse file allocation exceeded reserved expanded budget")
	}
	t.Logf("sparse physical blocks: source=%v restored=%v (512-byte blocks)", sourceSparseBlocks, restoredSparseBlocks)
	delete(before, "sparse_blocks")
	delete(after, "sparse_blocks")
	left, _ := json.MarshalIndent(before, "", "  ")
	right, _ := json.MarshalIndent(after, "", "  ")
	if !bytes.Equal(left, right) {
		t.Fatalf("root filesystem changed:\nsource=%s\nrestored=%s", left, right)
	}
	var configuration bytes.Buffer
	if err := dockerSnapshotCLI(ctx, nil, &configuration, "exec", targetName, "sh", "-c", `printf '%s:%s' "$SNAPSHOT_SETTING" "$PWD"`); err != nil || configuration.String() != "retained:/workspace" {
		t.Fatalf("restored configuration = %q, %v", configuration.String(), err)
	}
	var user bytes.Buffer
	if err := dockerSnapshotCLI(ctx, nil, &user, "exec", targetName, "id", "-u"); err != nil || strings.TrimSpace(user.String()) != "1234" {
		t.Fatalf("restored execution user = %q, %v", user.String(), err)
	}
	if _, err := runtime.DescribePersistent(ctx, targetName, targetID, instance.Spec); err != nil {
		t.Fatal("restored session cannot produce its next snapshot", err)
	}
	t.Logf("runtime=%s platform=%s archive_bytes=%d expanded_budget_bytes=%d export=%s import=%s sha256=%s", runtime.version, runtime.platform, archive.Size, archive.ExpandedBytes, exportElapsed, importElapsed, archive.SHA256)
}

func snapshotProbe(t *testing.T, ctx context.Context, name string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := dockerSnapshotCLI(ctx, nil, &out, "exec", "--user", "0", name, "python3", "-c", snapshotProbeScript, "verify"); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if json.Unmarshal(out.Bytes(), &result) != nil {
		t.Fatal("invalid filesystem probe result")
	}
	return result
}

func TestIntegrationDockerSnapshotExportQuotaRetainsFrozenSource(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1 to run against Docker")
	}
	image := os.Getenv("DOCKER_SNAPSHOT_HELPER_IMAGE")
	if image == "" {
		t.Fatal("DOCKER_SNAPSHOT_HELPER_IMAGE is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	runtime, err := newDockerSnapshotRuntime(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := fmt.Sprintf("snapshot-quota:%s", uuid.NewString())
	name := terminalSessionResourceName(sessionID)
	defer func() { _ = dockerSnapshotCLI(context.Background(), nil, io.Discard, "rm", "-f", name) }()
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, terminalExecDockerCreateArgs(name, image, "256m", "1", 128)...); err != nil {
		t.Fatal(err)
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "start", name); err != nil {
		t.Fatal(err)
	}
	instance, err := runtime.Describe(ctx, name, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	instance, err = runtime.Freeze(ctx, instance)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	_, _, err = runtime.Export(ctx, instance, uuid.NewString(), directory, 1024, 1<<30)
	if err != errSnapshotArchiveQuota {
		t.Fatalf("quota error = %v", err)
	}
	var state struct{ Paused bool }
	if err := dockerSnapshotJSON(ctx, &state, "inspect", "--format", "{{json .State}}", instance.ID); err != nil || !state.Paused {
		t.Fatalf("failed export lost frozen source: paused=%t err=%v", state.Paused, err)
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 0 {
		t.Fatalf("failed export retained incomplete archive: %v %v", files, err)
	}
	if err := runtime.Thaw(ctx, instance.ID); err != nil {
		t.Fatal("rollback", err)
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "exec", name, "python3", "-c", "import socket; s=socket.socket(socket.AF_UNIX); s.bind('/socket-not-persistable')"); err != nil {
		t.Fatal(err)
	}
	instance.Paused, instance.State = false, "running"
	instance, err = runtime.Freeze(ctx, instance)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = runtime.Export(ctx, instance, uuid.NewString(), directory, 512<<20, 1<<30)
	if err != errSnapshotRuntimeUnsupported {
		t.Fatalf("socket was silently lost instead of rejecting export: %v", err)
	}
}
