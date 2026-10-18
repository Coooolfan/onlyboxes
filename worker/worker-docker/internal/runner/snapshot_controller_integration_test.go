package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

func snapshotControllerCall(t *testing.T, c *snapshotController, scope, operation string, generation int64, action registryv1.SandboxPersistenceControl_Action, payload wire.Control) wire.Result {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	report := c.Handle(&registryv1.SandboxPersistenceControl{RequestId: uuid.NewString(), OperationId: operation, ScopedSessionId: scope, Generation: generation, Action: action, DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli(), PayloadJson: encoded})
	if report.Error != nil {
		t.Fatalf("action %s: %s", action, report.Error.Code)
	}
	var result wire.Result
	if json.Unmarshal(report.PayloadJson, &result) != nil {
		t.Fatal("invalid result")
	}
	return result
}

func TestIntegrationSnapshotControllerTransfersColdRestoreAndCache(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1 and DOCKER_SNAPSHOT_HELPER_IMAGE")
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
	cfg := terminalSessionManagerConfig{DockerImage: image, LeaseMinSec: 1, LeaseMaxSec: 600, LeaseDefaultSec: 300}
	manager := newTerminalSessionManager(cfg)
	defer manager.Close()
	journal := testSnapshotJournal(t)
	controllerCfg := snapshotControllerConfig{MaxInflight: 2, TmpQuotaBytes: 2 << 30, CacheMaxBytes: 512 << 20, CacheTTL: time.Hour, DrainTimeout: time.Second, TransferConcurrency: 2, TransferIdleTimeout: 5 * time.Second}
	controller, err := newSnapshotController(ctx, manager, journal, runtime, controllerCfg)
	if err != nil {
		t.Fatal(err)
	}
	scope := "obx:owner:controller-" + uuid.NewString()
	name := terminalSessionResourceName(scope)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "rm", "-f", name)
		state, _ := journal.Snapshot()
		for _, op := range state.Operations {
			if op.ImageID != "" {
				_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "image", "rm", op.ImageID)
			}
		}
	}()
	if _, err := manager.Execute(ctx, terminalExecRequest{SessionID: scope, CreateIfMissing: true, Command: "printf controller-data > /root/onlyboxes-controller.txt"}); err != nil {
		t.Fatal(err)
	}
	described := snapshotControllerCall(t, controller, scope, uuid.NewString(), 1, registryv1.SandboxPersistenceControl_INSPECT, wire.Control{})
	op := uuid.NewString()
	frozen := snapshotControllerCall(t, controller, scope, op, 1, registryv1.SandboxPersistenceControl_PREPARE_SNAPSHOT, wire.Control{Spec: described.Spec, MaxBytes: 256 << 20, MaxExpandedBytes: 1 << 30, DrainTimeoutSec: 1})
	if frozen.State != "frozen" || frozen.Archive == nil {
		t.Fatal("source not frozen")
	}
	if _, err := manager.Execute(ctx, terminalExecRequest{SessionID: scope, Generation: 1, Command: "must not execute"}); err == nil {
		t.Fatal("snapshot source admitted commands")
	}
	storeFile, err := os.Create(filepath.Join(t.TempDir(), "shared-archive"))
	if err != nil {
		t.Fatal(err)
	}
	defer storeFile.Close()
	var completed atomic.Bool
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			offset, e := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
			if e != nil {
				w.WriteHeader(400)
				return
			}
			data, e := io.ReadAll(io.LimitReader(r.Body, 8<<20+1))
			if e != nil || len(data) > 8<<20 {
				w.WriteHeader(400)
				return
			}
			if _, e = storeFile.WriteAt(data, offset); e != nil {
				w.WriteHeader(500)
				return
			}
			hash := sha256.Sum256(data)
			w.Header().Set("ETag", hex.EncodeToString(hash[:]))
			return
		}
		if r.Method == http.MethodGet && completed.Load() {
			gets.Add(1)
			http.ServeContent(w, r, "archive", time.Unix(1, 0), io.NewSectionReader(storeFile, 0, frozen.Archive.Size))
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	const partBytes int64 = 8 << 20
	for offset, number := int64(0), int32(1); offset < frozen.Archive.Size; {
		var parts []wire.UploadPart
		for len(parts) < 4 && offset < frozen.Archive.Size {
			size := min(partBytes, frozen.Archive.Size-offset)
			parts = append(parts, wire.UploadPart{Number: number, Grant: wire.TransferGrant{URL: server.URL + "/?private-signature=fixture-grant-secret-do-not-journal&offset=" + strconv.FormatInt(offset, 10), Method: http.MethodPut, ExpiresAt: time.Now().Add(time.Minute), Offset: offset, Size: size}})
			offset += size
			number++
		}
		result := snapshotControllerCall(t, controller, scope, op, 1, registryv1.SandboxPersistenceControl_UPLOAD_SNAPSHOT, wire.Control{Parts: parts})
		if len(result.Parts) != len(parts) {
			t.Fatal("missing uploaded part acknowledgements")
		}
	}
	if err := storeFile.Sync(); err != nil {
		t.Fatal(err)
	}
	completed.Store(true)
	grant := wire.TransferGrant{URL: server.URL + "/archive?private-signature=fixture-grant-secret-do-not-journal", Method: http.MethodGet, ExpiresAt: time.Now().Add(time.Minute), Size: frozen.Archive.Size}
	verified := snapshotControllerCall(t, controller, scope, op, 1, registryv1.SandboxPersistenceControl_VERIFY_SNAPSHOT, wire.Control{Archive: frozen.Archive, Download: &grant})
	if verified.State != "verified" {
		t.Fatal("remote content was not verified")
	}
	// No commit flag may authorize source removal.
	body, _ := json.Marshal(wire.Control{})
	denied := controller.Handle(&registryv1.SandboxPersistenceControl{RequestId: uuid.NewString(), OperationId: op, ScopedSessionId: scope, Generation: 1, Action: registryv1.SandboxPersistenceControl_FINALIZE_SUSPEND, DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli(), PayloadJson: body})
	if denied.Error == nil {
		t.Fatal("uncommitted source was removed")
	}
	finalized := snapshotControllerCall(t, controller, scope, op, 1, registryv1.SandboxPersistenceControl_FINALIZE_SUSPEND, wire.Control{SnapshotCommitted: true})
	if finalized.State != "finalized" {
		t.Fatal("source cleanup incomplete")
	}
	if id, err := snapshotContainerID(ctx, scope); err != nil || id != "" {
		t.Fatal("source survived committed cleanup", err)
	}
	wake := uuid.NewString()
	snapshotControllerCall(t, controller, scope, wake, 2, registryv1.SandboxPersistenceControl_PREPARE_RESTORE, wire.Control{Spec: frozen.Spec, Archive: frozen.Archive, LeaseTTLSec: 300})
	before := gets.Load()
	ready := snapshotControllerCall(t, controller, scope, wake, 2, registryv1.SandboxPersistenceControl_RESTORE, wire.Control{Archive: frozen.Archive, Download: &grant})
	if ready.State != "ready" || ready.InstanceID == described.InstanceID {
		t.Fatal("restore did not create a new ready instance")
	}
	if gets.Load() != before {
		t.Fatal("verified same-node cache did not avoid archive download")
	}
	if _, err := manager.Execute(ctx, terminalExecRequest{SessionID: scope, Generation: 2, Command: "must not execute"}); err == nil {
		t.Fatal("unactivated target admitted commands")
	}
	snapshotControllerCall(t, controller, scope, wake, 2, registryv1.SandboxPersistenceControl_ACTIVATE, wire.Control{})
	result, err := manager.Execute(ctx, terminalExecRequest{SessionID: scope, Generation: 2, Command: "cat /root/onlyboxes-controller.txt"})
	if err != nil || result.Stdout != "controller-data" || result.Created {
		t.Fatalf("restored data = %q, %v", result.Stdout, err)
	}
	state, _ := journal.Snapshot()
	encodedState, _ := json.Marshal(state)
	if strings.Contains(string(encodedState), "fixture-grant-secret-do-not-journal") {
		t.Fatal("journal retained an ephemeral transfer grant")
	}
	// A different controller/journal has no local cache. Restore the same
	// immutable bytes into a different stable session to exercise actual GET.
	coldManager := newTerminalSessionManager(cfg)
	defer coldManager.Close()
	coldRoot := t.TempDir()
	if err := os.Chmod(coldRoot, 0700); err != nil {
		t.Fatal(err)
	}
	coldJournal, err := openSnapshotJournal(coldRoot, "worker-b")
	if err != nil {
		t.Fatal(err)
	}
	defer coldJournal.Close()
	cold, err := newSnapshotController(ctx, coldManager, coldJournal, runtime, controllerCfg)
	if err != nil {
		t.Fatal(err)
	}
	snapshotControllerCall(t, controller, scope, wake, 2, registryv1.SandboxPersistenceControl_CANCEL, wire.Control{Delete: true})
	coldScope := scope
	coldName := terminalSessionResourceName(coldScope)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "rm", "-f", coldName)
		state, _ := coldJournal.Snapshot()
		for _, op := range state.Operations {
			if op.ImageID != "" {
				_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "image", "rm", op.ImageID)
			}
		}
	}()
	coldOp := uuid.NewString()
	snapshotControllerCall(t, cold, coldScope, coldOp, 3, registryv1.SandboxPersistenceControl_PREPARE_RESTORE, wire.Control{Spec: frozen.Spec, Archive: frozen.Archive, LeaseTTLSec: 300})
	before = gets.Load()
	snapshotControllerCall(t, cold, coldScope, coldOp, 3, registryv1.SandboxPersistenceControl_RESTORE, wire.Control{Archive: frozen.Archive, Download: &grant})
	if gets.Load() != before+1 {
		t.Fatal("cold controller skipped archive GET")
	}
	snapshotControllerCall(t, cold, coldScope, coldOp, 3, registryv1.SandboxPersistenceControl_CANCEL, wire.Control{})
	if id, err := snapshotContainerID(ctx, coldScope); err != nil || id != "" {
		t.Fatal("canceled target can still execute", err)
	}
}
