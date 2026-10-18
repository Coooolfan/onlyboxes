package sandboxpersistence

import (
	"math"
	"strings"
	"testing"
)

func TestRestoreSpecRetainsFractionalCPUAndProcessLimit(t *testing.T) {
	spec := RestoreSpec{Runtime: "docker", RuntimeVersion: "29.4.0", Platform: "linux/arm64", Format: DockerRootFSFormat, Image: "runtime@sha256:" + strings.Repeat("a", 64), MemoryMiB: 256, CPUs: .25, MaxProcesses: 128}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*RestoreSpec)
	}{
		{"unlimited processes", func(s *RestoreSpec) { s.MaxProcesses = 0 }},
		{"mutable image", func(s *RestoreSpec) { s.Image = "runtime:latest" }},
		{"wrong format", func(s *RestoreSpec) { s.Format = BoxliteArchiveFormat }},
		{"nonfinite CPU", func(s *RestoreSpec) { s.CPUs = math.Inf(1) }},
		{"oversized metadata", func(s *RestoreSpec) { s.Env = map[string]string{"SECRET": strings.Repeat("x", 32<<10)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := spec
			tc.edit(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatal("accepted unsafe restore specification")
			}
		})
	}
}

func TestArchiveRejectsUnsafeIdentityAndUnboundedImport(t *testing.T) {
	archive := Archive{SnapshotID: "snapshot-1", Size: 8, SHA256: strings.Repeat("a", 64), ExpandedBytes: 128, Format: BoxliteArchiveFormat}
	if err := archive.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../snapshot", "/snapshot", "snapshot/name", ""} {
		invalid := archive
		invalid.SnapshotID = id
		if err := invalid.Validate(); err == nil {
			t.Fatalf("accepted unsafe archive ID %q", id)
		}
	}
	archive.ExpandedBytes = 0
	if err := archive.Validate(); err == nil {
		t.Fatal("accepted archive without expanded storage budget")
	}
}
