//go:build !linux

package proc

import "os/exec"

// Group is a no-op off Linux; exo only runs on Linux.
func Group(cmd *exec.Cmd) {}
