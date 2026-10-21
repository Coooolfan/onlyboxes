package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/runtimelease"
)

type leaseRPCFixture struct {
	Entry         *runtimelease.Entry `json:"entry"`
	DropReply     bool                `json:"drop_reply"`
	MissingStatus bool                `json:"missing_status"`
	Calls         []string            `json:"calls"`
}

// An owned subprocess models only the Docker exec control boundary, including
// a committed mutation whose stdout acknowledgement is lost.
func TestSnapshotLeaseRPCChild(t *testing.T) {
	path := os.Getenv("ONLYBOXES_LEASE_RPC_FIXTURE")
	if path == "" {
		t.Skip("owned subprocess only")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		os.Exit(2)
	}
	var state leaseRPCFixture
	if json.Unmarshal(data, &state) != nil {
		os.Exit(2)
	}
	var request runtimelease.Request
	if json.NewDecoder(os.Stdin).Decode(&request) != nil {
		os.Exit(2)
	}
	state.Calls = append(state.Calls, request.Action)
	code := ""
	var reply any
	switch request.Action {
	case "status":
		if state.Entry == nil || state.MissingStatus {
			code = "not_found"
		} else {
			reply = state.Entry
		}
	case "arm", "renew":
		revision := int64(0)
		if state.Entry != nil {
			revision = state.Entry.Revision
		}
		if request.ExpectedRevision != revision {
			code = "conflict"
		} else {
			value := request.Entry
			state.Entry = &value
			reply = value
		}
	case "forget":
		if state.Entry != nil && state.Entry.Revision != request.ExpectedRevision {
			code = "conflict"
		} else {
			state.Entry = nil
			reply = map[string]bool{"forgotten": true}
		}
	default:
		code = "unavailable"
	}
	drop := state.DropReply && (request.Action == "arm" || request.Action == "renew") && code == ""
	if drop {
		state.DropReply = false
	}
	data, _ = json.Marshal(state)
	if os.WriteFile(path, data, 0600) != nil {
		os.Exit(2)
	}
	if drop {
		os.Exit(1)
	}
	if code != "" {
		_ = json.NewEncoder(os.Stdout).Encode(runtimelease.Response{Error: code})
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(reply)
	os.Exit(0)
}

func newLeaseRPCFixture(t *testing.T, state leaseRPCFixture) (*dockerSnapshotLeaseClient, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "state.json")
	data, _ := json.Marshal(state)
	if os.WriteFile(path, data, 0600) != nil {
		t.Fatal("fixture state")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ONLYBOXES_LEASE_RPC_FIXTURE", path)
	t.Setenv("ONLYBOXES_LEASE_RPC_EXECUTABLE", executable)
	script := "#!/bin/sh\nexec \"$ONLYBOXES_LEASE_RPC_EXECUTABLE\" -test.run=^TestSnapshotLeaseRPCChild$ -- \"$@\"\n"
	if os.WriteFile(filepath.Join(root, "docker"), []byte(script), 0700) != nil {
		t.Fatal("fixture executable")
	}
	t.Setenv("PATH", root)
	client, err := newDockerSnapshotLeaseClient(strings.Repeat("f", 64))
	if err != nil {
		t.Fatal(err)
	}
	return client, path
}

func readLeaseRPCFixture(t *testing.T, path string) leaseRPCFixture {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state leaseRPCFixture
	if json.Unmarshal(data, &state) != nil {
		t.Fatal("invalid fixture state")
	}
	return state
}

func TestLeaseClientReconcilesLostArmAckWithoutResettingLease(t *testing.T) {
	client, path := newLeaseRPCFixture(t, leaseRPCFixture{DropReply: true})
	op := testSnapshotOperation()
	if client.Arm(context.Background(), op) == nil {
		t.Fatal("lost receipt acknowledged")
	}
	if err := client.Arm(context.Background(), op); err != nil {
		t.Fatal("durable arm could not be reconciled", err)
	}
	state := readLeaseRPCFixture(t, path)
	if state.Entry == nil || state.Entry.Revision != 1 || state.Entry.ExpiresUnixMS != op.LeaseExpiresUnixMS || strings.Join(state.Calls, ",") != "status,arm,status" {
		t.Fatal("replay rearmed or changed original lease", state)
	}
}

func TestLeaseClientMissingRecordCannotBeRenewed(t *testing.T) {
	client, path := newLeaseRPCFixture(t, leaseRPCFixture{})
	if client.Renew(context.Background(), testSnapshotOperation()) == nil {
		t.Fatal("renew silently armed missing execution")
	}
	if strings.Join(readLeaseRPCFixture(t, path).Calls, ",") != "status" {
		t.Fatal("renew created new ownership")
	}
}

func TestLeaseClientAdoptionReconcilesLostReceipt(t *testing.T) {
	client, path := newLeaseRPCFixture(t, leaseRPCFixture{DropReply: true})
	op := testSnapshotOperation()
	if client.Adopt(context.Background(), op) == nil {
		t.Fatal("lost adoption receipt acknowledged")
	}
	if err := client.Adopt(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	op.LeaseExpiresUnixMS += 1000
	if err := client.Adopt(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	state := readLeaseRPCFixture(t, path)
	if strings.Join(state.Calls, ",") != "status,arm,status,status,renew" || state.Entry.Revision != 2 || state.Entry.ExpiresUnixMS != op.LeaseExpiresUnixMS {
		t.Fatal("adoption replay reset ownership", state)
	}
}

func TestLeaseClientAdoptionCannotReviveOwnership(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired", true: "revoked"}[revoked], func(t *testing.T) {
			op := testSnapshotOperation()
			expiry := time.Now().Add(-time.Second).UnixMilli()
			if revoked {
				expiry = op.LeaseExpiresUnixMS
			}
			entry := runtimelease.Entry{ContainerID: op.ContainerID, Scope: op.SessionID, Generation: op.Generation, Revision: 1, ExpiresUnixMS: expiry, Revoked: revoked}
			client, path := newLeaseRPCFixture(t, leaseRPCFixture{Entry: &entry})
			if client.Adopt(context.Background(), op) == nil {
				t.Fatal("adoption revived invalid ownership")
			}
			if strings.Join(readLeaseRPCFixture(t, path).Calls, ",") != "status" {
				t.Fatal("adoption mutated invalid ownership")
			}
		})
	}
}

func TestLeaseClientMissingStatusStillRequiresForgetBarrier(t *testing.T) {
	op := testSnapshotOperation()
	entry := runtimelease.Entry{ContainerID: op.ContainerID, Scope: op.SessionID, Generation: op.Generation, Revision: 1, ExpiresUnixMS: op.LeaseExpiresUnixMS}
	client, path := newLeaseRPCFixture(t, leaseRPCFixture{Entry: &entry, MissingStatus: true})
	if client.Forget(context.Background(), op) == nil {
		t.Fatal("missing status bypassed in-flight arm barrier")
	}
	state := readLeaseRPCFixture(t, path)
	if strings.Join(state.Calls, ",") != "status,forget" || state.Entry == nil {
		t.Fatal("cleanup was acknowledged without serialized forget", state)
	}
}

type testExecutionLeases struct{ arm, check, renew, forget func(snapshotOperation) error }

func (g testExecutionLeases) Adopt(ctx context.Context, op snapshotOperation) error {
	return g.Arm(ctx, op)
}

func (g testExecutionLeases) Arm(_ context.Context, op snapshotOperation) error {
	if g.arm != nil {
		return g.arm(op)
	}
	return nil
}
func (g testExecutionLeases) Check(_ context.Context, op snapshotOperation) error {
	if g.check != nil {
		return g.check(op)
	}
	return nil
}
func (g testExecutionLeases) Renew(_ context.Context, op snapshotOperation) error {
	if g.renew != nil {
		return g.renew(op)
	}
	return nil
}
func (g testExecutionLeases) Forget(_ context.Context, op snapshotOperation) error {
	if g.forget != nil {
		return g.forget(op)
	}
	return nil
}

func TestRollbackPersistsLeaseAndRequiresGuardBeforeAnyNativeStart(t *testing.T) {
	for _, failure := range []string{"guard", "journal"} {
		t.Run(failure, func(t *testing.T) {
			c, j := testSnapshotController(t)
			op := testSnapshotOperation()
			op.LeaseExpiresUnixMS = time.Now().Add(-time.Second).UnixMilli()
			if err := j.Claim(op); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "native-called")
			dir := t.TempDir()
			if os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n/usr/bin/touch '"+marker+"'\nexit 99\n"), 0700) != nil {
				t.Fatal("fixture")
			}
			t.Setenv("PATH", dir)
			armed := false
			c.config.LeaseGuard = testExecutionLeases{arm: func(got snapshotOperation) error {
				armed = true
				state, err := j.Snapshot()
				saved := state.Operations[op.ID]
				if err != nil || saved.Phase != "rolling_back" || saved.LeaseExpiresUnixMS != got.LeaseExpiresUnixMS || got.LeaseExpiresUnixMS <= time.Now().UnixMilli() {
					t.Fatal("guard called before durable controlled-boot intent")
				}
				return errSnapshotLeaseUnavailable
			}}
			if failure == "journal" {
				j.persist = func(snapshotJournalState) error { return errors.New("disk unavailable") }
			}
			if _, err := c.cancel(context.Background(), op, wire.Control{Rollback: true}); err == nil {
				t.Fatal("unconfirmed rollback succeeded")
			}
			if armed != (failure == "guard") {
				t.Fatal("guard reached before journal commit")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("unleased native action ran")
			}
		})
	}
}

func TestRestoreGuardRefusalCannotStartOrReplaceNativeRoot(t *testing.T) {
	c, j := testSnapshotController(t)
	op := testSnapshotOperation()
	op.Kind = "target"
	op.Generation = 2
	op.Phase = "prepared"
	op.ImageID = "sha256:" + strings.Repeat("a", 64)
	op.Artifact = snapshotArtifactName(op.SessionID, op.ID, op.Generation)
	data := []byte("verified")
	archive := transferTestArchive(data)
	op.Archive = &archive
	if err := j.Claim(op); err != nil {
		t.Fatal(err)
	}
	if os.WriteFile(c.artifact(op.Artifact), data, 0600) != nil {
		t.Fatal("artifact")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	script := "#!/bin/sh\ncase \"$1\" in inspect) echo '{\"Status\":\"created\",\"Paused\":false}' ;; *) /usr/bin/touch '" + marker + "'; exit 99 ;; esac\n"
	if os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700) != nil {
		t.Fatal("fixture")
	}
	t.Setenv("PATH", dir)
	c.config.LeaseGuard = testExecutionLeases{arm: func(got snapshotOperation) error {
		state, _ := j.Snapshot()
		saved := state.Operations[op.ID]
		if saved.Phase != "importing" || saved.ContainerID != got.ContainerID || saved.LeaseExpiresUnixMS != got.LeaseExpiresUnixMS {
			t.Fatal("guard preceded durable restore intent")
		}
		return errSnapshotLeaseUnavailable
	}}
	if _, err := c.restore(context.Background(), op, wire.Control{}); !errors.Is(err, errSnapshotLeaseUnavailable) {
		t.Fatal("unconfirmed guard did not block startup", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("guard refusal started or replaced native root")
	}
	state, _ := j.Snapshot()
	if state.Operations[op.ID].ContainerID != op.ContainerID {
		t.Fatal("startup failure discarded chosen native identity")
	}
}

func TestCompletedRollbackReplayCannotRenewExpiredLease(t *testing.T) {
	c, j := testSnapshotController(t)
	op := testSnapshotOperation()
	op.LeaseExpiresUnixMS = time.Now().Add(-time.Second).UnixMilli()
	if err := j.Claim(op); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"rolling_back", "rolled_back"} {
		if err := j.Update(op.ID, func(o *snapshotOperation) error { o.Phase = phase; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	op.Phase = "rolled_back"
	c.config.LeaseGuard = testExecutionLeases{arm: func(snapshotOperation) error { t.Fatal("completed rollback rearmed execution"); return nil }}
	if _, err := c.cancel(context.Background(), op, wire.Control{Rollback: true}); err == nil {
		t.Fatal("expired completed rollback restarted")
	}
	state, _ := j.Snapshot()
	if state.Operations[op.ID].LeaseExpiresUnixMS != op.LeaseExpiresUnixMS {
		t.Fatal("replay reset absolute lease")
	}
}

func TestPersistentRenewalNeedsMatchingGuardAcknowledgement(t *testing.T) {
	c, j := testSnapshotController(t)
	op := testSnapshotOperation()
	op.Kind = "adopt"
	op.Phase = "active"
	op.StagingBytes = 0
	if err := j.Claim(op); err != nil {
		t.Fatal(err)
	}
	until := time.UnixMilli(op.LeaseExpiresUnixMS + 60000)
	calls := 0
	c.config.LeaseGuard = testExecutionLeases{renew: func(got snapshotOperation) error {
		calls++
		state, _ := j.Snapshot()
		if state.Operations[op.ID].LeaseExpiresUnixMS != until.UnixMilli() || got.LeaseExpiresUnixMS != until.UnixMilli() {
			t.Fatal("guard renewal preceded durable intent")
		}
		return errSnapshotLeaseUnavailable
	}}
	if c.extendLease(op.SessionID, op.Generation, until) == nil {
		t.Fatal("unconfirmed renewal acknowledged")
	}
	// A matching durable intent is not itself evidence that the earlier RPC
	// succeeded. A retry must still obtain the guard's exact acknowledgement.
	if c.extendLease(op.SessionID, op.Generation, until) == nil || calls != 2 {
		t.Fatal("retry bypassed guard confirmation")
	}
}

func TestLeaseClientDoesNotTreatDiagnosticsAsMissingOwnership(t *testing.T) {
	for _, reply := range []string{"", `{"error":"unavailable"}`, `{"error":"not_found","private":"secret"}`, `{"error":"not_found"} {}`} {
		t.Run(reply, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "reply")
			_ = os.WriteFile(out, []byte(reply), 0600)
			_ = os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n/bin/cat '"+out+"'\necho private-secret >&2\nexit 1\n"), 0700)
			t.Setenv("PATH", dir)
			client, _ := newDockerSnapshotLeaseClient(strings.Repeat("f", 64))
			err := client.Arm(context.Background(), testSnapshotOperation())
			if !errors.Is(err, errSnapshotLeaseUnavailable) || strings.Contains(err.Error(), "private") {
				t.Fatal("unproven missing ownership or leaked diagnostic", err)
			}
		})
	}
}

func TestLeaseClientRejectsWrongScopeAndReceiptIdentity(t *testing.T) {
	op := testSnapshotOperation()
	dir := t.TempDir()
	out := filepath.Join(dir, "reply")
	_ = os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n/bin/cat '"+out+"'\n"), 0700)
	t.Setenv("PATH", dir)
	client, _ := newDockerSnapshotLeaseClient(strings.Repeat("f", 64))
	for _, value := range []map[string]any{
		{"container_id": op.ContainerID, "scope": "obx:another:session", "generation": op.Generation, "revision": 1, "expires_unix_ms": op.LeaseExpiresUnixMS, "revoked": false},
		{"container_id": strings.Repeat("c", 64), "scope": op.SessionID, "generation": op.Generation, "revision": 1, "expires_unix_ms": op.LeaseExpiresUnixMS, "revoked": false},
		{"container_id": op.ContainerID, "scope": op.SessionID, "generation": op.Generation, "revision": 1, "expires_unix_ms": op.LeaseExpiresUnixMS, "revoked": true},
	} {
		data, _ := json.Marshal(value)
		_ = os.WriteFile(out, data, 0600)
		if client.Check(context.Background(), op) == nil {
			t.Fatal("foreign or revoked lease accepted")
		}
	}
}
