//go:build !unix

package backups

// noFollow is not available here; open compares the opened file with the
// link-free Lstat of its name instead.
const noFollow = 0
