package runner

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

//go:embed testdata/snapshot-performance.py
var snapshotPerformanceScript string

type snapshotPerformanceResult struct {
	PayloadBytes    int64              `json:"payload_bytes"`
	Files           int                `json:"files"`
	ArchiveBytes    int64              `json:"archive_bytes"`
	ExpandedBudget  int64              `json:"expanded_budget_bytes"`
	ArchiveSHA256   string             `json:"archive_sha256"`
	PayloadSHA256   string             `json:"payload_sha256"`
	Seconds         map[string]float64 `json:"seconds"`
	ExportMiBPerSec float64            `json:"export_archive_mib_per_sec"`
	ImportMiBPerSec float64            `json:"import_archive_mib_per_sec"`
}

// This opt-in fixture measures backend stages, not Console/S3 latency. Every
// import creates a unique new image; it never evicts shared images or caches.
func TestIntegrationDockerSnapshotPerformance(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" || os.Getenv("DOCKER_SNAPSHOT_PERFORMANCE") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1 and DOCKER_SNAPSHOT_PERFORMANCE=1 for the native performance fixture")
	}
	readInt := func(key string, fallback int) int {
		t.Helper()
		if os.Getenv(key) == "" {
			return fallback
		}
		value, err := strconv.Atoi(os.Getenv(key))
		if err != nil {
			t.Fatalf("invalid %s", key)
		}
		return value
	}
	mib := readInt("DOCKER_SNAPSHOT_PERF_MIB", 1024)
	workers := readInt("DOCKER_SNAPSHOT_PERF_WORKERS", 1)
	files := readInt("DOCKER_SNAPSHOT_PERF_FILES", 1)
	if mib < 1 || mib > 10240 || (workers != 1 && workers != 4) || files < 1 || files > 65536 {
		t.Fatal("require 1..10240 MiB, 1 or 4 workers, and 1..65536 files")
	}
	image := os.Getenv("DOCKER_SNAPSHOT_HELPER_IMAGE")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	runtime, err := newDockerSnapshotRuntime(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	var imageInfo struct{ Size int64 }
	if err := dockerSnapshotJSON(ctx, &imageInfo, "image", "inspect", "--format", "{{json .}}", image); err != nil || imageInfo.Size <= 0 {
		t.Fatal("cannot determine local base image size", err)
	}
	payload := int64(mib) << 20
	// Check both filesystems: a remote daemon can have different capacity from
	// the CLI host. This is a conservative fixture guard, not a reservation.
	reserve := int64(workers)*6*(payload+imageInfo.Size+int64(files)*8192) + 2<<30
	root := t.TempDir()
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil || uint64(fs.Bavail)*uint64(fs.Bsize) < uint64(reserve) {
		t.Fatalf("archive filesystem needs at least %d free bytes for this fixture", reserve)
	}
	var space bytes.Buffer
	if err := dockerSnapshotCLI(ctx, nil, &space, "run", "--rm", "--network", "none", "--read-only", "--entrypoint", "stat", image, "-f", "-c", "%a %S", "/"); err != nil {
		t.Fatal("cannot determine daemon filesystem headroom", err)
	}
	parts := strings.Fields(space.String())
	if len(parts) != 2 {
		t.Fatal("invalid daemon filesystem headroom")
	}
	blocks, e1 := strconv.ParseUint(parts[0], 10, 64)
	blockSize, e2 := strconv.ParseUint(parts[1], 10, 64)
	if e1 != nil || e2 != nil || blockSize == 0 || blocks < uint64(reserve)/blockSize+1 {
		t.Fatalf("daemon filesystem needs at least %d free bytes for this fixture", reserve)
	}
	results := make([]snapshotPerformanceResult, workers)
	failures := make([]error, workers)
	var wg sync.WaitGroup
	started := time.Now()
	for index := range workers {
		directory := t.TempDir()
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], failures[index] = runSnapshotPerformance(ctx, runtime, image, directory, payload, imageInfo.Size, files)
		}(index)
	}
	wg.Wait()
	for index, err := range failures {
		if err != nil {
			t.Errorf("fixture %d failed: %v", index, err)
		}
	}
	if t.Failed() {
		return
	}
	report := struct {
		Runtime     string                      `json:"runtime_version"`
		Platform    string                      `json:"platform"`
		HelperImage string                      `json:"helper_image"`
		Workers     int                         `json:"concurrent_fixtures"`
		Headroom    int64                       `json:"required_headroom_bytes"`
		WallSeconds float64                     `json:"fixture_wall_seconds"`
		Results     []snapshotPerformanceResult `json:"results"`
	}{runtime.version, runtime.platform, image, workers, reserve, time.Since(started).Seconds(), results}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("snapshot_performance=%s", encoded)
}

func runSnapshotPerformance(ctx context.Context, runtime *dockerSnapshotRuntime, image, directory string, payload, baseBytes int64, files int) (result snapshotPerformanceResult, err error) {
	id := uuid.NewString()
	scope := "snapshot-performance:" + id
	name := terminalSessionResourceName(scope)
	tag := "onlyboxes-snapshot-performance:" + id
	result.PayloadBytes, result.Files = payload, files
	result.Seconds = map[string]float64{}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var listed bytes.Buffer
		if e := dockerSnapshotCLI(cleanup, nil, &listed, "ps", "-a", "--no-trunc", "--filter", "name=^/"+name+"$", "--format", "{{.ID}}"); e != nil {
			err = errors.Join(err, fmt.Errorf("container cleanup inspection: %w", e))
		} else if container := strings.TrimSpace(listed.String()); container != "" {
			err = errors.Join(err, runtime.Remove(cleanup, container))
		}
		listed.Reset()
		if e := dockerSnapshotCLI(cleanup, nil, &listed, "image", "ls", "--no-trunc", "--format", "{{.ID}}", tag); e != nil {
			err = errors.Join(err, fmt.Errorf("image cleanup inspection: %w", e))
		} else if listed.Len() > 0 {
			err = errors.Join(err, dockerSnapshotCLI(cleanup, nil, io.Discard, "image", "rm", tag))
		}
	}()
	stage := func(key string, fn func() error) error {
		start := time.Now()
		err := fn()
		result.Seconds[key] = time.Since(start).Seconds()
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return nil
	}
	if err = stage("prepare", func() error {
		if e := dockerSnapshotCLI(ctx, nil, io.Discard, terminalExecDockerCreateArgs(name, image, "512m", "1", 128)...); e != nil {
			return e
		}
		return dockerSnapshotCLI(ctx, nil, io.Discard, "start", name)
	}); err != nil {
		return
	}
	var before, after bytes.Buffer
	probe := func(action string, out io.Writer) error {
		return dockerSnapshotCLI(ctx, nil, out, "exec", name, "python3", "-c", snapshotPerformanceScript, action, strconv.FormatInt(payload, 10), strconv.Itoa(files))
	}
	if err = stage("seed", func() error { return probe("seed", &before) }); err != nil {
		return
	}
	instance, e := runtime.Describe(ctx, name, scope)
	if e != nil {
		return result, e
	}
	if err = stage("freeze", func() error { instance, e = runtime.Freeze(ctx, instance); return e }); err != nil {
		return
	}
	var fileName string
	var archive wire.Archive
	if err = stage("export", func() error {
		archive, fileName, e = runtime.Export(ctx, instance, id, directory, payload+baseBytes+int64(files)*4096+256<<20, payload+baseBytes+int64(files)*8192+512<<20)
		return e
	}); err != nil {
		return
	}
	defer func() { err = errors.Join(err, os.Remove(fileName)) }()
	result.ArchiveBytes, result.ExpandedBudget, result.ArchiveSHA256 = archive.Size, archive.ExpandedBytes, archive.SHA256
	file, e := os.Open(fileName)
	if e != nil {
		return result, e
	}
	defer file.Close()
	if err = stage("local_verify", func() error { return verifySnapshotFile(ctx, file, archive) }); err != nil {
		return
	}
	if err = stage("source_remove", func() error { return runtime.Remove(ctx, instance.ID) }); err != nil {
		return
	}
	var imported string
	if err = stage("import", func() error { imported, e = runtime.Import(ctx, file, archive, instance.Spec, tag); return e }); err != nil {
		return
	}
	if err = stage("create_start_ready", func() error {
		args, e := dockerSnapshotCreateArgs(name, scope, imported, "restore-"+id, 2, instance.Spec, "")
		if e != nil {
			return e
		}
		if e = dockerSnapshotCLI(ctx, nil, io.Discard, args...); e != nil {
			return e
		}
		if e = dockerSnapshotCLI(ctx, nil, io.Discard, "start", name); e != nil {
			return e
		}
		return dockerSnapshotCLI(ctx, nil, io.Discard, "exec", name, "python3", "-c", "pass")
	}); err != nil {
		return
	}
	if err = stage("content_verify", func() error { return probe("verify", &after) }); err != nil {
		return
	}
	if !bytes.Equal(before.Bytes(), after.Bytes()) {
		return result, errors.New("restored payload differs from seeded bytes/files/digest")
	}
	var content struct {
		SHA256 string `json:"sha256"`
	}
	if e := json.Unmarshal(after.Bytes(), &content); e != nil || len(content.SHA256) != 64 {
		return result, errors.New("invalid restored payload digest")
	}
	result.PayloadSHA256 = content.SHA256
	result.ExportMiBPerSec = float64(archive.Size) / (1 << 20) / result.Seconds["export"]
	result.ImportMiBPerSec = float64(archive.Size) / (1 << 20) / result.Seconds["import"]
	return
}
