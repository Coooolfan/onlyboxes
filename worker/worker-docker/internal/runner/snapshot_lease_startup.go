package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/runtimelease"
)

const leaseGuardComponent = "runtime-lease-guard"
const leaseGuardRunTmpfs = "rw,nosuid,nodev,noexec,size=8m,mode=755"

type snapshotLeaseBinding struct {
	StateID       string `json:"state_id,omitempty"`
	Version       int    `json:"version"`
	EngineID      string `json:"engine_id"`
	Token         string `json:"token"`
	ImageRef      string `json:"image_ref"`
	ImageID       string `json:"image_id"`
	Socket        string `json:"socket"`
	HelperName    string `json:"helper_name"`
	VolumeName    string `json:"volume_name"`
	VolumeCreated string `json:"volume_created,omitempty"`
	HelperID      string `json:"helper_id,omitempty"`
}

func (b snapshotLeaseBinding) valid(worker string) bool {
	token, err := uuid.Parse(b.Token)
	if err != nil || token.String() != b.Token || token.Version() != 4 {
		return false
	}
	hash := sha256.Sum256([]byte(worker))
	prefix := "onlyboxes-lease-" + hex.EncodeToString(hash[:12]) + "-" + b.Token[:12]
	if b.Version != 1 || !regexp.MustCompile(`^[A-Za-z0-9:_.-]{1,128}$`).MatchString(b.EngineID) || !dockerSnapshotDigest.MatchString(b.ImageID) || !pinnedLeaseImage(b.ImageRef) || !filepath.IsAbs(b.Socket) || strings.ContainsAny(b.Socket, "\x00,\r\n") || b.HelperName != prefix || b.VolumeName != prefix+"-state" || (b.HelperID != "" && !dockerSnapshotDigest.MatchString("sha256:"+b.HelperID)) {
		return false
	}
	if b.StateID != "" && (b.HelperID == "" || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(b.StateID)) {
		return false
	}
	if b.VolumeCreated != "" {
		if _, err := time.Parse(time.RFC3339Nano, b.VolumeCreated); err != nil {
			return false
		}
	}
	return b.HelperID == "" || b.VolumeCreated != ""
}

func pinnedLeaseImage(image string) bool {
	return dockerSnapshotDigest.MatchString(image) || regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`).MatchString(image)
}

func (j *snapshotJournal) bindLeaseGuard(b snapshotLeaseBinding) error {
	return j.transaction(func(state *snapshotJournalState) error {
		if !b.valid(state.WorkerID) {
			return errSnapshotJournalConflict
		}
		if prior := state.LeaseGuard; prior != nil {
			x, y := *prior, b
			x.VolumeCreated, y.VolumeCreated = "", ""
			x.HelperID, y.HelperID = "", ""
			x.StateID, y.StateID = "", ""
			if x != y || (prior.VolumeCreated != "" && prior.VolumeCreated != b.VolumeCreated) || (prior.HelperID != "" && prior.HelperID != b.HelperID) || (prior.StateID != "" && prior.StateID != b.StateID) {
				return errSnapshotJournalConflict
			}
		}
		state.LeaseGuard = &b
		return nil
	})
}

type leaseGuardVolume struct {
	Name, Driver, Scope, CreatedAt string
	Labels, Options                map[string]string
}

func (v leaseGuardVolume) matches(b snapshotLeaseBinding, worker string) bool {
	return v.Name == b.VolumeName && v.Driver == "local" && v.Scope == "local" && len(v.Options) == 0 && v.Labels["onlyboxes.component"] == leaseGuardComponent && v.Labels["onlyboxes.worker_id"] == worker && v.Labels["onlyboxes.lease_binding"] == b.Token && v.CreatedAt != "" && (b.VolumeCreated == "" || b.VolumeCreated == v.CreatedAt)
}

type leaseGuardNative struct {
	ID           string `json:"Id"`
	Image, Name  string
	RestartCount int
	State        struct {
		Running, Restarting bool
		Pid                 int
		StartedAt           string
	}
	Config struct {
		Image, User          string
		Entrypoint, Cmd, Env []string
		Labels               map[string]string
		Volumes              map[string]any
	}
	HostConfig struct {
		Init                                               *bool
		ReadonlyRootfs, Privileged                         bool
		NetworkMode, PidMode, IpcMode, UTSMode, UsernsMode string
		CapAdd, CapDrop, SecurityOpt, Binds                []string
		Devices                                            []any
		Tmpfs                                              map[string]string
		RestartPolicy                                      struct {
			Name              string
			MaximumRetryCount int
		}
	}
	Mounts []struct {
		Type, Name, Source, Destination string
		RW                              bool
	}
}

func (n leaseGuardNative) matches(b snapshotLeaseBinding, worker string) bool {
	if !dockerSnapshotDigest.MatchString("sha256:"+n.ID) || (b.HelperID != "" && n.ID != b.HelperID) || n.Image != b.ImageID || n.Name != "/"+b.HelperName || n.Config.Image != b.ImageID || n.Config.User != "0:0" || !reflect.DeepEqual(n.Config.Entrypoint, []string{"/runtime-lease-guard"}) || !reflect.DeepEqual(n.Config.Cmd, []string{"-worker-id", worker}) || len(n.Config.Volumes) != 0 || n.Config.Labels["onlyboxes.component"] != leaseGuardComponent || n.Config.Labels["onlyboxes.worker_id"] != worker || n.Config.Labels["onlyboxes.lease_binding"] != b.Token {
		return false
	}
	if n.HostConfig.Init == nil || !*n.HostConfig.Init || !n.HostConfig.ReadonlyRootfs || n.HostConfig.Privileged || n.HostConfig.NetworkMode != "none" || n.HostConfig.PidMode != "" || n.HostConfig.UsernsMode != "" || n.HostConfig.UTSMode != "" || len(n.HostConfig.CapAdd)+len(n.HostConfig.Devices)+len(n.HostConfig.Binds) != 0 || !reflect.DeepEqual(n.HostConfig.CapDrop, []string{"ALL"}) || !reflect.DeepEqual(n.HostConfig.SecurityOpt, []string{"no-new-privileges:true"}) || n.HostConfig.RestartPolicy.Name != "always" || n.HostConfig.RestartPolicy.MaximumRetryCount != 0 || len(n.HostConfig.Tmpfs) != 1 || n.HostConfig.Tmpfs["/run"] != leaseGuardRunTmpfs || len(n.Mounts) != 2 {
		return false
	}
	state, socket := false, false
	for _, m := range n.Mounts {
		switch m.Destination {
		case "/state":
			state = m.Type == "volume" && m.Name == b.VolumeName && m.RW
		case "/var/run/docker.sock":
			socket = m.Type == "bind" && m.Source == b.Socket && !m.RW
		default:
			return false
		}
	}
	return state && socket
}

// startSnapshotLeaseGuard preserves all resources on unknown outcomes. A
// persisted volume/container identity cannot be silently replaced on restart.
// The caller owns the Worker journal lock for this entire initialization.
func startSnapshotLeaseGuard(ctx context.Context, j *snapshotJournal, image, socket, platform string) (*dockerSnapshotLeaseClient, error) {
	if !pinnedLeaseImage(image) || !filepath.IsAbs(socket) || strings.ContainsAny(socket, "\x00,\r\n") {
		return nil, errSnapshotLeaseUnavailable
	}
	startup, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var daemon struct{ ID string }
	if dockerSnapshotJSON(startup, &daemon, "info", "--format", "{{json .}}") != nil {
		return nil, errSnapshotLeaseUnavailable
	}
	var metadata struct {
		ID               string `json:"Id"`
		Os, Architecture string
		Config           struct {
			User            string
			Entrypoint, Cmd []string
			Labels          map[string]string
		}
	}
	// Keep the engine's runnable image identity. With the containerd image
	// store, --platform can return a config digest that cannot be used as a
	// create reference and differs from Container.Image. Native-platform
	// selection is still verified against the daemon's exact runtime platform.
	if dockerSnapshotJSON(startup, &metadata, "image", "inspect", "--format", "{{json .}}", image) != nil || metadata.Os+"/"+metadata.Architecture != platform || metadata.Config.User != "0:0" || !reflect.DeepEqual(metadata.Config.Entrypoint, []string{"/runtime-lease-guard"}) || len(metadata.Config.Cmd) != 0 || metadata.Config.Labels["onlyboxes.component"] != leaseGuardComponent || metadata.Config.Labels["onlyboxes.runtime-lease-guard.schema"] != "2" {
		return nil, errSnapshotLeaseUnavailable
	}
	state, err := j.Snapshot()
	if err != nil {
		return nil, err
	}
	var binding snapshotLeaseBinding
	if state.LeaseGuard != nil {
		binding = *state.LeaseGuard
		if binding.StateID == "" {
			if len(state.Probes) > 0 {
				return nil, errSnapshotLeaseUnavailable
			}
			for _, id := range state.Current {
				if !state.Operations[id].terminal() {
					return nil, errSnapshotLeaseUnavailable
				}
			}
		}
		if !binding.valid(state.WorkerID) || binding.EngineID != daemon.ID || binding.ImageID != metadata.ID || binding.ImageRef != image || binding.Socket != socket {
			return nil, errSnapshotLeaseUnavailable
		}
	} else {
		if len(state.Probes) > 0 {
			return nil, errSnapshotLeaseUnavailable
		}
		for _, id := range state.Current {
			op := state.Operations[id]
			if !op.terminal() {
				return nil, errSnapshotLeaseUnavailable
			}
		}
		// Existing owner-labeled resources without the authoritative binding
		// are not an empty installation, including a lost Worker journal.
		for _, args := range [][]string{{"ps", "-a", "--filter", "label=onlyboxes.component=" + leaseGuardComponent, "--filter", "label=onlyboxes.worker_id=" + state.WorkerID, "--format", "{{.ID}}"}, {"volume", "ls", "--filter", "label=onlyboxes.component=" + leaseGuardComponent, "--filter", "label=onlyboxes.worker_id=" + state.WorkerID, "--format", "{{.Name}}"}} {
			var existing bytesBuffer
			if dockerSnapshotCLI(startup, nil, &existing, args...) != nil || strings.TrimSpace(existing.String()) != "" {
				return nil, errSnapshotLeaseUnavailable
			}
		}
		token := uuid.NewString()
		hash := sha256.Sum256([]byte(state.WorkerID))
		name := "onlyboxes-lease-" + hex.EncodeToString(hash[:12]) + "-" + token[:12]
		binding = snapshotLeaseBinding{Version: 1, EngineID: daemon.ID, Token: token, ImageRef: image, ImageID: metadata.ID, Socket: socket, HelperName: name, VolumeName: name + "-state"}
		if err := j.bindLeaseGuard(binding); err != nil {
			return nil, err
		}
	}
	var volumes bytesBuffer
	if dockerSnapshotCLI(startup, nil, &volumes, "volume", "ls", "--filter", "name="+binding.VolumeName, "--format", "{{.Name}}") != nil {
		return nil, errSnapshotLeaseUnavailable
	}
	volumeExists := false
	for _, name := range strings.Fields(volumes.String()) {
		if name == binding.VolumeName {
			volumeExists = true
		}
	}
	if !volumeExists {
		if binding.VolumeCreated != "" {
			return nil, errSnapshotLeaseUnavailable
		}
		if dockerSnapshotCLI(startup, nil, io.Discard, "volume", "create", "--driver", "local", "--label", "onlyboxes.component="+leaseGuardComponent, "--label", "onlyboxes.worker_id="+state.WorkerID, "--label", "onlyboxes.lease_binding="+binding.Token, binding.VolumeName) != nil {
			return nil, errSnapshotLeaseUnavailable
		}
	}
	var volume leaseGuardVolume
	if dockerSnapshotJSON(startup, &volume, "volume", "inspect", "--format", "{{json .}}", binding.VolumeName) != nil || !volume.matches(binding, state.WorkerID) {
		return nil, errSnapshotLeaseUnavailable
	}
	if binding.VolumeCreated == "" {
		binding.VolumeCreated = volume.CreatedAt
		if err := j.bindLeaseGuard(binding); err != nil {
			return nil, err
		}
	}
	var ids bytesBuffer
	if dockerSnapshotCLI(startup, nil, &ids, "ps", "-a", "--no-trunc", "--filter", "name=^/"+binding.HelperName+"$", "--format", "{{.ID}}") != nil {
		return nil, errSnapshotLeaseUnavailable
	}
	id := strings.TrimSpace(ids.String())
	if id == "" {
		if binding.HelperID != "" {
			return nil, errSnapshotLeaseUnavailable
		}
		args := []string{"create", "--platform", platform, "--name", binding.HelperName, "--init", "--restart", "always", "--read-only", "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--tmpfs", "/run:" + leaseGuardRunTmpfs, "--mount", "type=volume,source=" + binding.VolumeName + ",target=/state", "--mount", "type=bind,source=" + socket + ",target=/var/run/docker.sock,readonly", "--label", "onlyboxes.component=" + leaseGuardComponent, "--label", "onlyboxes.worker_id=" + state.WorkerID, "--label", "onlyboxes.lease_binding=" + binding.Token, binding.ImageID, "-worker-id", state.WorkerID}
		var created bytesBuffer
		if dockerSnapshotCLI(startup, nil, &created, args...) != nil {
			return nil, errSnapshotLeaseUnavailable
		}
		id = strings.TrimSpace(created.String())
	}
	if !dockerSnapshotDigest.MatchString("sha256:" + id) {
		return nil, errSnapshotLeaseUnavailable
	}
	var native leaseGuardNative
	if dockerSnapshotJSON(startup, &native, "inspect", "--format", "{{json .}}", id) != nil || !native.matches(binding, state.WorkerID) {
		return nil, errSnapshotLeaseUnavailable
	}
	if binding.HelperID == "" {
		binding.HelperID = native.ID
		if err := j.bindLeaseGuard(binding); err != nil {
			return nil, err
		}
	}
	if !native.State.Running {
		if dockerSnapshotCLI(startup, nil, io.Discard, "start", binding.HelperID) != nil {
			return nil, errSnapshotLeaseUnavailable
		}
	}
	client, err := newDockerSnapshotLeaseClient(binding.HelperID)
	if err != nil {
		return nil, err
	}
	var observed string
	var since time.Time
	for {
		if dockerSnapshotJSON(startup, &native, "inspect", "--format", "{{json .}}", binding.HelperID) != nil || !native.matches(binding, state.WorkerID) {
			return nil, errSnapshotLeaseUnavailable
		}
		identity := native.State.StartedAt + "/" + strconv.Itoa(native.State.Pid) + "/" + strconv.Itoa(native.RestartCount)
		if !native.State.Running || native.State.Restarting || native.State.Pid <= 0 {
			observed = ""
			since = time.Time{}
		} else if observed != identity {
			observed = identity
			since = time.Now()
		}
		if !since.IsZero() && time.Since(since) >= 10*time.Second {
			var ready runtimelease.Identity
			if client.rpc(startup, runtimelease.Request{Action: "ping"}, &ready) != nil || !ready.Ready || ready.WorkerID != state.WorkerID || ready.EngineID != binding.EngineID || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(ready.StateID) || (binding.StateID != "" && binding.StateID != ready.StateID) {
				return nil, errSnapshotLeaseUnavailable
			}
			if binding.StateID == "" {
				binding.StateID = ready.StateID
				if err := j.bindLeaseGuard(binding); err != nil {
					return nil, err
				}
			}
			var confirmed leaseGuardNative
			if dockerSnapshotJSON(startup, &confirmed, "inspect", "--format", "{{json .}}", binding.HelperID) != nil || !confirmed.matches(binding, state.WorkerID) || confirmed.State != native.State || confirmed.RestartCount != native.RestartCount {
				return nil, errSnapshotLeaseUnavailable
			}
			return client, nil
		}
		select {
		case <-startup.Done():
			return nil, errSnapshotLeaseUnavailable
		case <-time.After(200 * time.Millisecond):
		}
	}
}
