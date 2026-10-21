package runtimelease

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This child only issues one lease control. Its SIGKILL demonstrates that
// enforcement does not depend on the control issuer's process or timers.
func TestRuntimeLeaseControllerChild(t *testing.T) {
	input := os.Getenv("ONLYBOXES_LEASE_CONTROLLER_INPUT")
	if input == "" {
		t.Skip("owned subprocess only")
	}
	data, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("docker", "exec", "-i", os.Getenv("ONLYBOXES_LEASE_HELPER"), "/runtime-lease-guard", "-mode", "control")
	command.Stdin = bytes.NewReader(data)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("control failed: %s %v", output, err)
	}
	fmt.Println("armed")
	select {}
}

func TestIntegrationDockerIndependentLease(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1 and DOCKER_SNAPSHOT_HELPER_IMAGE")
	}
	image := os.Getenv("DOCKER_SNAPSHOT_HELPER_IMAGE")
	if !strings.Contains(image, "@sha256:") {
		t.Fatal("pinned fixture image required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	docker := func(input []byte, args ...string) ([]byte, error) {
		c := exec.CommandContext(ctx, "docker", args...)
		c.Stdin = bytes.NewReader(input)
		return c.CombinedOutput()
	}
	run := func(input []byte, args ...string) []byte {
		t.Helper()
		out, err := docker(input, args...)
		if err != nil {
			t.Fatalf("Docker fixture operation failed: %s %v", out, err)
		}
		return out
	}
	arch := strings.TrimSpace(string(run(nil, "version", "--format", "{{.Server.Arch}}")))
	if arch != "arm64" && arch != "amd64" {
		t.Fatal("unsupported fixture architecture")
	}
	// Build only the auxiliary fixture/helper, never start a Worker service.
	binary := filepath.Join(t.TempDir(), "runtime-lease-guard")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/runtime-lease-guard")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %s %v", out, err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	helper := "onlyboxes-lease-helper-" + suffix
	source := "onlyboxes-lease-source-" + suffix
	volume := helper + "-state"
	scope := "obx:lease-test:" + suffix
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "docker", "rm", "-f", source, helper).Run()
		_ = exec.CommandContext(cleanup, "docker", "volume", "rm", volume).Run()
	})
	hash := sha256.Sum256([]byte(scope))
	worker := "lease-test-worker"
	cid := strings.TrimSpace(string(run(nil, "create", "--name", source, "--label", "onlyboxes.managed=true", "--label", "onlyboxes.worker_id="+worker, "--label", "onlyboxes.capability=terminalExec", "--label", "onlyboxes.session_id_hash="+hex.EncodeToString(hash[:]), "--stop-signal", "SIGUSR1", "--entrypoint", "sh", image, "-c", "trap 'echo caught >> /caught-stop-signal' TERM USR1; while :; do echo write >> /lease-writes; sleep 0.1; done")))
	run(nil, "create", "--name", helper, "--init", "--restart", "always", "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock", "--mount", "type=volume,source="+volume+",target=/state", "--entrypoint", "/runtime-lease-guard", image, "-worker-id", worker)
	run(nil, "cp", binary, helper+":/runtime-lease-guard")
	run(nil, "start", helper)
	// Docker activates restart supervision after a successful ten-second run.
	// No sandbox lease is issued before that stabilization barrier.
	time.Sleep(11 * time.Second)
	control := func(request Request) (Entry, error) {
		data, _ := json.Marshal(request)
		out, err := docker(data, "exec", "-i", helper, "/runtime-lease-guard", "-mode", "control")
		if err != nil {
			return Entry{}, err
		}
		var entry Entry
		err = json.Unmarshal(out, &entry)
		return entry, err
	}
	e := Entry{ContainerID: cid, Scope: scope, Generation: 1, Revision: 1, ExpiresUnixMS: time.Now().Add(6 * time.Second).UnixMilli()}
	request := Request{Action: "arm", Entry: e}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := control(request); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("guardian never accepted lease")
		}
		time.Sleep(50 * time.Millisecond)
	}
	input := filepath.Join(t.TempDir(), "lease.json")
	data, _ := json.Marshal(request)
	if os.WriteFile(input, data, 0600) != nil {
		t.Fatal("write input")
	}
	child := exec.Command(os.Args[0], "-test.run=^TestRuntimeLeaseControllerChild$")
	child.Env = append(os.Environ(), "ONLYBOXES_LEASE_CONTROLLER_INPUT="+input, "ONLYBOXES_LEASE_HELPER="+helper)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if child.Start() != nil {
		t.Fatal("start issuer child")
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	armed := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			armed <- scanner.Text()
		} else {
			armed <- ""
		}
	}()
	select {
	case line := <-armed:
		if line != "armed" {
			t.Fatal("issuer did not arm")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("issuer timeout")
	}
	run(nil, "start", cid)
	time.Sleep(300 * time.Millisecond)
	run(nil, "pause", cid)
	if child.Process.Kill() != nil {
		t.Fatal("kill issuer")
	}
	_ = child.Wait()
	state := func() string {
		return strings.TrimSpace(string(run(nil, "inspect", "--format", "{{.State.Status}}", cid)))
	}
	stopDeadline := time.UnixMilli(e.ExpiresUnixMS).Add(5 * time.Second)
	for state() != "exited" {
		if time.Now().After(stopDeadline) {
			t.Fatal("independent guard did not stop paused root after issuer SIGKILL")
		}
		time.Sleep(50 * time.Millisecond)
	}
	retained := filepath.Join(t.TempDir(), "writes")
	run(nil, "cp", cid+":/lease-writes", retained)
	writes, err := os.ReadFile(retained)
	if err != nil || len(writes) == 0 {
		t.Fatal("writable root was lost")
	}
	if _, err := docker(nil, "cp", cid+":/caught-stop-signal", filepath.Join(t.TempDir(), "caught")); err == nil {
		t.Fatal("lease expiry ran a sandbox stop-signal handler")
	}
	status, err := control(Request{Action: "status", Entry: Entry{ContainerID: cid}})
	if err != nil || !status.Revoked || status.Revision <= e.Revision {
		t.Fatal("revocation was not durable", status, err)
	}
	// A late start cannot escape an already-revoked immutable ID.
	run(nil, "start", cid)
	deadline = time.Now().Add(3 * time.Second)
	for state() != "exited" {
		if time.Now().After(deadline) {
			t.Fatal("late start escaped expired guard")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Restart the guardian after an explicitly authorized rollback of the same root.
	e.Revision = status.Revision + 1
	e.ExpiresUnixMS = time.Now().Add(30 * time.Second).UnixMilli()
	if _, err := control(Request{Action: "arm", ExpectedRevision: status.Revision, Entry: e}); err != nil {
		t.Fatal("rearm", err)
	}
	run(nil, "start", cid)
	// Kill the actual guard child of Docker's init, rather than marking the
	// container manually stopped through the Docker kill API.
	run(nil, "exec", helper, "sh", "-c", `set -- $(cat /proc/1/task/1/children); [ "$#" -eq 1 ] || exit 1; kill -9 "$1"`)
	deadline = time.Now().Add(8 * time.Second)
	restarted := false
	for time.Now().Before(deadline) {
		status, err = control(Request{Action: "status", Entry: Entry{ContainerID: cid}})
		if err == nil && status.Revoked && status.Revision > e.Revision && state() == "exited" {
			restarted = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !restarted {
		t.Logf("guardian state: %s", run(nil, "inspect", "--format", "{{json .State}} {{.RestartCount}}", helper))
		t.Logf("guardian diagnostics: %s", run(nil, "logs", helper))
		t.Fatal("guardian restart did not revoke old execution")
	}
	t.Log("issuer SIGKILL, paused expiry, retained writable root, late start fencing and guardian restart verified")
}
