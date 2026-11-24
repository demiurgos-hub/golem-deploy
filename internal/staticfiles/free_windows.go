//go:build windows

package staticfiles

import "golang.org/x/sys/windows"

// freeSpace returns the bytes available to the daemon's user on the volume
// holding dir.
func freeSpace(dir string) (int64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return -1, err
	}
	var avail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &totalFree); err != nil {
		return -1, err
	}
	return int64(avail), nil
}
