package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLeaseGuardStartupRejectsChangedDaemonBeforeResourceMutation(t *testing.T) {
	j := testSnapshotJournal(t)
	binding := testLeaseBinding("worker-a")
	if err := j.bindLeaseGuard(binding); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	info := filepath.Join(root, "info.json")
	image := filepath.Join(root, "image.json")
	marker := filepath.Join(root, "mutation")
	if os.WriteFile(info, []byte(`{"ID":"another-engine"}`), 0600) != nil {
		t.Fatal("fixture")
	}
	metadata := map[string]any{"Id": binding.ImageID, "Os": "linux", "Architecture": "arm64", "Config": map[string]any{"User": "0:0", "Entrypoint": []string{"/runtime-lease-guard"}, "Labels": map[string]string{"onlyboxes.component": leaseGuardComponent, "onlyboxes.runtime-lease-guard.schema": "2"}}}
	data, _ := json.Marshal(metadata)
	if os.WriteFile(image, data, 0600) != nil {
		t.Fatal("fixture")
	}
	t.Setenv("ONLYBOXES_LEASE_INFO_FIXTURE", info)
	t.Setenv("ONLYBOXES_LEASE_IMAGE_FIXTURE", image)
	t.Setenv("ONLYBOXES_LEASE_MUTATION_FIXTURE", marker)
	script := "#!/bin/sh\ncase \"$1\" in info) /bin/cat \"$ONLYBOXES_LEASE_INFO_FIXTURE\" ;; image) /bin/cat \"$ONLYBOXES_LEASE_IMAGE_FIXTURE\" ;; *) /usr/bin/touch \"$ONLYBOXES_LEASE_MUTATION_FIXTURE\";exit 99 ;; esac\n"
	if os.WriteFile(filepath.Join(root, "docker"), []byte(script), 0700) != nil {
		t.Fatal("fixture executable")
	}
	t.Setenv("PATH", root)
	if _, err := startSnapshotLeaseGuard(context.Background(), j, binding.ImageRef, binding.Socket, "linux/arm64"); err == nil {
		t.Fatal("changed daemon accepted old ownership")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("daemon mismatch changed native resources")
	}
	state, _ := j.Snapshot()
	if *state.LeaseGuard != binding {
		t.Fatal("daemon mismatch changed binding")
	}
	unfinished := testSnapshotJournal(t)
	if err := unfinished.Claim(testSnapshotOperation()); err != nil {
		t.Fatal(err)
	}
	if _, err := startSnapshotLeaseGuard(context.Background(), unfinished, binding.ImageRef, binding.Socket, "linux/arm64"); err == nil {
		t.Fatal("unfinished unguarded ownership was promoted to an empty guard")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("unguarded ownership changed native resources")
	}
	pending, _ := unfinished.Snapshot()
	if pending.LeaseGuard != nil {
		t.Fatal("unguarded ownership acquired an empty binding")
	}
}

func testLeaseBinding(worker string) snapshotLeaseBinding {
	token := uuid.NewString()
	hash := sha256.Sum256([]byte(worker))
	name := "onlyboxes-lease-" + hex.EncodeToString(hash[:12]) + "-" + token[:12]
	return snapshotLeaseBinding{Version: 1, EngineID: "engine-test", Token: token, ImageRef: "sha256:" + strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("a", 64), Socket: "/var/run/docker.sock", HelperName: name, VolumeName: name + "-state"}
}

func TestLeaseGuardBindingSurvivesReopenAndRejectsResourceReplacement(t *testing.T) {
	j := testSnapshotJournal(t)
	binding := testLeaseBinding("worker-a")
	if err := j.bindLeaseGuard(binding); err != nil {
		t.Fatal(err)
	}
	binding.VolumeCreated = time.Now().UTC().Format(time.RFC3339Nano)
	if err := j.bindLeaseGuard(binding); err != nil {
		t.Fatal(err)
	}
	binding.HelperID = strings.Repeat("b", 64)
	binding.StateID = strings.Repeat("e", 32)
	if err := j.bindLeaseGuard(binding); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err := openSnapshotJournal(j.root, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	state, _ := j.Snapshot()
	if state.LeaseGuard == nil || *state.LeaseGuard != binding {
		t.Fatal("durable helper binding was lost")
	}
	for _, change := range []func(*snapshotLeaseBinding){
		func(b *snapshotLeaseBinding) { b.StateID = strings.Repeat("f", 32) },
		func(b *snapshotLeaseBinding) { b.EngineID = "another-engine" }, func(b *snapshotLeaseBinding) {
			b.VolumeCreated = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
		}, func(b *snapshotLeaseBinding) { b.HelperID = strings.Repeat("c", 64) }, func(b *snapshotLeaseBinding) { b.Socket = "/another/docker.sock" }, func(b *snapshotLeaseBinding) { b.ImageRef = "sha256:" + strings.Repeat("d", 64) },
	} {
		altered := binding
		change(&altered)
		if j.bindLeaseGuard(altered) == nil {
			t.Fatal("pinned helper ownership was replaced")
		}
	}
	state, _ = j.Snapshot()
	if *state.LeaseGuard != binding {
		t.Fatal("rejected replacement changed durable ownership")
	}
}

func TestLeaseGuardBindingWriteFailureCannotAcknowledgeOwnership(t *testing.T) {
	j := testSnapshotJournal(t)
	j.persist = func(snapshotJournalState) error { return errSnapshotJournalUnavailable }
	if j.bindLeaseGuard(testLeaseBinding("worker-a")) == nil {
		t.Fatal("failed helper intent was acknowledged")
	}
	if _, err := j.Snapshot(); err == nil {
		t.Fatal("uncertain helper binding reopened admission")
	}
}
