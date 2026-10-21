package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/runtimelease"
)

var errSnapshotLeaseUnavailable = errors.New("independent execution lease unavailable")

// Arm is reserved for an explicitly authorized controlled boot. Check and
// Renew can never revive stopped/revoked ownership or create a missing record.
type snapshotExecutionLeases interface {
	Arm(context.Context, snapshotOperation) error
	Check(context.Context, snapshotOperation) error
	Renew(context.Context, snapshotOperation) error
	Forget(context.Context, snapshotOperation) error
}

// The launcher must verify the immutable helper image/ID, Worker, mounts and
// supervision readiness before installing this client into a controller.
// Calls never use a reusable container name or expose helper diagnostics.
type dockerSnapshotLeaseClient struct {
	helperID string
	locks    [64]sync.Mutex
}

func newDockerSnapshotLeaseClient(helperID string) (*dockerSnapshotLeaseClient, error) {
	if !dockerSnapshotDigest.MatchString("sha256:" + helperID) {
		return nil, errSnapshotLeaseUnavailable
	}
	return &dockerSnapshotLeaseClient{helperID: helperID}, nil
}

func (g *dockerSnapshotLeaseClient) rpc(ctx context.Context, request runtimelease.Request, reply any) error {
	data, err := json.Marshal(request)
	if err != nil || len(data) > 4096 {
		return errSnapshotLeaseUnavailable
	}
	call, cancel := context.WithTimeout(ctx, 7*time.Second)
	defer cancel()
	var output bytes.Buffer
	bounded := &snapshotBoundedWriter{writer: &output, remaining: 4096}
	callErr := dockerSnapshotCLI(call, bytes.NewReader(data), bounded, "exec", "-i", g.helperID, "/runtime-lease-guard", "-mode", "control")
	if bounded.exceeded {
		return errSnapshotLeaseUnavailable
	}
	// Failed controls have an allowlisted structured stdout frame. Stderr and
	// arbitrary daemon output are never used as evidence of a missing record.
	var failure runtimelease.Response
	if json.Unmarshal(output.Bytes(), &failure) == nil && failure.Error != "" {
		decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&failure) != nil || decoder.Decode(&extra) != io.EOF || failure.Entry != nil || failure.Ready || failure.Forgotten {
			return errSnapshotLeaseUnavailable
		}
		if callErr != nil && failure.Error == "not_found" {
			return runtimelease.ErrNotFound
		}
		return errSnapshotLeaseUnavailable
	}
	if callErr != nil {
		return errSnapshotLeaseUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(reply) != nil || decoder.Decode(&extra) != io.EOF {
		return errSnapshotLeaseUnavailable
	}
	return nil
}

func (g *dockerSnapshotLeaseClient) lock(op snapshotOperation) (func(), error) {
	if !validPersistenceScope(op.SessionID) || op.Generation <= 0 || !dockerSnapshotDigest.MatchString("sha256:"+op.ContainerID) {
		return nil, errSnapshotLeaseUnavailable
	}
	bucket, _ := strconv.ParseUint(op.ContainerID[:2], 16, 8)
	l := &g.locks[bucket%64]
	l.Lock()
	return l.Unlock, nil
}

func leaseIdentity(entry runtimelease.Entry, op snapshotOperation) bool {
	return entry.ContainerID == op.ContainerID && entry.Scope == op.SessionID && entry.Generation == op.Generation && entry.Revision > 0 && entry.Revision < 1<<62 && entry.ExpiresUnixMS > 0
}

func (g *dockerSnapshotLeaseClient) status(ctx context.Context, op snapshotOperation) (runtimelease.Entry, error) {
	var entry runtimelease.Entry
	err := g.rpc(ctx, runtimelease.Request{Action: "status", Entry: runtimelease.Entry{ContainerID: op.ContainerID}}, &entry)
	if err != nil {
		return runtimelease.Entry{}, err
	}
	if !leaseIdentity(entry, op) {
		return runtimelease.Entry{}, errSnapshotLeaseUnavailable
	}
	return entry, nil
}

func (g *dockerSnapshotLeaseClient) Arm(ctx context.Context, op snapshotOperation) error {
	return g.grant(ctx, op, true)
}
func (g *dockerSnapshotLeaseClient) Renew(ctx context.Context, op snapshotOperation) error {
	return g.grant(ctx, op, false)
}

func (g *dockerSnapshotLeaseClient) grant(ctx context.Context, op snapshotOperation, arm bool) error {
	unlock, err := g.lock(op)
	if err != nil {
		return err
	}
	defer unlock()
	if op.LeaseExpiresUnixMS <= time.Now().UnixMilli() {
		return errSnapshotLeaseUnavailable
	}
	old, err := g.status(ctx, op)
	if err != nil && !(arm && errors.Is(err, runtimelease.ErrNotFound)) {
		return errSnapshotLeaseUnavailable
	}
	if !arm && (old.Revoked || old.ExpiresUnixMS <= time.Now().UnixMilli()) {
		return errSnapshotLeaseUnavailable
	}
	if old.Revision > 0 && old.ExpiresUnixMS > op.LeaseExpiresUnixMS {
		return errSnapshotLeaseUnavailable
	}
	if old.Revision > 0 && !old.Revoked && old.ExpiresUnixMS == op.LeaseExpiresUnixMS {
		return nil
	}
	want := runtimelease.Entry{ContainerID: op.ContainerID, Scope: op.SessionID, Generation: op.Generation, Revision: old.Revision + 1, ExpiresUnixMS: op.LeaseExpiresUnixMS}
	action := "renew"
	if arm {
		action = "arm"
	}
	var got runtimelease.Entry
	if g.rpc(ctx, runtimelease.Request{Action: action, ExpectedRevision: old.Revision, Entry: want}, &got) != nil || got != want || got.ExpiresUnixMS <= time.Now().UnixMilli() {
		return errSnapshotLeaseUnavailable
	}
	return nil
}

func (g *dockerSnapshotLeaseClient) Check(ctx context.Context, op snapshotOperation) error {
	unlock, err := g.lock(op)
	if err != nil {
		return err
	}
	defer unlock()
	entry, err := g.status(ctx, op)
	if err != nil || entry.Revoked || entry.ExpiresUnixMS != op.LeaseExpiresUnixMS || entry.ExpiresUnixMS <= time.Now().UnixMilli() {
		return errSnapshotLeaseUnavailable
	}
	return nil
}

func (g *dockerSnapshotLeaseClient) Forget(ctx context.Context, op snapshotOperation) error {
	unlock, err := g.lock(op)
	if err != nil {
		return err
	}
	defer unlock()
	var entry runtimelease.Entry
	err = g.rpc(ctx, runtimelease.Request{Action: "status", Entry: runtimelease.Entry{ContainerID: op.ContainerID}}, &entry)
	if errors.Is(err, runtimelease.ErrNotFound) {
		// Even an absent status must pass through the guard's per-CID lock: an
		// earlier arm can have inspected the old root and still be committing.
		var reply struct {
			Forgotten bool `json:"forgotten"`
		}
		if g.rpc(ctx, runtimelease.Request{Action: "forget", Entry: runtimelease.Entry{ContainerID: op.ContainerID}}, &reply) != nil || !reply.Forgotten {
			return errSnapshotLeaseUnavailable
		}
		return nil
	}
	if err != nil {
		return errSnapshotLeaseUnavailable
	}
	// Destructive deletion may use a newer fencing generation than the stopped
	// native instance. Forget never touches a root: the daemon requires its ID
	// to be missing, and still checks this exact scope and record revision.
	if entry.ContainerID != op.ContainerID || entry.Scope != op.SessionID || entry.Generation <= 0 || entry.Generation > op.Generation || entry.Revision <= 0 || entry.Revision >= 1<<62 {
		return errSnapshotLeaseUnavailable
	}
	var reply struct {
		Forgotten bool `json:"forgotten"`
	}
	if g.rpc(ctx, runtimelease.Request{Action: "forget", ExpectedRevision: entry.Revision, Entry: entry}, &reply) != nil || !reply.Forgotten {
		return errSnapshotLeaseUnavailable
	}
	return nil
}

func (c *snapshotController) armExecutionLease(ctx context.Context, op snapshotOperation) error {
	if c.config.LeaseGuard == nil {
		return nil
	}
	return c.config.LeaseGuard.Arm(ctx, op)
}
func (c *snapshotController) checkExecutionLease(ctx context.Context, op snapshotOperation) error {
	if c.config.LeaseGuard == nil {
		return nil
	}
	return c.config.LeaseGuard.Check(ctx, op)
}
func (c *snapshotController) forgetExecutionLease(ctx context.Context, op snapshotOperation) error {
	if c.config.LeaseGuard == nil || op.ContainerID == "" {
		return nil
	}
	return c.config.LeaseGuard.Forget(ctx, op)
}
