//go:build unix

package runner

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureProcessIsolation places the command in its own process group and
// overrides Cmd.Cancel so that ctx cancellation delivers SIGKILL to the entire
// group, not just the direct child. Without this, a shell that fork()s into a
// long-running grandchild (e.g. `sleep 999 & wait` or anything wrapped in
// parentheses) leaves the grandchild alive after the deadline fires; the
// grandchild keeps the stdout/stderr pipe write end open, which blocks
// `exec.Cmd.Run()` and pins the worker's single command slot.
func configureProcessIsolation(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}
