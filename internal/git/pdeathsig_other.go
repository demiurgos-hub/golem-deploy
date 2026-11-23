//go:build !linux && !windows

package git

import "syscall"

func setPdeathsig(*syscall.SysProcAttr) {}
