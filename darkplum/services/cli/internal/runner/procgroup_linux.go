//go:build linux

package runner

import "syscall"

// parentDeathSignal has the kernel send SIGTERM to the child when the CLI
// exits for any reason, including SIGKILL, so no tunnel outlives it.
func parentDeathSignal(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGTERM }
