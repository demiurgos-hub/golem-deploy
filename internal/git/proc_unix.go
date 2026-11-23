//go:build !windows

package git

import (
	"os/exec"
	"syscall"
)

// procTree makes cancellation kill git and every process it spawned
// (remote helpers, ssh, git-lfs): git runs in its own process group and
// the whole group receives SIGKILL.
type procTree struct{}

func newProcTree(cmd *exec.Cmd) *procTree {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return &procTree{}
}

func (*procTree) started(*exec.Cmd) {}

func (*procTree) close() {}
