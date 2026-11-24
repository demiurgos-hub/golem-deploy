//go:build unix

package backups

import "syscall"

// noFollow makes opening an archive fail when its name is a symbolic link.
const noFollow = syscall.O_NOFOLLOW
