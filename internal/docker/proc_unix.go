//go:build !windows

package docker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func runProcessTree(ctx context.Context, cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return errors.Join(waitProcessTree(cmd), ctx.Err())
}
