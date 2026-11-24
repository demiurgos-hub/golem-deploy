//go:build !(linux || darwin || freebsd || dragonfly || windows)

package staticfiles

// freeSpace is unknown on this platform; the quota still bounds the store.
func freeSpace(string) (int64, error) { return -1, nil }
