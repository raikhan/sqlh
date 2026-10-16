//go:build !windows

package cmd

import (
	"os/exec"
	"syscall"
)

// setDetached puts cmd in its own session: no controlling terminal, so no
// SIGHUP when the editor's terminal goes away.
func setDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// processAlive reports whether a process exists.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
