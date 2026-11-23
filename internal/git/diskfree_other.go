//go:build !linux && !darwin && !freebsd && !windows

package git

import "errors"

// freeSpace is not implemented here; the free-space check is skipped.
func freeSpace(string) (uint64, error) { return 0, errors.New("not supported") }
