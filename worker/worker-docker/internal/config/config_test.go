package config

import "testing"

func TestLoadUsesDefaultDockerImages(t *testing.T) {
	t.Setenv("WORKER_PYTHON_EXEC_DOCKER_IMAGE", "")
	t.Setenv("WORKER_TERMINAL_EXEC_DOCKER_IMAGE", "")

	cfg := Load()
	if cfg.PythonExecDockerImage != defaultPythonExecImage {
		t.Fatalf("expected default pythonExec image %q, got %q", defaultPythonExecImage, cfg.PythonExecDockerImage)
	}
	if cfg.TerminalExecDockerImage != defaultTerminalExecImage {
		t.Fatalf("expected default terminalExec image %q, got %q", defaultTerminalExecImage, cfg.TerminalExecDockerImage)
	}
}

func TestLoadSupportsCustomDockerImages(t *testing.T) {
	t.Setenv("WORKER_PYTHON_EXEC_DOCKER_IMAGE", "python:3.12-alpine")
	t.Setenv("WORKER_TERMINAL_EXEC_DOCKER_IMAGE", "debian:bookworm-slim")

	cfg := Load()
	if cfg.PythonExecDockerImage != "python:3.12-alpine" {
		t.Fatalf("expected custom pythonExec image, got %q", cfg.PythonExecDockerImage)
	}
	if cfg.TerminalExecDockerImage != "debian:bookworm-slim" {
		t.Fatalf("expected custom terminalExec image, got %q", cfg.TerminalExecDockerImage)
	}
}
