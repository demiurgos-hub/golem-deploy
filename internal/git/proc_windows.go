//go:build windows

package git

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procTree makes cancellation kill git and every process it spawned
// (remote helpers, ssh, git-lfs): git is assigned to a job object right
// after it starts (children inherit the job), cancellation terminates the
// job, and closing the job handle kills stragglers.
//
// Children spawned in the instant between process creation and assignment
// would escape the job; git does not spawn helpers that early.
type procTree struct {
	job windows.Handle
}

func newProcTree(cmd *exec.Cmd) *procTree {
	t := &procTree{}
	if job, err := windows.CreateJobObject(nil, nil); err == nil {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err == nil {
			t.job = job
		} else {
			_ = windows.CloseHandle(job)
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Cancel = func() error {
		if t.job != 0 {
			if err := windows.TerminateJobObject(t.job, 1); err == nil {
				return nil
			}
		}
		return cmd.Process.Kill()
	}
	return t
}

func (t *procTree) started(cmd *exec.Cmd) {
	if t.job == 0 {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.AssignProcessToJobObject(t.job, h)
}

func (t *procTree) close() {
	if t.job != 0 {
		_ = windows.CloseHandle(t.job)
		t.job = 0
	}
}
