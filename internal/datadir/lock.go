package datadir

import (
	"errors"
	"fmt"
	"os"
)

// ErrLocked means another process holds the data directory lock.
var ErrLocked = errors.New("the data directory is in use by another golem-deploy serve process")

// Lock is an exclusive lock on a data directory, held by serve for as long
// as it runs. The operating system releases it when the process exits,
// however it exits.
type Lock struct {
	f *os.File
}

// AcquireLock takes the exclusive, non-blocking lock on l.LockFile(). It
// fails with an error wrapping ErrLocked, naming the data directory, when
// another process (or another Lock in this process) holds it.
func AcquireLock(l Layout) (*Lock, error) {
	f, err := os.OpenFile(l.LockFile(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	locked, err := lockFile(f)
	if err != nil {
		f.Close()
		if locked {
			return nil, fmt.Errorf("%w: %s (stop the other process or use a different --data-dir)", ErrLocked, l.Root)
		}
		return nil, fmt.Errorf("lock %s: %w", l.LockFile(), err)
	}
	return &Lock{f: f}, nil
}

// Release releases the lock.
func (k *Lock) Release() error {
	if k == nil || k.f == nil {
		return nil
	}
	err := unlockFile(k.f)
	if cerr := k.f.Close(); err == nil {
		err = cerr
	}
	k.f = nil
	return err
}
