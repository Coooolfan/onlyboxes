package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

func TestIntegrationSnapshotRetiredImportsProtectLiveRootsAndRetryLateImages(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("requires DOCKER_INTEGRATION=1 and DOCKER_SNAPSHOT_HELPER_IMAGE")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	backend, err := newDockerSnapshotRuntime(ctx, os.Getenv("DOCKER_SNAPSHOT_HELPER_IMAGE"))
	if err != nil {
		t.Fatal(err)
	}
	manager := newTerminalSessionManager(terminalSessionManagerConfig{WorkerID: "worker-a", PersistenceEnabled: true})
	defer manager.Close()
	journal := testSnapshotJournal(t)
	cfg := snapshotControllerConfig{MaxInflight: 1, TmpQuotaBytes: 1 << 30, CacheMaxBytes: 1 << 20, CacheTTL: time.Hour, DrainTimeout: time.Second, TransferConcurrency: 1, TransferIdleTimeout: time.Second}
	c, err := newSnapshotController(ctx, manager, journal, backend, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if writer.WriteHeader(&tar.Header{Name: "data", Mode: 0600, Size: 1}) != nil {
		t.Fatal("archive header")
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	op := testSnapshotOperation()
	op.ID, op.SessionID = uuid.NewString(), "obx:owner:import-gc-"+uuid.NewString()
	op.Kind, op.Phase, op.ContainerID, op.LeaseExpiresUnixMS = "target", "prepared", "", 0
	op.Spec.RuntimeVersion, op.Spec.Platform = backend.version, backend.platform
	op.Spec.Command = []string{"/no-execution"}
	archive := transferTestArchive(buffer.Bytes())
	op.Archive = &archive
	op.Artifact = snapshotArtifactName(op.SessionID, op.ID, op.Generation)
	if err := os.WriteFile(c.artifact(op.Artifact), buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(c.artifact(op.Artifact))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	tag := c.imageTag(op)
	var imageIDs, containerIDs []string
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		for _, id := range containerIDs {
			_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "rm", "-f", id)
		}
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "image", "rm", tag)
		for _, id := range imageIDs {
			_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "image", "rm", id)
		}
	}()
	op.ImageID, err = backend.Import(ctx, file, archive, op.Spec, tag)
	if err != nil {
		t.Fatal(err)
	}
	imageIDs = append(imageIDs, op.ImageID)
	args, err := dockerSnapshotCreateArgs(terminalSessionResourceName(op.SessionID), op.SessionID, op.ImageID, op.ID, op.Generation, op.Spec, "")
	if err != nil {
		t.Fatal(err)
	}
	var created bytes.Buffer
	if err := dockerSnapshotCLI(ctx, nil, &created, args...); err != nil {
		t.Fatal(err)
	}
	op.ContainerID = strings.TrimSpace(created.String())
	containerIDs = append(containerIDs, op.ContainerID)
	if err := journal.Claim(op); err != nil {
		t.Fatal(err)
	}
	if err := journal.Update(op.ID, func(o *snapshotOperation) error { o.Phase = "importing"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := journal.Update(op.ID, func(o *snapshotOperation) error {
		o.Phase = "ready"
		o.Verified = true
		o.CacheUntilUnixMS = time.Now().Add(time.Hour).UnixMilli()
		o.LeaseExpiresUnixMS = time.Now().Add(time.Hour).UnixMilli()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Update(op.ID, func(o *snapshotOperation) error { o.Phase = "active"; return nil }); err != nil {
		t.Fatal(err)
	}
	next := make(map[string]time.Time)
	now := time.Now()
	state, _ := journal.Snapshot()
	if c.reconcileRetiredImports(ctx, state, now, next) != 0 {
		t.Fatal("active target was collected")
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "image", "inspect", op.ImageID); err != nil {
		t.Fatal("active image disappeared")
	}
	// A source commit alone is insufficient; actual FINALIZE removes the
	// exact old container and records the durable retired-root proof.
	source := testSnapshotOperation()
	source.ID, source.SessionID, source.Generation = uuid.NewString(), op.SessionID, op.Generation
	source.Spec, source.ContainerID, source.Artifact = op.Spec, op.ContainerID, snapshotArtifactName(op.SessionID, source.ID, op.Generation)
	if err := journal.Claim(source); err != nil {
		t.Fatal(err)
	}
	if err := journal.Update(source.ID, func(o *snapshotOperation) error {
		o.Phase = "frozen"
		o.Archive = &archive
		o.Verified = true
		o.RemoteVerified = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state, _ = journal.Snapshot()
	if c.reconcileRetiredImports(ctx, state, now, next) != 0 {
		t.Fatal("frozen source authorized image collection")
	}
	// Another stopped container still referencing the image must remain intact.
	var borrower bytes.Buffer
	if err := dockerSnapshotCLI(ctx, nil, &borrower, "create", "--name", "onlyboxes-import-gc-borrower-"+uuid.NewString(), "--entrypoint", "/no-execution", op.ImageID); err != nil {
		t.Fatal(err)
	}
	borrowerID := strings.TrimSpace(borrower.String())
	containerIDs = append(containerIDs, borrowerID)
	snapshotControllerCall(t, c, source.SessionID, source.ID, source.Generation, registryv1.SandboxPersistenceControl_FINALIZE_SUSPEND, wire.Control{SnapshotCommitted: true})
	state, _ = journal.Snapshot()
	if c.reconcileRetiredImports(ctx, state, now, next) != 1 {
		t.Fatal("finalized source did not authorize cleanup")
	}
	c.wg.Wait()
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "inspect", borrowerID); err != nil {
		t.Fatal("collector removed an unrelated container")
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "image", "inspect", op.ImageID); err != nil {
		t.Fatal("collector forced removal of an image still in use")
	}
	if c.reconcileRetiredImports(ctx, state, now, next) != 0 {
		t.Fatal("native failure caused unbounded immediate retries")
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "rm", borrowerID); err != nil {
		t.Fatal(err)
	}
	if c.reconcileRetiredImports(ctx, state, now.Add(time.Minute), next) != 1 {
		t.Fatal("failed image cleanup did not retry")
	}
	c.wg.Wait()
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "image", "inspect", op.ImageID); err == nil {
		t.Fatal("retired import image was retained")
	}
	if _, err := os.Stat(c.artifact(op.Artifact)); err != nil {
		t.Fatal("image GC removed a verified archive cache")
	}
	// A delayed import may reappear after success. Reopening the durable
	// journal supplies the same retirement proof without a new lifecycle action.
	late, err := backend.Import(ctx, file, archive, op.Spec, tag)
	if err != nil {
		t.Fatal(err)
	}
	imageIDs = append(imageIDs, late)
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err = openSnapshotJournal(journal.root, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	c, err = newSnapshotController(ctx, manager, journal, backend, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	state, _ = journal.Snapshot()
	maintenance, stopMaintenance := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); c.maintain(maintenance) }()
	defer func() { stopMaintenance(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := dockerSnapshotCLI(ctx, nil, io.Discard, "image", "inspect", late); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late import survived background maintenance after journal reopen")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if state.Operations[op.ID].Phase != "active" || state.Current[op.SessionID] != source.ID {
		t.Fatal("collector rewrote lifecycle identity")
	}
}
