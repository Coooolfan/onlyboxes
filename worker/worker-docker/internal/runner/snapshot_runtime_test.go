package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

func snapshotTestTar(t *testing.T, headers ...*tar.Header) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "archive-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	writer := tar.NewWriter(file)
	for _, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := writer.Write(bytes.Repeat([]byte("x"), int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestDockerSnapshotTarValidationAndExpandedBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header *tar.Header
		limit  int64
		want   error
	}{
		{"absolute path", &tar.Header{Name: "/etc/escape", Typeflag: tar.TypeReg}, 1 << 20, errSnapshotArchiveIntegrity},
		{"parent path", &tar.Header{Name: "../escape", Typeflag: tar.TypeReg}, 1 << 20, errSnapshotArchiveIntegrity},
		{"escaping hardlink", &tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "../../host"}, 1 << 20, errSnapshotArchiveIntegrity},
		{"device", &tar.Header{Name: "device", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}, 1 << 20, errSnapshotRuntimeUnsupported},
		{"materialization limit", &tar.Header{Name: "data", Typeflag: tar.TypeReg, Size: 8193}, 12288, errSnapshotArchiveQuota},
		{"fifo", &tar.Header{Name: "fifo", Typeflag: tar.TypeFifo, Mode: 0640}, 4096, nil},
		{"symlink", &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/usr/lib"}, 4096, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateDockerSnapshotTar(context.Background(), snapshotTestTar(t, tc.header), tc.limit)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	file := snapshotTestTar(t, &tar.Header{Name: "data", Typeflag: tar.TypeReg, Size: 4097}, &tar.Header{Name: "same", Typeflag: tar.TypeLink, Linkname: "data"})
	// 8192 rounded data bytes plus one metadata block per entry; hardlinks do
	// not charge a second full file. Tiny-file overhead still consumes budget.
	if expanded, err := validateDockerSnapshotTar(context.Background(), file, 16384); err != nil || expanded != 16384 {
		t.Fatalf("expanded = %d, %v", expanded, err)
	}
	if _, err := validateDockerSnapshotTar(context.Background(), snapshotTestTar(t), 1<<20); !errors.Is(err, errSnapshotArchiveIntegrity) {
		t.Fatalf("accepted empty archive: %v", err)
	}
}

func TestDockerSnapshotCorruptionAndCanceledVerificationNeverInvokeImport(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "invoked")
	if err := os.WriteFile(filepath.Join(directory, "docker"), []byte("#!/bin/sh\n/usr/bin/touch '"+marker+"'\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	file := snapshotTestTar(t, &tar.Header{Name: "data", Typeflag: tar.TypeReg, Size: 8})
	stat, _ := file.Stat()
	spec := wire.RestoreSpec{Runtime: "docker", RuntimeVersion: "29.4.0", Platform: "linux/arm64", Format: wire.DockerRootFSFormat, Image: "runtime@sha256:" + strings.Repeat("a", 64), MemoryMiB: 256, CPUs: .5, MaxProcesses: 128}
	runtime := &dockerSnapshotRuntime{version: spec.RuntimeVersion, platform: spec.Platform}
	archive := wire.Archive{SnapshotID: "snapshot", Size: stat.Size(), SHA256: strings.Repeat("0", 64), ExpandedBytes: 8192, Format: spec.Format}
	if _, err := runtime.Import(context.Background(), file, archive, spec, "onlyboxes-snapshot-test:v1"); !errors.Is(err, errSnapshotArchiveIntegrity) {
		t.Fatalf("corruption = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.Import(ctx, file, archive, spec, "onlyboxes-snapshot-test:v1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled verification = %v", err)
	}
	content, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(content)
	archive.SHA256 = hex.EncodeToString(hash[:])
	archive.ExpandedBytes--
	if _, err := runtime.Import(context.Background(), file, archive, spec, "onlyboxes-snapshot-test:v1"); !errors.Is(err, errSnapshotArchiveQuota) {
		t.Fatalf("understated import budget = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("invalid archive reached Docker import")
	}
}

func TestDockerSnapshotCLIErrorDoesNotExposePrivateOutput(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "docker"), []byte("#!/bin/sh\necho 'private-environment-secret' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	err := dockerSnapshotCLI(context.Background(), nil, nil, "create", "--env", "private-environment-secret")
	if err == nil || strings.Contains(err.Error(), "private-environment-secret") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestDockerSnapshotRestoreStartupArgumentsPreserveEntrypointAndCommand(t *testing.T) {
	spec := wire.RestoreSpec{Runtime: "docker", RuntimeVersion: "29.4.0", Platform: "linux/arm64", Format: wire.DockerRootFSFormat, Image: "runtime@sha256:" + strings.Repeat("a", 64), MemoryMiB: 512, CPUs: .25, MaxProcesses: 64, User: "1234:2345", WorkingDir: "/workspace", Env: map[string]string{"A": "one two", "B": "$(keep-literal)"}, Entrypoint: []string{"/bin/sh", "-c", "exec \"$@\"", "entrypoint"}, Command: []string{"python3", "-c", "print('hello')"}}
	image := "sha256:" + strings.Repeat("b", 64)
	args, err := dockerSnapshotCreateArgs(terminalSessionResourceName("session"), "session", image, "operation", 2, spec, "sandbox-network")
	if err != nil {
		t.Fatal(err)
	}
	var suffix []string
	for i, arg := range args {
		if arg == image {
			suffix = args[i+1:]
			break
		}
	}
	want := append(append([]string{}, spec.Entrypoint[1:]...), spec.Command...)
	if strings.Join(suffix, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("startup changed: %q != %q", suffix, want)
	}
	joined := strings.Join(args, "\x00")
	for _, value := range []string{"--user\x001234:2345", "--workdir\x00/workspace", "--cpus\x000.25", "--pids-limit\x0064", "--env\x00B=$(keep-literal)", "--network\x00sandbox-network", "onlyboxes.generation=2"} {
		if !strings.Contains(joined, value) {
			t.Fatalf("missing configuration %q", value)
		}
	}
}
