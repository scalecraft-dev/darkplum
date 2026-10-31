//go:build windows

package runner

import "os/exec"

func setProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func forceKillProcessGroup(cmd *exec.Cmd) error { return killProcessGroup(cmd) }
