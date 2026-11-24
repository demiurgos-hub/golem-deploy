package docker

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func runProcessTree(ctx context.Context, cmd *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(job)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return err
	}
	// The process cannot spawn a plugin between Start and job assignment.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED, HideWindow: true}
	cmd.Cancel = func() error { return windows.TerminateJobObject(job, 1) }
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := uint32(cmd.Process.Pid)
	startErr := assignAndResume(ctx, job, pid)
	if startErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	stopErr := windows.TerminateJobObject(job, 1)
	return errors.Join(startErr, waitErr, stopErr, waitJob(job), ctx.Err())
}

func assignAndResume(ctx context.Context, job windows.Handle, pid uint32) error {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// os/exec closes the primary thread handle after CreateProcess. Look up
	// the suspended thread using the supported Tool Help API.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err := windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID == pid {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return err
			}
			defer windows.CloseHandle(thread)
			_, err = windows.ResumeThread(thread)
			return err
		}
	}
	return errors.New("docker: suspended process has no thread")
}

func waitJob(job windows.Handle) error {
	// Layout of JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
	var info struct {
		Times                                                            [4]int64
		PageFaults, TotalProcesses, ActiveProcesses, TerminatedProcesses uint32
	}
	for {
		if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
			return err
		}
		if info.ActiveProcesses == 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}
