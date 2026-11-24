package docker

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func waitProcessTree(cmd *exec.Cmd) error {
	pgid := cmd.Process.Pid
	// Leave the leader waitable until the group has stopped, so its PID
	// cannot be reused by an unrelated process group while draining it.
	var info unix.Siginfo
	var err error
	for {
		err = unix.Waitid(unix.P_PID, pgid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err == nil {
		for {
			killErr := syscall.Kill(-pgid, syscall.SIGKILL)
			if killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
				err = killErr
				break
			}
			if !processGroupActive(pgid) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	} else {
		// Do not leave the tree running if the wait syscall is denied.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	return errors.Join(cmd.Wait(), err)
}

func processGroupActive(pgid int) bool {
	if syscall.Kill(-pgid, 0) != nil {
		return false
	}
	// A zombie cannot export anything, but can stay in a process group
	// indefinitely when the daemon itself runs below a non-reaping PID 1.
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	group := strconv.Itoa(pgid)
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		info, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				continue
			}
			return true
		}
		// comm is parenthesized and may itself include spaces and ')'.
		stat := string(info)
		fields := strings.Fields(stat[strings.LastIndexByte(stat, ')')+1:])
		if len(fields) >= 3 && fields[2] == group && fields[0] != "Z" && fields[0] != "X" {
			return true
		}
	}
	return false
}
