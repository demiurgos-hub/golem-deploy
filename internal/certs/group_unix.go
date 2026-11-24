//go:build !windows

package certs

import (
	"fmt"
	"os"
)

// groupPolicy returns the group the files get and the supplementary groups
// containers need. The daemon's own group is shared with the container
// user; group 0 is never handed to a container, so a daemon running with
// group 0 gives the files the container's own group instead.
func groupPolicy() (fileGID int, groupAdd []int) {
	gid := os.Getegid()
	if gid == 0 {
		return ContainerGID, nil
	}
	return gid, []int{gid}
}

func chownGroup(path string, gid int) error {
	if gid < 0 {
		return nil
	}
	if err := os.Lchown(path, -1, gid); err != nil {
		return fmt.Errorf("set group %d on %s (the server containers read the certificate through it): %w", gid, path, err)
	}
	return nil
}

// syncDir flushes a directory's entries (the renames) to disk.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
