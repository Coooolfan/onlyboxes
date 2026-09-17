package runner

import (
	"runtime"
	"testing"

	"github.com/onlyboxes/onlyboxes/worker/worker-sys/internal/config"
)

func TestBuildHelloAddsHostPlatformLabels(t *testing.T) {
	cfg := config.Config{
		WorkerID:     "worker-12345678",
		WorkerSecret: "secret",
		Labels: map[string]string{
			"region":  "cn",
			goosLabel: "configured-os",
			archLabel: "configured-arch",
		},
	}

	hello, err := buildHello(cfg)
	if err != nil {
		t.Fatalf("build hello: %v", err)
	}

	labels := hello.GetLabels()
	if got := labels[goosLabel]; got != runtime.GOOS {
		t.Fatalf("expected %s label %q, got %q", goosLabel, runtime.GOOS, got)
	}
	if got := labels[archLabel]; got != runtime.GOARCH {
		t.Fatalf("expected %s label %q, got %q", archLabel, runtime.GOARCH, got)
	}
	if got := labels["region"]; got != "cn" {
		t.Fatalf("expected custom label to be preserved, got %q", got)
	}
	if cfg.Labels[goosLabel] != "configured-os" || cfg.Labels[archLabel] != "configured-arch" {
		t.Fatalf("build hello mutated configured labels: %#v", cfg.Labels)
	}
}
