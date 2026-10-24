package grpcserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
)

// These are protocol actors, not runtime snapshot adapters. Archive bytes go
// through real HTTP transfers; SQLite and registry dispatch are the real code.
type coordinatorObjects struct {
	mu              sync.Mutex
	server          *httptest.Server
	base            *objectstore.SnapshotStore
	parts           map[string]map[int32][]byte
	archives        map[string][]byte
	manifests       map[string]json.RawMessage
	corrupt         bool
	recoverComplete bool
	workerGets      int
}

func newCoordinatorObjects(t *testing.T) *coordinatorObjects {
	t.Helper()
	o := &coordinatorObjects{parts: make(map[string]map[int32][]byte), archives: make(map[string][]byte), manifests: make(map[string]json.RawMessage)}
	o.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pieces := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(pieces) < 2 || r.Header.Get("X-Test-Worker") == "" {
			http.Error(w, "only worker archive transfers are allowed", http.StatusForbidden)
			return
		}
		id := pieces[1]
		if r.Method == http.MethodPut && pieces[0] == "upload" && len(pieces) == 3 {
			part, e := strconv.Atoi(pieces[2])
			if e != nil {
				w.WriteHeader(400)
				return
			}
			data, e := io.ReadAll(io.LimitReader(r.Body, 6<<20))
			if e != nil {
				w.WriteHeader(500)
				return
			}
			o.mu.Lock()
			if o.parts[id] == nil {
				o.parts[id] = make(map[int32][]byte)
			}
			o.parts[id][int32(part)] = data
			o.mu.Unlock()
			w.Header().Set("ETag", fmt.Sprintf("part-%d", part))
			w.WriteHeader(200)
			return
		}
		if r.Method == http.MethodGet && pieces[0] == "archive" {
			o.mu.Lock()
			data := bytes.Clone(o.archives[id])
			corrupt := o.corrupt
			o.workerGets++
			o.mu.Unlock()
			if len(data) == 0 {
				w.WriteHeader(404)
				return
			}
			if corrupt {
				data[0] ^= 1
			}
			_, _ = w.Write(data)
			return
		}
		w.WriteHeader(400)
	}))
	t.Cleanup(o.server.Close)
	var err error
	o.base, err = objectstore.NewSnapshotStore(objectstore.SnapshotConfig{Config: objectstore.Config{Endpoint: o.server.URL, Region: "test", BucketName: "snapshots", AccessKey: "ak", SecretKey: "sk"}, Prefix: "snapshots", MaxBytes: 16 << 20, PartSize: 5 << 20, PresignTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return o
}
func (o *coordinatorObjects) Object(owner, id, snapshot string, size int64, sha string) (objectstore.SnapshotObject, error) {
	return o.base.Object(owner, id, snapshot, size, sha)
}
func (o *coordinatorObjects) BeginUpload(_ context.Context, object objectstore.SnapshotObject) (objectstore.SnapshotUpload, error) {
	return objectstore.SnapshotUpload{Object: object, UploadID: object.SnapshotID, PartSize: 5 << 20}, nil
}
func (o *coordinatorObjects) PresignPart(_ context.Context, u objectstore.SnapshotUpload, n int32) (objectstore.TransferGrant, error) {
	size := u.PartSize
	if tail := u.Object.Size - int64(n-1)*u.PartSize; tail < size {
		size = tail
	}
	return objectstore.TransferGrant{URL: fmt.Sprintf("%s/upload/%s/%d", o.server.URL, u.Object.SnapshotID, n), Method: "PUT", Headers: make(http.Header), ExpiresAt: time.Now().Add(time.Minute), Offset: int64(n-1) * u.PartSize, Size: size}, nil
}
func (o *coordinatorObjects) PresignSnapshotDownload(_ context.Context, object objectstore.SnapshotObject, _ int64) (objectstore.TransferGrant, error) {
	return objectstore.TransferGrant{URL: o.server.URL + "/archive/" + object.SnapshotID, Method: "GET", Headers: make(http.Header), ExpiresAt: time.Now().Add(time.Minute), Size: object.Size}, nil
}
func (o *coordinatorObjects) CompleteUploadWithVerifier(ctx context.Context, u objectstore.SnapshotUpload, parts []objectstore.SnapshotPart, verify func(context.Context, objectstore.SnapshotObject) error) (objectstore.SnapshotObject, error) {
	o.mu.Lock()
	var archive []byte
	for _, part := range parts {
		data := o.parts[u.Object.SnapshotID][part.Number]
		if int64(len(data)) != part.Size || part.ETag != fmt.Sprintf("part-%d", part.Number) {
			o.mu.Unlock()
			return objectstore.SnapshotObject{}, errors.New("invalid uploaded parts")
		}
		archive = append(archive, data...)
	}
	o.archives[u.Object.SnapshotID] = archive
	recover := o.recoverComplete
	o.mu.Unlock()
	object := u.Object
	object.VersionID = "archive-v1"
	if recover {
		if err := verify(ctx, object); err != nil {
			return objectstore.SnapshotObject{}, err
		}
	}
	return object, nil
}
func (o *coordinatorObjects) PutSnapshotManifest(_ context.Context, object objectstore.SnapshotObject, payload json.RawMessage) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.manifests[object.SnapshotID] = bytes.Clone(payload)
	return "manifest-v1", nil
}
func (o *coordinatorObjects) AbortSnapshotUpload(context.Context, objectstore.SnapshotUpload) error {
	return nil
}

type coordinatorActor struct {
	mu           sync.Mutex
	session      *activeSession
	data         []byte
	spec         wire.RestoreSpec
	states       map[string]string
	counts       map[registryv1.SandboxPersistenceControl_Action]int
	blocks       map[registryv1.SandboxPersistenceControl_Action]<-chan struct{}
	restoreError bool
	refuseCancel bool
	boots        int
	lease        int64
}

func startCoordinatorActor(t *testing.T, session *activeSession, data []byte) *coordinatorActor {
	t.Helper()
	session.persistence.mu.Lock()
	session.persistence.availableBytes = 64 << 20
	session.persistence.mu.Unlock()
	a := &coordinatorActor{session: session, data: data, spec: snapshotTestSpec(), states: make(map[string]string), counts: make(map[registryv1.SandboxPersistenceControl_Action]int), blocks: make(map[registryv1.SandboxPersistenceControl_Action]<-chan struct{}), lease: time.Now().Add(time.Hour).UnixMilli()}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-session.controlOutbound:
				control := message.GetSandboxPersistenceControl()
				if control == nil {
					continue
				}
				go func() {
					var payload wire.Control
					if json.Unmarshal(control.PayloadJson, &payload) != nil {
						return
					}
					result, code := a.apply(control, payload)
					a.mu.Lock()
					block := a.blocks[control.Action]
					a.mu.Unlock()
					if block != nil {
						select {
						case <-block:
						case <-ctx.Done():
							return
						}
					}
					encoded, _ := json.Marshal(result)
					report := &registryv1.SandboxPersistenceReport{RequestId: control.RequestId, OperationId: control.OperationId, ScopedSessionId: control.ScopedSessionId, Generation: control.Generation, Action: control.Action, Completed: true, PayloadJson: encoded}
					if code != "" {
						report.PayloadJson = nil
						report.Error = &registryv1.CommandError{Code: code, Message: "worker action failed"}
					}
					_ = session.persistence.resolve(report)
				}()
			}
		}
	}()
	return a
}

func (a *coordinatorActor) apply(c *registryv1.SandboxPersistenceControl, p wire.Control) (wire.Result, string) {
	a.mu.Lock()
	a.counts[c.Action]++
	state := a.states[c.OperationId]
	a.mu.Unlock()
	result := wire.Result{State: state, Spec: &a.spec, LeaseExpiresUnixMS: a.lease, InstanceID: "instance-" + c.OperationId}
	switch c.Action {
	case registryv1.SandboxPersistenceControl_INSPECT:
		if state == "" {
			result.State = "running"
		}
	case registryv1.SandboxPersistenceControl_PREPARE_SNAPSHOT:
		hash := sha256.Sum256(a.data)
		result.State = "frozen"
		result.Archive = &wire.Archive{SnapshotID: c.OperationId, Size: int64(len(a.data)), SHA256: hex.EncodeToString(hash[:]), ExpandedBytes: int64(len(a.data)), Format: a.spec.Format}
	case registryv1.SandboxPersistenceControl_UPLOAD_SNAPSHOT:
		result.State = "frozen"
		for _, part := range p.Parts {
			if part.Grant.Offset < 0 || part.Grant.Size < 1 || part.Grant.Offset+part.Grant.Size > int64(len(a.data)) {
				return result, "snapshot_integrity_error"
			}
			request, _ := http.NewRequest(part.Grant.Method, part.Grant.URL, bytes.NewReader(a.data[part.Grant.Offset:part.Grant.Offset+part.Grant.Size]))
			request.Header.Set("X-Test-Worker", a.session.nodeID)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				return result, "snapshot_storage_error"
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 200 {
				return result, "snapshot_storage_error"
			}
			result.Parts = append(result.Parts, wire.CompletedPart{Number: part.Number, Size: part.Grant.Size, ETag: response.Header.Get("ETag")})
		}
	case registryv1.SandboxPersistenceControl_VERIFY_SNAPSHOT, registryv1.SandboxPersistenceControl_RESTORE:
		a.mu.Lock()
		fail := a.restoreError
		a.mu.Unlock()
		if c.Action == registryv1.SandboxPersistenceControl_RESTORE && fail {
			return result, "snapshot_integrity_error"
		}
		if p.Download == nil || p.Archive == nil || p.Archive.Validate() != nil {
			return result, "invalid_payload"
		}
		request, _ := http.NewRequest("GET", p.Download.URL, nil)
		request.Header.Set("X-Test-Worker", a.session.nodeID)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return result, "snapshot_storage_error"
		}
		hash := sha256.New()
		size, err := io.Copy(hash, response.Body)
		response.Body.Close()
		if err != nil || size != p.Archive.Size || hex.EncodeToString(hash.Sum(nil)) != p.Archive.SHA256 {
			return result, "snapshot_integrity_error"
		}
		result.State = "verified"
		if c.Action == registryv1.SandboxPersistenceControl_RESTORE {
			result.State = "ready"
			a.mu.Lock()
			if a.states[c.OperationId] != "ready" && a.states[c.OperationId] != "active" {
				a.boots++
			}
			a.mu.Unlock()
		}
	case registryv1.SandboxPersistenceControl_FINALIZE_SUSPEND:
		if !p.SnapshotCommitted {
			return result, "session_state_conflict"
		}
		result.State = "finalized"
	case registryv1.SandboxPersistenceControl_PREPARE_RESTORE:
		if state == "ready" {
			result.State = "ready"
		} else {
			result.State = "prepared"
		}
	case registryv1.SandboxPersistenceControl_ACTIVATE:
		result.State = "active"
	case registryv1.SandboxPersistenceControl_CANCEL:
		a.mu.Lock()
		refuse := a.refuseCancel
		a.mu.Unlock()
		if refuse {
			return wire.Result{State: "busy"}, ""
		}
		result.State = "canceled"
		if p.PurgeSession {
			result.State = "deleted"
		}
		if p.Rollback {
			result.State = "running"
		}
	default:
		return result, "invalid_payload"
	}
	a.mu.Lock()
	a.states[c.OperationId] = result.State
	a.mu.Unlock()
	return result, ""
}

func coordinatorTestConfig() SnapshotCoordinatorConfig {
	return SnapshotCoordinatorConfig{MaxBytes: 16 << 20, MaxExpandedBytes: 32 << 20, AccountQuotaBytes: 64 << 20, OperationTimeout: 5 * time.Second, RetentionTTL: time.Hour, MaxRetentionTTL: 24 * time.Hour, RetryInterval: 10 * time.Millisecond, ReconcileTimeout: time.Second, DrainTimeout: time.Second, MultipartConcurrency: 2, MaxConcurrentOperations: 2}
}

func startCoordinatorTest(t *testing.T, svc *RegistryService, o SnapshotObjects, cfg SnapshotCoordinatorConfig) *SnapshotCoordinator {
	t.Helper()
	c, err := NewSnapshotCoordinator(svc, o, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return c
}

func waitCoordinatorOperation(t *testing.T, c *SnapshotCoordinator, op *sandboxsnapshot.Operation, phase string) sandboxsnapshot.Operation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		current, err := c.repository.Operation(context.Background(), op.OwnerID, op.SessionID, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Phase == phase {
			return current
		}
		time.Sleep(5 * time.Millisecond)
	}
	current, _ := c.repository.Operation(context.Background(), op.OwnerID, op.SessionID, op.ID)
	t.Fatalf("operation did not reach %s: %+v code=%s message=%s", phase, current, current.ErrorCode, current.ErrorMessage)
	return current
}

func TestSnapshotCoordinatorTransfersAndResumesAcrossWorkers(t *testing.T) {
	svc, a, b, _ := setupSnapshotRouteTest(t)
	data := bytes.Repeat([]byte("snapshot-data"), 450000)
	source := startCoordinatorActor(t, a, data)
	target := startCoordinatorActor(t, b, nil)
	o := newCoordinatorObjects(t)
	o.recoverComplete = true
	c := startCoordinatorTest(t, svc, o, coordinatorTestConfig())
	request, cancel := context.WithCancel(context.Background())
	_, op, err := c.Begin(request, "owner", "session", sandboxsnapshot.Suspend, "suspend", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	finished := waitCoordinatorOperation(t, c, op, "completed")
	if finished.Status != "succeeded" {
		t.Fatal(finished)
	}
	o.mu.Lock()
	manifest := bytes.Clone(o.manifests[op.ID])
	workerGets := o.workerGets
	o.mu.Unlock()
	if len(manifest) == 0 || workerGets < 1 {
		t.Fatal("publication skipped immutable manifest or worker readback")
	}
	source.mu.Lock()
	sourceFinalized := source.states[op.ID] == "finalized"
	source.mu.Unlock()
	if !sourceFinalized {
		t.Fatal("source cleanup was not acknowledged")
	}
	// Force cross-worker placement even though the source has the archive.
	a.lastHeartbeatUnixMs.Store(time.Now().Add(-time.Minute).UnixMilli())
	_, wake, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Resume, "resume", 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	woken := waitCoordinatorOperation(t, c, wake, "completed")
	if woken.Status != "succeeded" || woken.TargetWorkerID != "worker-b" {
		t.Fatal(woken)
	}
	session, err := c.repository.Session(context.Background(), "owner", "session")
	if err != nil || session.State != sandboxsnapshot.Running || session.WorkerID != "worker-b" || session.Generation != 2 {
		t.Fatalf("restore ownership: %+v %v", session, err)
	}
	target.mu.Lock()
	boots := target.boots
	target.mu.Unlock()
	if boots != 1 {
		t.Fatalf("booted %d instances", boots)
	}
}

func TestSnapshotCoordinatorCorruptionPreservesSourceAndNoRecoveryPoint(t *testing.T) {
	svc, a, _, _ := setupSnapshotRouteTest(t)
	startCoordinatorActor(t, a, []byte("snapshot"))
	o := newCoordinatorObjects(t)
	o.corrupt = true
	c := startCoordinatorTest(t, svc, o, coordinatorTestConfig())
	_, op, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Suspend, "suspend", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	failed := waitCoordinatorOperation(t, c, op, "resolved")
	if failed.Status != "failed" || failed.ErrorCode != "snapshot_integrity_error" {
		t.Fatalf("%+v code=%s message=%s", failed, failed.ErrorCode, failed.ErrorMessage)
	}
	session, err := c.repository.Session(context.Background(), "owner", "session")
	if err != nil || session.State != sandboxsnapshot.Running || session.LatestSnapshotID != "" || session.OperationID != "" {
		t.Fatalf("lost source: %+v %v", session, err)
	}
	o.mu.Lock()
	manifests := len(o.manifests)
	o.mu.Unlock()
	if manifests != 0 {
		t.Fatal("published a corrupt recovery point")
	}
}

func TestSnapshotCoordinatorFailedTargetMustConfirmCancellation(t *testing.T) {
	svc, a, b, _ := setupSnapshotRouteTest(t)
	startCoordinatorActor(t, a, []byte("snapshot"))
	target := startCoordinatorActor(t, b, nil)
	o := newCoordinatorObjects(t)
	c := startCoordinatorTest(t, svc, o, coordinatorTestConfig())
	_, op, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Suspend, "suspend", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorOperation(t, c, op, "completed")
	a.lastHeartbeatUnixMs.Store(time.Now().Add(-time.Minute).UnixMilli())
	target.mu.Lock()
	target.restoreError = true
	target.refuseCancel = true
	target.mu.Unlock()
	_, wake, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Resume, "resume", 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorOperation(t, c, wake, "reconciling")
	session, _ := c.repository.Session(context.Background(), "owner", "session")
	if session.State != sandboxsnapshot.Restoring || session.OperationID != wake.ID || session.LatestSnapshotID != op.ID {
		t.Fatalf("unconfirmed target was released: %+v", session)
	}
	if _, _, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Resume, "resume-again", 0, 300); !errors.Is(err, sandboxsnapshot.ErrConflict) {
		t.Fatalf("duplicate writable target admitted: %v", err)
	}
	target.mu.Lock()
	target.refuseCancel = false
	target.mu.Unlock()
	waitCoordinatorOperation(t, c, wake, "resolved")
	session, _ = c.repository.Session(context.Background(), "owner", "session")
	if session.State != sandboxsnapshot.Suspended || session.LatestSnapshotID != op.ID {
		t.Fatalf("restore consumed recovery point: %+v", session)
	}
}

func waitCoordinatorActorState(t *testing.T, actor *coordinatorActor, id, state string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		actor.mu.Lock()
		current := actor.states[id]
		actor.mu.Unlock()
		if current == state {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("worker did not reach %s", state)
}

func TestSnapshotCoordinatorLostActivationAcknowledgementFinishesSameInstance(t *testing.T) {
	svc, a, b, _ := setupSnapshotRouteTest(t)
	startCoordinatorActor(t, a, []byte("snapshot"))
	target := startCoordinatorActor(t, b, nil)
	block := make(chan struct{})
	defer close(block)
	target.blocks[registryv1.SandboxPersistenceControl_ACTIVATE] = block
	cfg := coordinatorTestConfig()
	cfg.OperationTimeout = time.Second
	c := startCoordinatorTest(t, svc, newCoordinatorObjects(t), cfg)
	_, op, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Suspend, "suspend", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorOperation(t, c, op, "completed")
	a.lastHeartbeatUnixMs.Store(time.Now().Add(-time.Minute).UnixMilli())
	_, wake, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Resume, "resume", 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorActorState(t, target, wake.ID, "active")
	waitCoordinatorOperation(t, c, wake, "completed")
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.boots != 1 || target.counts[registryv1.SandboxPersistenceControl_CANCEL] != 0 || target.counts[registryv1.SandboxPersistenceControl_INSPECT] < 1 {
		t.Fatalf("lost acknowledgement caused replacement: boots=%d controls=%v", target.boots, target.counts)
	}
}

func TestSnapshotCoordinatorRestartInspectsReadyTargetBeforeActivation(t *testing.T) {
	svc, a, b, _ := setupSnapshotRouteTest(t)
	startCoordinatorActor(t, a, []byte("snapshot"))
	target := startCoordinatorActor(t, b, nil)
	block := make(chan struct{})
	defer close(block)
	target.blocks[registryv1.SandboxPersistenceControl_RESTORE] = block
	o := newCoordinatorObjects(t)
	c := startCoordinatorTest(t, svc, o, coordinatorTestConfig())
	_, op, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Suspend, "suspend", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorOperation(t, c, op, "completed")
	a.lastHeartbeatUnixMs.Store(time.Now().Add(-time.Minute).UnixMilli())
	_, wake, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Resume, "resume", 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorActorState(t, target, wake.ID, "ready")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	// Replace the registry and connection as well as the coordinator, so no
	// process-local reservation or pending delivery can satisfy recovery.
	restarted := NewRegistryService(svc.store, nil, 5, 15, time.Minute)
	connection := addSnapshotTestWorker(t, restarted, snapshotTestHello("worker-b"))
	recovered := startCoordinatorActor(t, connection, nil)
	recovered.mu.Lock()
	recovered.states[wake.ID] = "ready"
	recovered.mu.Unlock()
	c2 := startCoordinatorTest(t, restarted, o, coordinatorTestConfig())
	waitCoordinatorOperation(t, c2, wake, "completed")
	recovered.mu.Lock()
	defer recovered.mu.Unlock()
	if recovered.boots != 0 || recovered.counts[registryv1.SandboxPersistenceControl_RESTORE] != 0 || recovered.counts[registryv1.SandboxPersistenceControl_INSPECT] < 1 || recovered.states[wake.ID] != "active" {
		t.Fatalf("recovery duplicated the existing target: boots=%d controls=%v state=%s", recovered.boots, recovered.counts, recovered.states[wake.ID])
	}
}

func TestSnapshotCoordinatorWorkerReconnectDuringActivation(t *testing.T) {
	svc, a, b, _ := setupSnapshotRouteTest(t)
	startCoordinatorActor(t, a, []byte("snapshot"))
	target := startCoordinatorActor(t, b, nil)
	block := make(chan struct{})
	defer close(block)
	target.blocks[registryv1.SandboxPersistenceControl_ACTIVATE] = block
	cfg := coordinatorTestConfig()
	cfg.OperationTimeout = time.Minute
	c := startCoordinatorTest(t, svc, newCoordinatorObjects(t), cfg)
	_, op, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Suspend, "suspend", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorOperation(t, c, op, "completed")
	a.lastHeartbeatUnixMs.Store(time.Now().Add(-time.Minute).UnixMilli())
	_, wake, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Resume, "resume", 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorActorState(t, target, wake.ID, "active")
	// Keep the coordinator and its old reservation alive. Only the Worker
	// connection is replaced after activation took effect but before its reply.
	b.close(errors.New("worker disconnected"))
	connection := addSnapshotTestWorker(t, svc, snapshotTestHello("worker-b"))
	recovered := startCoordinatorActor(t, connection, nil)
	recovered.mu.Lock()
	recovered.states[wake.ID] = "active"
	recovered.mu.Unlock()
	waitCoordinatorOperation(t, c, wake, "completed")
	recovered.mu.Lock()
	defer recovered.mu.Unlock()
	if recovered.boots != 0 || recovered.counts[registryv1.SandboxPersistenceControl_RESTORE] != 0 || recovered.counts[registryv1.SandboxPersistenceControl_INSPECT] < 1 || recovered.counts[registryv1.SandboxPersistenceControl_ACTIVATE] < 1 || recovered.counts[registryv1.SandboxPersistenceControl_CANCEL] != 0 {
		t.Fatalf("reconnected activation did not reconcile the existing target: boots=%d controls=%v", recovered.boots, recovered.counts)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reservations[wake.ID] != nil {
		t.Fatal("old connection reservation retained after successful activation")
	}
}

func TestSnapshotCoordinatorActivationReconnectRetainsReservationWithoutLiveTarget(t *testing.T) {
	for _, state := range []string{"missing", "prepared", "canceled", "wrong-spec", "expired"} {
		t.Run(state, func(t *testing.T) {
			svc, a, b, _ := setupSnapshotRouteTest(t)
			startCoordinatorActor(t, a, []byte("snapshot"))
			target := startCoordinatorActor(t, b, nil)
			block := make(chan struct{})
			defer close(block)
			target.blocks[registryv1.SandboxPersistenceControl_ACTIVATE] = block
			c := startCoordinatorTest(t, svc, newCoordinatorObjects(t), coordinatorTestConfig())
			_, op, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Suspend, "suspend", 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			waitCoordinatorOperation(t, c, op, "completed")
			a.lastHeartbeatUnixMs.Store(time.Now().Add(-time.Minute).UnixMilli())
			_, wake, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Resume, "resume", 0, 300)
			if err != nil {
				t.Fatal(err)
			}
			waitCoordinatorActorState(t, target, wake.ID, "active")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := c.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			b.close(errors.New("worker disconnected"))
			connection := addSnapshotTestWorker(t, svc, snapshotTestHello("worker-b"))
			recovered := startCoordinatorActor(t, connection, nil)
			recovered.states[wake.ID] = state
			if state == "wrong-spec" {
				recovered.states[wake.ID] = "active"
				recovered.spec.CPUs++
			}
			if state == "expired" {
				recovered.states[wake.ID] = "active"
				recovered.lease = time.Now().Add(-time.Second).UnixMilli()
			}
			current, err := c.repository.Operation(ctx, wake.OwnerID, wake.SessionID, wake.ID)
			if err != nil {
				t.Fatal(err)
			}
			session, err := c.repository.Session(ctx, wake.OwnerID, wake.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.resume(ctx, current, session, snapshotParameters{LeaseTTLSec: 300}); !errors.Is(err, sandboxsnapshot.ErrConflict) {
				t.Fatalf("invalid target accepted: %v", err)
			}
			reservation := c.reservations[wake.ID]
			if reservation == nil || reservation.released {
				t.Fatal("invalid target evidence released the old reservation")
			}
			recovered.mu.Lock()
			defer recovered.mu.Unlock()
			if recovered.counts[registryv1.SandboxPersistenceControl_INSPECT] != 1 || recovered.counts[registryv1.SandboxPersistenceControl_ACTIVATE] != 0 || recovered.counts[registryv1.SandboxPersistenceControl_RESTORE] != 0 {
				t.Fatalf("invalid target caused execution: %v", recovered.counts)
			}
		})
	}
}

func TestSnapshotCoordinatorResumeWaitsForCommittedSourceCleanup(t *testing.T) {
	svc, a, _, _ := setupSnapshotRouteTest(t)
	source := startCoordinatorActor(t, a, []byte("snapshot"))
	block := make(chan struct{})
	source.blocks[registryv1.SandboxPersistenceControl_FINALIZE_SUSPEND] = block
	c := startCoordinatorTest(t, svc, newCoordinatorObjects(t), coordinatorTestConfig())
	_, op, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Suspend, "suspend", 0, 0)
	if err != nil {
		close(block)
		t.Fatal(err)
	}
	waitCoordinatorActorState(t, source, op.ID, "finalized")
	_, _, resumeErr := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Resume, "resume", 0, 300)
	close(block)
	if !errors.Is(resumeErr, sandboxsnapshot.ErrConflict) {
		t.Fatalf("started target without source cleanup acknowledgement: %v", resumeErr)
	}
	waitCoordinatorOperation(t, c, op, "completed")
}

func TestSnapshotCoordinatorNoCompatibleWorkerKeepsRecoveryPoint(t *testing.T) {
	svc, a, b, _ := setupSnapshotRouteTest(t)
	startCoordinatorActor(t, a, []byte("snapshot"))
	c := startCoordinatorTest(t, svc, newCoordinatorObjects(t), coordinatorTestConfig())
	_, op, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Suspend, "suspend", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorOperation(t, c, op, "completed")
	a.lastHeartbeatUnixMs.Store(time.Now().Add(-time.Minute).UnixMilli())
	b.lastHeartbeatUnixMs.Store(time.Now().Add(-time.Minute).UnixMilli())
	_, wake, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Resume, "resume", 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	failed := waitCoordinatorOperation(t, c, wake, "resolved")
	if failed.ErrorCode != "no_worker" || failed.TargetWorkerID != "" {
		t.Fatalf("wrong scheduling failure: %+v code=%s", failed, failed.ErrorCode)
	}
	session, err := c.repository.Session(context.Background(), "owner", "session")
	if err != nil || session.State != sandboxsnapshot.Suspended || session.OperationID != "" || session.LatestSnapshotID != op.ID {
		t.Fatalf("no worker consumed recovery point: %+v %v", session, err)
	}
}

func TestSnapshotCoordinatorFirstSuspendDescribesExistingRuntime(t *testing.T) {
	svc, a, _, _ := setupSnapshotRouteTest(t)
	if _, err := svc.store.Persistence().SQL.ExecContext(context.Background(), `DELETE FROM sandbox_persistent_sessions`); err != nil {
		t.Fatal(err)
	}
	source := startCoordinatorActor(t, a, []byte("snapshot"))
	c := startCoordinatorTest(t, svc, newCoordinatorObjects(t), coordinatorTestConfig())
	_, op, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Suspend, "suspend", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorOperation(t, c, op, "completed")
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.counts[registryv1.SandboxPersistenceControl_INSPECT] != 1 {
		t.Fatal("first suspend did not describe the existing runtime")
	}
}

func TestSnapshotCoordinatorMissingTargetJournalRequiresCancellation(t *testing.T) {
	svc, a, b, _ := setupSnapshotRouteTest(t)
	startCoordinatorActor(t, a, []byte("snapshot"))
	target := startCoordinatorActor(t, b, nil)
	o := newCoordinatorObjects(t)
	c := startCoordinatorTest(t, svc, o, coordinatorTestConfig())
	_, op, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Suspend, "suspend", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorOperation(t, c, op, "completed")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	// A crash after target persistence leaves uncertainty about whether its
	// first delivery ran. Missing local state cannot become a fresh restore.
	wake, err := svc.BeginSnapshotOperation(context.Background(), sandboxsnapshot.BeginRequest{OwnerID: "owner", SessionID: "session", Kind: sandboxsnapshot.Resume, IdempotencyKey: "resume", Parameters: json.RawMessage(`{"lease_ttl_sec":300}`), DeadlineUnixMS: time.Now().Add(5 * time.Second).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.snapshotRepository.SetRestoreTarget(context.Background(), wake, "worker-b"); err != nil {
		t.Fatal(err)
	}
	c2 := startCoordinatorTest(t, svc, o, coordinatorTestConfig())
	waitCoordinatorOperation(t, c2, &wake, "resolved")
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.boots != 0 || target.counts[registryv1.SandboxPersistenceControl_PREPARE_RESTORE] != 0 || target.counts[registryv1.SandboxPersistenceControl_CANCEL] != 1 {
		t.Fatalf("missing journal allowed blind restore: boots=%d controls=%v", target.boots, target.counts)
	}
}

func TestSnapshotCoordinatorExpiredPublishedLeaseCancelsActiveTarget(t *testing.T) {
	svc, a, b, scopedID := setupSnapshotRouteTest(t)
	startCoordinatorActor(t, a, []byte("snapshot"))
	target := startCoordinatorActor(t, b, nil)
	block := make(chan struct{})
	defer close(block)
	target.blocks[registryv1.SandboxPersistenceControl_ACTIVATE] = block
	cfg := coordinatorTestConfig()
	cfg.OperationTimeout = time.Second
	c := startCoordinatorTest(t, svc, newCoordinatorObjects(t), cfg)
	_, op, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Suspend, "suspend", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorOperation(t, c, op, "completed")
	a.lastHeartbeatUnixMs.Store(time.Now().Add(-time.Minute).UnixMilli())
	_, wake, err := c.Begin(context.Background(), "owner", "session", sandboxsnapshot.Resume, "resume", 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	waitCoordinatorActorState(t, target, wake.ID, "active")
	if _, err := svc.store.Persistence().SQL.ExecContext(context.Background(), `UPDATE terminal_session_routes SET lease_expires_unix_ms=1 WHERE scoped_session_id=?`, scopedID); err != nil {
		t.Fatal(err)
	}
	waitCoordinatorOperation(t, c, wake, "resolved")
	session, err := c.repository.Session(context.Background(), "owner", "session")
	if err != nil || session.State != sandboxsnapshot.Suspended || session.OperationID != "" || session.LatestSnapshotID != op.ID {
		t.Fatalf("expired activation did not return to recovery point: %+v %v", session, err)
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.boots != 1 || target.states[wake.ID] != "canceled" {
		t.Fatalf("expired target remained executable: boots=%d state=%s", target.boots, target.states[wake.ID])
	}
}

func TestSnapshotCoordinatorConcurrentFirstAdoptionJoinsDurableOperation(t *testing.T) {
	for _, lateState := range []string{"running", "busy"} {
		t.Run(lateState, func(t *testing.T) {
			svc, source, _, _ := setupSnapshotRouteTest(t)
			if _, err := svc.store.Persistence().SQL.ExecContext(context.Background(), `DELETE FROM sandbox_persistent_sessions`); err != nil {
				t.Fatal(err)
			}
			coordinator := startCoordinatorTest(t, svc, newCoordinatorObjects(t), coordinatorTestConfig())
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			type answer struct {
				op  *sandboxsnapshot.Operation
				err error
			}
			first := make(chan answer, 1)
			second := make(chan answer, 1)
			begin := func(key string, result chan<- answer) {
				_, op, err := coordinator.Begin(ctx, "owner", "session", sandboxsnapshot.Suspend, key, 0, 0)
				result <- answer{op, err}
			}
			go begin("first-key", first)
			readInspect := func() *registryv1.SandboxPersistenceControl {
				t.Helper()
				select {
				case message := <-source.controlOutbound:
					control := message.GetSandboxPersistenceControl()
					if control == nil || control.Action != registryv1.SandboxPersistenceControl_INSPECT {
						t.Fatal("unexpected first-adoption control", message)
					}
					return control
				case <-ctx.Done():
					t.Fatal("first-adoption inspect did not arrive")
					return nil
				}
			}
			firstInspect := readInspect()
			go begin("second-key", second)
			secondInspect := readInspect()
			reply := func(control *registryv1.SandboxPersistenceControl, state string) {
				t.Helper()
				spec := snapshotTestSpec()
				body, _ := json.Marshal(wire.Result{State: state, Spec: &spec, InstanceID: "existing-source", LeaseExpiresUnixMS: time.Now().Add(time.Hour).UnixMilli()})
				if err := source.persistence.resolve(&registryv1.SandboxPersistenceReport{RequestId: control.RequestId, OperationId: control.OperationId, ScopedSessionId: control.ScopedSessionId, Generation: control.Generation, Action: control.Action, Completed: true, PayloadJson: body}); err != nil {
					t.Fatal(err)
				}
			}
			reply(firstInspect, "running")
			var accepted answer
			select {
			case accepted = <-first:
			case <-ctx.Done():
				t.Fatal("first request was not accepted")
			}
			if accepted.err != nil || accepted.op == nil {
				t.Fatal("first adoption failed", accepted.err)
			}
			// The second inspection finishes only after the first request has moved
			// the durable session into snapshotting. It must join that operation.
			reply(secondInspect, lateState)
			select {
			case joined := <-second:
				if joined.err != nil || joined.op == nil || joined.op.ID != accepted.op.ID {
					t.Fatal("concurrent first suspend did not join durable ownership", joined.err)
				}
			case <-ctx.Done():
				t.Fatal("second adoption did not settle")
			}
		})
	}
}
