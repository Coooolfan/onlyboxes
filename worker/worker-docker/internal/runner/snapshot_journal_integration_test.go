package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

func TestIntegrationSnapshotJournalRestartProtectsFrozenSource(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1 and DOCKER_SNAPSHOT_HELPER_IMAGE")
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
	sessionID := "obx:owner:journal-" + uuid.NewString()
	name := terminalSessionResourceName(sessionID)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "rm", "-f", name)
	}()
	cfg := terminalSessionManagerConfig{DockerImage: image, LeaseMinSec: 1, LeaseMaxSec: 600, LeaseDefaultSec: 60}
	first := newTerminalSessionManager(cfg)
	defer first.Close()
	if _, err := first.Execute(ctx, terminalExecRequest{SessionID: sessionID, CreateIfMissing: true, Command: "printf protected-source > /root/onlyboxes-journal.txt"}); err != nil {
		t.Fatal(err)
	}
	instance, err := runtime.Describe(ctx, name, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	j := testSnapshotJournal(t)
	op := snapshotOperation{ID: uuid.NewString(), SessionID: sessionID, Generation: 1, Kind: "source", Phase: "preparing", Spec: instance.Spec, ContainerID: instance.ID, LeaseExpiresUnixMS: time.Now().Add(time.Minute).UnixMilli(), StagingBytes: 512 << 20}
	if err := first.HoldSource(op.SessionID, op.ID, op.Generation); err != nil {
		t.Fatal(err)
	}
	if err := j.Claim(op); err != nil {
		t.Fatal(err)
	}
	if err := first.DrainPersistence(ctx, op.SessionID, op.ID, op.Generation); err != nil {
		t.Fatal(err)
	}
	instance, err = runtime.Freeze(ctx, instance)
	if err != nil {
		t.Fatal(err)
	}
	archive, temp, err := runtime.Export(ctx, instance, op.ID, filepath.Join(j.root, "artifacts"), 512<<20, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	artifact := snapshotArtifactName(op.SessionID, op.ID, op.Generation)
	if err := os.Rename(temp, filepath.Join(j.root, "artifacts", artifact)); err != nil {
		t.Fatal(err)
	}
	if err := j.Update(op.ID, func(o *snapshotOperation) error {
		o.Phase = "frozen"
		o.Archive = &archive
		o.Artifact = artifact
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.InstallSnapshotJournal(j); err != nil {
		t.Fatal(err)
	}
	first.Close()
	_ = j.Close()
	reopened, err := openSnapshotJournal(j.root, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	second := newTerminalSessionManager(cfg)
	defer second.Close()
	if err := second.InstallSnapshotJournal(reopened); err != nil {
		t.Fatal(err)
	}
	hints := []*registryv1.SandboxPersistenceRecoveryCandidate{{ScopedSessionId: op.SessionID, OperationId: op.ID, Generation: op.Generation, Role: registryv1.SandboxPersistenceRecoveryCandidate_SOURCE}}
	if err := second.ProtectPersistenceRecovery(hints); err != nil {
		t.Fatal(err)
	}
	// Frozen sources are absent from the ordinary candidate list.
	second.Recover(ctx, nil)
	second.cleanupExpiredSessions()
	actual, err := runtime.Describe(ctx, name, sessionID)
	if err != nil || !actual.Paused || actual.ID != instance.ID {
		t.Fatalf("ordinary reconnect lost frozen source: paused=%t err=%v", actual.Paused, err)
	}
	if second.ActiveSessionCount() != 1 {
		t.Fatal("frozen backend was omitted from capacity")
	}
	if _, err := second.Execute(ctx, terminalExecRequest{SessionID: sessionID, Generation: 1, Command: "must-not-run", CreateIfMissing: true}); err == nil {
		t.Fatal("reconnect opened frozen source")
	}
	if err := reopened.Update(op.ID, func(o *snapshotOperation) error { o.Phase = "rolling_back"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Thaw(ctx, instance.ID); err != nil {
		t.Fatal(err)
	}
	lease := time.Now().Add(time.Minute).UnixMilli()
	if err := reopened.Update(op.ID, func(o *snapshotOperation) error { o.Phase = "rolled_back"; o.LeaseExpiresUnixMS = lease; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := second.InstallSnapshotJournal(reopened); err != nil {
		t.Fatal(err)
	}
	if err := second.ProtectPersistenceRecovery(nil); err != nil {
		t.Fatal(err)
	}
	results := second.Recover(ctx, []*registryv1.TerminalSessionRecoveryCandidate{{SessionId: sessionID, Generation: 1, LeaseExpiresUnixMs: lease}})
	if len(results) != 1 || results[0].Status != registryv1.TerminalSessionRecoveryResult_RECOVERED {
		t.Fatal("confirmed rollback did not restore existing instance", results)
	}
	result, err := second.Execute(ctx, terminalExecRequest{SessionID: sessionID, Generation: 1, Command: "cat /root/onlyboxes-journal.txt"})
	if err != nil || result.Created || result.Stdout != "protected-source" {
		t.Fatalf("rollback changed source data: %q %v", result.Stdout, err)
	}
}
