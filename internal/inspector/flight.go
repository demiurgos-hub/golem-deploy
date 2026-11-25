package inspector

import (
	"context"
	"sync"
	"time"
)

// flight runs one refresh of a cache at a time. Concurrent callers wait for
// the running call instead of starting another, no lock is held while it
// talks to Docker or the hub, and a call that completed less than minAge
// ago is not repeated.
type flight struct {
	mu   sync.Mutex
	done chan struct{}
	at   time.Time
}

// do runs fn unless a call is running (it then waits for that call or for
// ctx) or one completed less than minAge ago. It reports whether it ran fn.
// The completion time is taken from now when fn returns.
func (f *flight) do(ctx context.Context, now func() time.Time, minAge time.Duration, fn func()) bool {
	f.mu.Lock()
	if ch := f.done; ch != nil {
		f.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		return false
	}
	if minAge > 0 && !f.at.IsZero() && now().Sub(f.at) < minAge {
		f.mu.Unlock()
		return false
	}
	ch := make(chan struct{})
	f.done = ch
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.at = now()
		f.done = nil
		f.mu.Unlock()
		close(ch)
	}()
	fn()
	return true
}
