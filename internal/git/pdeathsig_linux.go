//go:build linux

package git

import "syscall"

// setPdeathsig makes the kernel SIGKILL git when the daemon thread that
// started it exits (the daemon crashing or being killed).
func setPdeathsig(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }
