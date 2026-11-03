//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group, so a Ctrl-C in the
// terminal goes to the CLI alone and the CLI decides when the child stops.
// On Linux the kernel also stops the child if the CLI dies without cleaning
// up (see parentDeathSignal).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	parentDeathSignal(cmd.SysProcAttr)
}

// killProcessGroup stops the child and anything it started.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// forceKillProcessGroup kills the child and anything it started, without giving
// them a chance to clean up. It is the second Ctrl-C's answer, never the first.
func forceKillProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
