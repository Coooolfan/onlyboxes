package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

var (
	errSnapshotRuntimeUnsupported = errors.New("Docker snapshot configuration is unsupported")
	errSnapshotArchiveIntegrity   = errors.New("snapshot archive integrity check failed")
	errSnapshotArchiveQuota       = errors.New("snapshot archive storage budget exceeded")
	dockerSnapshotDigest          = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// This adapter only owns backend I/O. Its caller must durably fence the session,
// drain accepted commands and protect its source from ordinary lease/orphan
// cleanup before Freeze. It never removes a source on export failure.
type dockerSnapshotRuntime struct {
	helperImage string
	version     string
	platform    string
}

type dockerSnapshotInstance struct {
	ID     string
	Name   string
	PID    int
	State  string
	Paused bool
	Spec   wire.RestoreSpec
}

type dockerSnapshotContainer struct {
	ID    string `json:"Id"`
	Image string
	Name  string
	State struct {
		Status  string
		Running bool
		Paused  bool
		Pid     int
	}
	Config struct {
		Env        []string
		Entrypoint []string
		Cmd        []string
		User       string
		WorkingDir string
		Labels     map[string]string
		Volumes    map[string]json.RawMessage
	}
	HostConfig struct {
		Memory         int64
		NanoCpus       int64
		PidsLimit      *int64
		Privileged     bool
		ReadonlyRootfs bool
		CapAdd         []string
		CapDrop        []string
		SecurityOpt    []string
		Devices        []json.RawMessage
	}
	Mounts []json.RawMessage
}

func newDockerSnapshotRuntime(ctx context.Context, helperImage string) (*dockerSnapshotRuntime, error) {
	// An immutable, administrator-provided helper supplies GNU tar. Sandbox
	// binaries are never trusted to produce the persisted archive.
	probeSpec := wire.RestoreSpec{Runtime: "docker", RuntimeVersion: "probe", Platform: "linux/amd64", Format: wire.DockerRootFSFormat, Image: helperImage, CPUs: 1, MemoryMiB: 1, MaxProcesses: 1}
	if probeSpec.Validate() != nil || strings.HasPrefix(helperImage, "-") {
		return nil, errSnapshotRuntimeUnsupported
	}
	var server struct {
		Version string
		Os      string
		Arch    string
	}
	if err := dockerSnapshotJSON(ctx, &server, "version", "--format", "{{json .Server}}"); err != nil {
		return nil, err
	}
	platform := server.Os + "/" + server.Arch
	probeSpec.RuntimeVersion, probeSpec.Platform = server.Version, platform
	if probeSpec.Validate() != nil {
		return nil, errSnapshotRuntimeUnsupported
	}
	var securityOptions []string
	if err := dockerSnapshotJSON(ctx, &securityOptions, "info", "--format", "{{json .SecurityOptions}}"); err != nil {
		return nil, err
	}
	for _, option := range securityOptions {
		if strings.Contains(option, "rootless") || strings.Contains(option, "userns") {
			return nil, errSnapshotRuntimeUnsupported
		}
	}
	r := &dockerSnapshotRuntime{helperImage: helperImage, version: server.Version, platform: platform}
	var helper struct {
		ID string `json:"Id"`
	}
	if err := dockerSnapshotJSON(ctx, &helper, "image", "inspect", "--platform", platform, "--format", "{{json .}}", helperImage); err != nil {
		return nil, err
	}
	var version bytes.Buffer
	if err := dockerSnapshotCLI(ctx, nil, &version, "run", "--rm", "--network", "none", "--read-only", "--entrypoint", "tar", helperImage, "--version"); err != nil || !strings.Contains(version.String(), "GNU tar") {
		return nil, errSnapshotRuntimeUnsupported
	}
	return r, nil
}

func dockerSnapshotJSON(ctx context.Context, value any, args ...string) error {
	var out bytes.Buffer
	limited := snapshotBoundedWriter{writer: &out, remaining: 128 << 10}
	if err := dockerSnapshotCLI(ctx, nil, &limited, args...); err != nil {
		return err
	}
	if json.Unmarshal(out.Bytes(), value) != nil {
		return errSnapshotRuntimeUnsupported
	}
	return nil
}

// CLI errors deliberately omit daemon output, command arguments and private
// metadata. In particular, environment variables must never reach error logs.
func dockerSnapshotCLI(ctx context.Context, input io.Reader, output io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin, cmd.Stdout = input, output
	stderr := &snapshotDiagnosticWriter{limit: 16 << 10}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errSnapshotArchiveQuota) {
			return errSnapshotArchiveQuota
		}
		return errors.New("Docker snapshot backend command failed")
	}
	// GNU tar may silently skip unsupported objects (for example sockets) while
	// returning success. Warnings invalidate an archive rather than losing data.
	if len(args) > 0 && args[0] == "run" && stderr.written > 0 {
		return errSnapshotRuntimeUnsupported
	}
	return nil
}

type snapshotDiagnosticWriter struct {
	limit   int
	written int
}

func (w *snapshotDiagnosticWriter) Write(p []byte) (int, error) {
	// Count diagnostics without retaining sandbox-controlled file names.
	w.written += min(len(p), w.limit-w.written)
	return len(p), nil
}

type snapshotBoundedWriter struct {
	writer    io.Writer
	remaining int64
	exceeded  bool
}

func (w *snapshotBoundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		w.exceeded = true
		return 0, errSnapshotArchiveQuota
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func (r *dockerSnapshotRuntime) Describe(ctx context.Context, name, sessionID string) (dockerSnapshotInstance, error) {
	return r.describe(ctx, name, sessionID, nil)
}

// DescribePersistent uses a journal-owned immutable specification when the
// current image came from an earlier full-rootfs import rather than a registry.
func (r *dockerSnapshotRuntime) DescribePersistent(ctx context.Context, name, sessionID string, expected wire.RestoreSpec) (dockerSnapshotInstance, error) {
	if expected.Validate() != nil {
		return dockerSnapshotInstance{}, errSnapshotRuntimeUnsupported
	}
	return r.describe(ctx, name, sessionID, &expected)
}

func (r *dockerSnapshotRuntime) describe(ctx context.Context, name, sessionID string, expected *wire.RestoreSpec) (dockerSnapshotInstance, error) {
	if sessionID == "" || (name != terminalSessionResourceName(sessionID) && !dockerSnapshotDigest.MatchString("sha256:"+name)) {
		return dockerSnapshotInstance{}, errSnapshotRuntimeUnsupported
	}
	var c dockerSnapshotContainer
	if err := dockerSnapshotJSON(ctx, &c, "inspect", "--format", "{{json .}}", name); err != nil {
		return dockerSnapshotInstance{}, err
	}
	if c.Config.Labels[terminalExecSessionLabelKey] != terminalSessionIDHash(sessionID) || c.Config.Labels[terminalExecSchemaLabelKey] != terminalExecSchemaVersion || c.Config.Labels["onlyboxes.managed"] != "true" || !dockerSnapshotDigest.MatchString(c.Image) || len(c.Mounts) != 0 || len(c.Config.Volumes) != 0 || c.HostConfig.Privileged || c.HostConfig.ReadonlyRootfs || len(c.HostConfig.CapAdd)+len(c.HostConfig.CapDrop)+len(c.HostConfig.SecurityOpt)+len(c.HostConfig.Devices) != 0 || c.HostConfig.Memory <= 0 || c.HostConfig.Memory%(1<<20) != 0 || c.HostConfig.PidsLimit == nil || *c.HostConfig.PidsLimit <= 0 || *c.HostConfig.PidsLimit > 1<<31-1 {
		return dockerSnapshotInstance{}, errSnapshotRuntimeUnsupported
	}
	var image struct {
		RepoDigests  []string
		Os           string
		Architecture string
		Config       struct{ Labels map[string]string }
		Descriptor   struct {
			Digest    string
			MediaType string
		}
	}
	if err := dockerSnapshotJSON(ctx, &image, "image", "inspect", "--platform", r.platform, "--format", "{{json .}}", c.Image); err != nil {
		return dockerSnapshotInstance{}, err
	}
	if image.Os+"/"+image.Architecture != r.platform || (image.Descriptor.MediaType != "application/vnd.oci.image.manifest.v1+json" && image.Descriptor.MediaType != "application/vnd.docker.distribution.manifest.v2+json") || !dockerSnapshotDigest.MatchString(image.Descriptor.Digest) {
		return dockerSnapshotInstance{}, errSnapshotRuntimeUnsupported
	}
	var pinnedImage string
	if expected != nil && dockerSnapshotDigest.MatchString("sha256:"+image.Config.Labels["onlyboxes.snapshot.sha256"]) {
		pinnedImage = expected.Image
	} else {
		if len(image.RepoDigests) == 0 {
			return dockerSnapshotInstance{}, errSnapshotRuntimeUnsupported
		}
		repository, _, ok := strings.Cut(image.RepoDigests[0], "@sha256:")
		if !ok || repository == "" {
			return dockerSnapshotInstance{}, errSnapshotRuntimeUnsupported
		}
		pinnedImage = repository + "@" + image.Descriptor.Digest
	}
	env := make(map[string]string, len(c.Config.Env))
	for _, item := range c.Config.Env {
		key, value, ok := strings.Cut(item, "=")
		if _, duplicate := env[key]; !ok || key == "" || duplicate {
			return dockerSnapshotInstance{}, errSnapshotRuntimeUnsupported
		}
		env[key] = value
	}
	spec := wire.RestoreSpec{Runtime: "docker", RuntimeVersion: r.version, Platform: r.platform, Format: wire.DockerRootFSFormat, Image: pinnedImage, MemoryMiB: c.HostConfig.Memory >> 20, CPUs: float64(c.HostConfig.NanoCpus) / 1e9, MaxProcesses: int32(*c.HostConfig.PidsLimit), User: c.Config.User, WorkingDir: c.Config.WorkingDir, Env: env, Entrypoint: c.Config.Entrypoint, Command: c.Config.Cmd}
	if spec.Validate() != nil || c.ID == "" {
		return dockerSnapshotInstance{}, errSnapshotRuntimeUnsupported
	}
	if expected != nil {
		actual, _ := json.Marshal(spec)
		want, _ := json.Marshal(expected)
		if !bytes.Equal(actual, want) {
			return dockerSnapshotInstance{}, errSnapshotRuntimeUnsupported
		}
	}
	return dockerSnapshotInstance{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), PID: c.State.Pid, State: c.State.Status, Paused: c.State.Paused, Spec: spec}, nil
}

func (r *dockerSnapshotRuntime) Freeze(ctx context.Context, instance dockerSnapshotInstance) (dockerSnapshotInstance, error) {
	if !dockerSnapshotDigest.MatchString("sha256:"+instance.ID) || (instance.State != "running" && instance.State != "paused") {
		return instance, errSnapshotRuntimeUnsupported
	}
	if !instance.Paused {
		if err := dockerSnapshotCLI(ctx, nil, io.Discard, "pause", instance.ID); err != nil {
			return instance, err
		}
	}
	var state struct {
		Pid    int
		Paused bool
	}
	if err := dockerSnapshotJSON(ctx, &state, "inspect", "--format", "{{json .State}}", instance.ID); err != nil {
		return instance, err
	}
	if !state.Paused || state.Pid <= 0 {
		return instance, errSnapshotRuntimeUnsupported
	}
	instance.PID, instance.Paused, instance.State = state.Pid, true, "paused"
	return instance, nil
}

// Export writes a private archive file; only a fully synced, validated archive
// is returned. Any failure leaves the frozen source intact for reconciliation.
func (r *dockerSnapshotRuntime) Export(ctx context.Context, instance dockerSnapshotInstance, snapshotID, directory string, maxBytes, maxExpandedBytes int64) (wire.Archive, string, error) {
	probe := wire.Archive{SnapshotID: snapshotID, Size: 1, ExpandedBytes: 1, SHA256: strings.Repeat("0", 64), Format: wire.DockerRootFSFormat}
	if probe.Validate() != nil || !instance.Paused || instance.PID <= 0 || !dockerSnapshotDigest.MatchString("sha256:"+instance.ID) || instance.Spec.Validate() != nil || instance.Spec.Runtime != "docker" || instance.Spec.RuntimeVersion != r.version || instance.Spec.Platform != r.platform || maxBytes <= 0 || maxExpandedBytes <= 0 || maxBytes > 1<<60 || maxExpandedBytes > 1<<60 {
		return wire.Archive{}, "", errSnapshotRuntimeUnsupported
	}
	file, err := os.CreateTemp(directory, snapshotExportTempPrefix(snapshotID)+"*.tar")
	if err != nil {
		return wire.Archive{}, "", err
	}
	fileName := file.Name()
	succeeded := false
	defer func() {
		_ = file.Close()
		if !succeeded {
			_ = os.Remove(fileName)
		}
	}()
	helperName := snapshotExportHelperName(instance.ID, snapshotID)
	if err := removeSnapshotExportHelper(ctx, instance.ID, snapshotID); err != nil {
		return wire.Archive{}, "", err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "rm", "-f", helperName)
	}()
	hash := sha256.New()
	bounded := &snapshotBoundedWriter{writer: io.MultiWriter(file, hash), remaining: maxBytes}
	args := []string{"run", "--name", helperName, "--rm", "--label", "onlyboxes.snapshot.export_source=" + instance.ID, "--label", "onlyboxes.snapshot.export_operation=" + snapshotID, "--privileged", "--user", "0:0", "--pid=container:" + instance.ID, "--network", "none", "--read-only", "--entrypoint", "tar", r.helperImage, "--format=pax", "--pax-option=delete=atime,delete=ctime", "--numeric-owner", "--xattrs", "--xattrs-include=*", "--acls", "--sparse", "--one-file-system", "-C", "/proc/1/root", "--exclude=./proc", "--exclude=./sys", "--exclude=./dev", "--exclude=./tmp/*", "--exclude=./run/*", "--exclude=./etc/hostname", "--exclude=./etc/hosts", "--exclude=./etc/resolv.conf", "-cpf", "-", "."}
	err = dockerSnapshotCLI(ctx, nil, bounded, args...)
	if bounded.exceeded {
		err = errSnapshotArchiveQuota
	}
	if err != nil {
		return wire.Archive{}, "", err
	}
	if err := file.Sync(); err != nil {
		return wire.Archive{}, "", err
	}
	expanded, err := validateDockerSnapshotTar(ctx, file, maxExpandedBytes)
	if err != nil {
		return wire.Archive{}, "", err
	}
	stat, err := file.Stat()
	if err != nil {
		return wire.Archive{}, "", err
	}
	archive := wire.Archive{SnapshotID: snapshotID, Size: stat.Size(), ExpandedBytes: expanded, SHA256: hex.EncodeToString(hash.Sum(nil)), Format: wire.DockerRootFSFormat}
	if archive.Validate() != nil {
		return wire.Archive{}, "", errSnapshotArchiveIntegrity
	}
	succeeded = true
	return archive, fileName, nil
}

// validateDockerSnapshotTar budgets block-rounded logical regular-file sizes
// (hardlinks once), including sparse extents, plus 4 KiB per directory entry.
// Docker import can densify sparse files. Archive bytes are limited separately.
func validateDockerSnapshotTar(ctx context.Context, file *os.File, limit int64) (int64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	reader := tar.NewReader(snapshotContextReader{ctx: ctx, reader: file})
	var expanded int64
	for entries := 0; ; entries++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if entries == 0 {
				return 0, errSnapshotArchiveIntegrity
			}
			return expanded, nil
		}
		if err != nil || entries >= 1_000_000 || !safeSnapshotTarPath(header.Name) {
			return 0, errSnapshotArchiveIntegrity
		}
		if expanded > limit-4096 {
			return 0, errSnapshotArchiveQuota
		}
		expanded += 4096
		switch header.Typeflag {
		case tar.TypeReg, tar.TypeRegA, tar.TypeGNUSparse:
			if header.Size < 0 || header.Size > limit-expanded || header.Size > 1<<60 {
				return 0, errSnapshotArchiveQuota
			}
			blocks := (header.Size + 4095) / 4096 * 4096
			if blocks > limit-expanded {
				return 0, errSnapshotArchiveQuota
			}
			expanded += blocks
		case tar.TypeDir, tar.TypeSymlink:
		case tar.TypeLink:
			if !safeSnapshotTarPath(header.Linkname) {
				return 0, errSnapshotArchiveIntegrity
			}
		case tar.TypeFifo:
		default:
			return 0, errSnapshotRuntimeUnsupported
		}
	}
}

func safeSnapshotTarPath(name string) bool {
	return name != "" && len(name) <= 4096 && !strings.ContainsRune(name, '\x00') && !strings.HasPrefix(name, "/") && path.Clean(name) != ".." && !strings.HasPrefix(path.Clean(name), "../")
}

// Import reads from the same fixed file descriptor used for verification. It
// does not start a container or grant execution ownership.
func (r *dockerSnapshotRuntime) Import(ctx context.Context, file *os.File, archive wire.Archive, spec wire.RestoreSpec, tag string) (string, error) {
	if archive.Validate() != nil || spec.Validate() != nil || spec.Runtime != "docker" || spec.RuntimeVersion != r.version || spec.Platform != r.platform || archive.Format != spec.Format || !strings.HasPrefix(tag, "onlyboxes-snapshot-") || strings.ContainsAny(tag, " \t\r\n") {
		return "", errSnapshotRuntimeUnsupported
	}
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != archive.Size {
		return "", errSnapshotArchiveIntegrity
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, snapshotContextReader{ctx: ctx, reader: file})
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || n != archive.Size || hex.EncodeToString(hash.Sum(nil)) != archive.SHA256 {
		return "", errSnapshotArchiveIntegrity
	}
	expanded, err := validateDockerSnapshotTar(ctx, file, archive.ExpandedBytes)
	if err != nil {
		return "", err
	}
	if expanded != archive.ExpandedBytes {
		return "", errSnapshotArchiveIntegrity
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := dockerSnapshotCLI(ctx, file, &out, "import", "--platform", r.platform, "--change", "LABEL onlyboxes.snapshot.sha256="+archive.SHA256, "-", tag); err != nil {
		return "", err
	}
	id := strings.TrimSpace(out.String())
	if !dockerSnapshotDigest.MatchString(id) {
		return "", errSnapshotArchiveIntegrity
	}
	return id, nil
}

func dockerSnapshotCreateArgs(name, sessionID, imageID, operationID string, generation int64, spec wire.RestoreSpec, network string) ([]string, error) {
	if spec.Validate() != nil || spec.Runtime != "docker" || !dockerSnapshotDigest.MatchString(imageID) || name != terminalSessionResourceName(sessionID) || operationID == "" || generation <= 0 {
		return nil, errSnapshotRuntimeUnsupported
	}
	args := []string{"create", "--name", name, "--label", pythonExecManagedLabel, "--label", terminalExecCapabilityLabel, "--label", pythonExecRuntimeLabel, "--label", terminalExecSessionLabelKey + "=" + terminalSessionIDHash(sessionID), "--label", terminalExecSchemaLabelKey + "=" + terminalExecSchemaVersion, "--label", "onlyboxes.persistence.operation_id=" + operationID, "--label", "onlyboxes.generation=" + strconv.FormatInt(generation, 10), "--memory", strconv.FormatInt(spec.MemoryMiB, 10) + "m", "--cpus", strconv.FormatFloat(spec.CPUs, 'f', -1, 64), "--pids-limit", strconv.Itoa(int(spec.MaxProcesses))}
	if network != "" {
		args = append(args, "--network", network)
	}
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}
	if spec.WorkingDir != "" {
		args = append(args, "--workdir", spec.WorkingDir)
	}
	keys := make([]string, 0, len(spec.Env))
	for key := range spec.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(spec.Env[key], '\x00') {
			return nil, errSnapshotRuntimeUnsupported
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--env", key+"="+spec.Env[key])
	}
	entrypoint := ""
	if len(spec.Entrypoint) > 0 {
		entrypoint = spec.Entrypoint[0]
	}
	args = append(args, "--entrypoint", entrypoint, imageID)
	if len(spec.Entrypoint) > 1 {
		args = append(args, spec.Entrypoint[1:]...)
	}
	args = append(args, spec.Command...)
	if len(spec.Command)+len(spec.Entrypoint) == 0 {
		return nil, fmt.Errorf("%w: missing startup command", errSnapshotRuntimeUnsupported)
	}
	return args, nil
}

type snapshotContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r snapshotContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// Thaw is an explicit rollback primitive. An export failure never invokes it
// automatically; only the durable operation owner may authorize source rollback.
func (r *dockerSnapshotRuntime) Thaw(ctx context.Context, id string) error {
	if !dockerSnapshotDigest.MatchString("sha256:" + id) {
		return errSnapshotRuntimeUnsupported
	}
	var state struct {
		Status string
		Paused bool
	}
	if err := dockerSnapshotJSON(ctx, &state, "inspect", "--format", "{{json .State}}", id); err != nil {
		return err
	}
	if state.Paused {
		return dockerSnapshotCLI(ctx, nil, io.Discard, "unpause", id)
	}
	switch state.Status {
	case "running":
		return nil
	case "exited", "created":
		return dockerSnapshotCLI(ctx, nil, io.Discard, "start", id)
	default:
		return errSnapshotRuntimeUnsupported
	}
}

// Remove accepts an immutable backend ID, never a reusable session name. A
// successful empty daemon listing confirms an already-completed removal.
func (r *dockerSnapshotRuntime) Remove(ctx context.Context, id string) error {
	if !dockerSnapshotDigest.MatchString("sha256:" + id) {
		return errSnapshotRuntimeUnsupported
	}
	var out bytes.Buffer
	if err := dockerSnapshotCLI(ctx, nil, &out, "ps", "-a", "--no-trunc", "--filter", "id="+id, "--format", "{{.ID}}"); err != nil {
		return err
	}
	found := strings.TrimSpace(out.String())
	if found == "" {
		return nil
	}
	if found != id {
		return errSnapshotRuntimeUnsupported
	}
	return dockerSnapshotCLI(ctx, nil, io.Discard, "rm", "-f", id)
}

func snapshotExportTempPrefix(operation string) string {
	hash := sha256.Sum256([]byte(operation))
	return "snapshot-" + hex.EncodeToString(hash[:]) + "-"
}
func snapshotExportHelperName(source, operation string) string {
	hash := sha256.Sum256([]byte(source + "\x00" + operation))
	return "onlyboxes-snapshot-export-" + hex.EncodeToString(hash[:])
}
func removeSnapshotExportHelper(ctx context.Context, source, operation string) error {
	name := snapshotExportHelperName(source, operation)
	var listed bytes.Buffer
	if err := dockerSnapshotCLI(ctx, nil, &listed, "ps", "-a", "--no-trunc", "--filter", "name=^/"+name+"$", "--format", "{{.ID}}"); err != nil {
		return err
	}
	id := strings.TrimSpace(listed.String())
	if id == "" {
		return nil
	}
	if !dockerSnapshotDigest.MatchString("sha256:" + id) {
		return errSnapshotJournalConflict
	}
	var native dockerSnapshotContainer
	if err := dockerSnapshotJSON(ctx, &native, "inspect", "--format", "{{json .}}", id); err != nil {
		return err
	}
	if native.Config.Labels["onlyboxes.snapshot.export_source"] != source || native.Config.Labels["onlyboxes.snapshot.export_operation"] != operation {
		return errSnapshotJournalConflict
	}
	return dockerSnapshotCLI(ctx, nil, io.Discard, "rm", "-f", id)
}
