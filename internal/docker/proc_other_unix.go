//go:build !linux && !windows

package docker

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func waitProcessTree(cmd *exec.Cmd) error {
	err := cmd.Wait()
	pgid := -cmd.Process.Pid
	if killErr := syscall.Kill(pgid, syscall.SIGKILL); killErr == nil {
		for syscall.Kill(pgid, 0) == nil {
			time.Sleep(10 * time.Millisecond)
		}
	} else if !errors.Is(killErr, syscall.ESRCH) {
		return errors.Join(err, killErr)
	}
	return err
}
