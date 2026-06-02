//go:build windows

package runner

import "os/exec"

// configureProcessIsolation is a no-op on Windows. The default cancel behavior
// (TerminateProcess on the direct child) plus Cmd.WaitDelay is sufficient to
// guarantee `Run()` returns within a bounded time even if a child process
// inherits the stdout/stderr pipe and lingers — WaitDelay will close the pipes
// from the Go side and unblock the io.Copy goroutines. Tree-kill semantics
// (Job Objects) can be layered on later if a leaked grandchild becomes
// problematic in practice.
func configureProcessIsolation(_ *exec.Cmd) {}
