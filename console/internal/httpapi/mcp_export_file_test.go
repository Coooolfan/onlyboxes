package httpapi

import "testing"

func TestBuildExportObjectKey(t *testing.T) {
	originalExportObjectID := newExportObjectID
	newExportObjectID = func() string { return "fixed-id" }
	t.Cleanup(func() {
		newExportObjectID = originalExportObjectID
	})

	got := buildExportObjectKey(" /exports/ ", "session/\x00name", "/tmp/report\\v1.png")
	want := "exports/session__name/fixed-id-report_v1.png"
	if got != want {
		t.Fatalf("expected object key %q, got %q", want, got)
	}
}
