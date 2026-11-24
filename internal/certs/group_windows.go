//go:build windows

package certs

// groupPolicy: Windows development hosts run Linux containers through
// Docker Desktop, whose bind mounts do not carry Unix ownership, so no
// group handling is done.
func groupPolicy() (fileGID int, groupAdd []int) { return -1, nil }

func chownGroup(string, int) error { return nil }

func syncDir(string) error { return nil }
