//go:build !linux && !windows

package commands

import "syscall"

// parentDeathSignal: macOS and the BSDs have no Pdeathsig; the CLI's own
// signal handling (SIGINT, SIGTERM, SIGHUP) closes the tunnel instead.
func parentDeathSignal(*syscall.SysProcAttr) {}
