//go:build linux || darwin || freebsd || dragonfly

package staticfiles

import "golang.org/x/sys/unix"

// freeSpace returns the bytes available to unprivileged users on the file
// system holding dir.
func freeSpace(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return -1, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
