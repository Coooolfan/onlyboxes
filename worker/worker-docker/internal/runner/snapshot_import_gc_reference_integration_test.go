package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestIntegrationSnapshotImportGCReferenceSurvivesCrashAndPreservesConcurrentForeignTag(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("requires DOCKER_INTEGRATION=1 and DOCKER_SNAPSHOT_HELPER_IMAGE")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	backend, err := newDockerSnapshotRuntime(ctx, os.Getenv("DOCKER_SNAPSHOT_HELPER_IMAGE"))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := testSnapshotController(t)
	op := testSnapshotOperation()
	op.ID = uuid.NewString()
	op.Kind, op.Phase = "target", "prepared"
	op.Spec.RuntimeVersion, op.Spec.Platform = backend.version, backend.platform
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	if writer.WriteHeader(&tar.Header{Name: "data", Mode: 0600, Size: 1}) != nil {
		t.Fatal("archive header")
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archive := transferTestArchive(data.Bytes())
	op.Archive = &archive
	file, err := os.CreateTemp(t.TempDir(), "archive")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(data.Bytes()); err != nil {
		t.Fatal(err)
	}
	tag := c.imageTag(op)
	op.ImageID, err = backend.Import(ctx, file, archive, op.Spec, tag)
	if err != nil {
		t.Fatal(err)
	}
	gcTag := strings.TrimSuffix(tag, ":rootfs") + ":gc-" + strings.TrimPrefix(op.ImageID, "sha256:")
	foreignTag := "onlyboxes-import-gc-foreign-" + uuid.NewString() + ":retained"
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		for _, ref := range []string{tag, gcTag, foreignTag, op.ImageID} {
			_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "image", "rm", ref)
		}
	}()
	// Recreate the crash window after the collector has claimed a dangling
	// image, but before deleting its own temporary reference.
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "image", "tag", op.ImageID, gcTag); err != nil {
		t.Fatal(err)
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "image", "rm", tag); err != nil {
		t.Fatal(err)
	}
	realDocker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = image ] && [ "$2" = rm ] && [ "$3" = "$ONLYBOXES_GC_REFERENCE" ]; then
  "$ONLYBOXES_GC_REAL_DOCKER" image tag "$ONLYBOXES_GC_IMAGE" "$ONLYBOXES_GC_FOREIGN_TAG" || exit 1
fi
exec "$ONLYBOXES_GC_REAL_DOCKER" "$@"
`
	if err := os.WriteFile(filepath.Join(root, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ONLYBOXES_GC_REAL_DOCKER", realDocker)
	t.Setenv("ONLYBOXES_GC_REFERENCE", gcTag)
	t.Setenv("ONLYBOXES_GC_IMAGE", op.ImageID)
	t.Setenv("ONLYBOXES_GC_FOREIGN_TAG", foreignTag)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := c.removeImportTag(ctx, op); err != nil {
		t.Fatal(err)
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "image", "inspect", gcTag); err == nil {
		t.Fatal("interrupted GC reference was not collected")
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "image", "inspect", foreignTag); err != nil {
		t.Fatal("concurrently added foreign reference was removed")
	}
	if err := c.removeImportTag(ctx, op); err != nil {
		t.Fatal(err)
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "image", "inspect", foreignTag); err != nil {
		t.Fatal("foreign image reference was removed on replay")
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "image", "tag", op.ImageID, gcTag); err != nil {
		t.Fatal(err)
	}
	if err := c.removeImportTag(ctx, op); err != nil {
		t.Fatal(err)
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "image", "inspect", gcTag); err == nil {
		t.Fatal("GC reference survived alongside an existing foreign tag")
	}
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "image", "inspect", foreignTag); err != nil {
		t.Fatal("existing foreign tag was removed during GC-reference recovery")
	}
}
