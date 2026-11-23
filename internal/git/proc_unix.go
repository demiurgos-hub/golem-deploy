//go:build !windows

package git

import (
	"os/exec"
	"syscall"
)

// procTree makes git and every process it spawned (remote helpers, ssh,
// git-lfs) die together: git runs in its own process group, which receives
// SIGKILL on cancellation and again after git exited (so no helper that
// outlived git keeps writing into the checkout or tmp/). On Linux git also
// gets SIGKILL when the daemon dies (Pdeathsig); its helpers then fail on
// their broken pipes.
type procTree struct {
	pid int
}

func newProcTree(cmd *exec.Cmd) *procTree {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setPdeathsig(cmd.SysProcAttr)
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return &procTree{}
}

func (t *procTree) started(cmd *exec.Cmd) { t.pid = cmd.Process.Pid }

// close kills what is left of the process group after Wait returned. The
// group id cannot have been reused while any member is alive.
func (t *procTree) close() {
	if t.pid > 0 {
		_ = syscall.Kill(-t.pid, syscall.SIGKILL)
		t.pid = 0
	}
}
