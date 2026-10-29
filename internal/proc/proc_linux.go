// Package proc runs child processes in their own process group so a timeout
// or cancel kills everything they spawned, not just the top process.
package proc

import (
	"os/exec"
	"syscall"
)

func Group(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
