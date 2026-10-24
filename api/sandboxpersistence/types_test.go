package sandboxpersistence

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestBoxLiteDiskSizeIsOptionalBoundedAndRuntimeSpecific(t *testing.T) {
	spec := RestoreSpec{Runtime: "boxlite", RuntimeVersion: "0.10.7", Platform: "linux/arm64", Format: BoxliteArchiveFormat, Image: "runtime@sha256:" + strings.Repeat("a", 64), MemoryMiB: 256, CPUs: 1, MaxProcesses: 128}
	encoded, err := json.Marshal(spec)
	if err != nil || strings.Contains(string(encoded), "disk_size_gb") {
		t.Fatal("unspecified disk size changed its wire representation", err)
	}
	for _, size := range []int64{-1, 0, 1, 4, 10240, 10241} {
		spec.DiskSizeGB = &size
		valid := size >= 1 && size <= 10240
		if (spec.Validate() == nil) != valid {
			t.Fatalf("unexpected validation for disk size %d", size)
		}
		if valid {
			data, err := json.Marshal(spec)
			var restored RestoreSpec
			if err != nil || json.Unmarshal(data, &restored) != nil || restored.DiskSizeGB == nil || *restored.DiskSizeGB != size {
				t.Fatal("disk capacity lost during wire round trip")
			}
		}
	}
	size := int64(4)
	spec.DiskSizeGB, spec.Runtime, spec.Format = &size, "docker", DockerRootFSFormat
	if spec.Validate() == nil {
		t.Fatal("BoxLite disk setting accepted for Docker")
	}
}

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
