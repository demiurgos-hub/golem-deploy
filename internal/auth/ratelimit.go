package auth

import (
	"sync"
	"time"
)

// Limiter counts attempts per key in a fixed window and refuses a key once
// it has used the maximum, until the window that started with its first
// attempt ends.
//
// Attempts are counted up front with Reserve, under one lock, before any
// expensive work, so concurrent requests cannot all pass a check that only
// one of them should have passed. The caller then settles the reservation:
// Refund when the attempt must not count (it succeeded, or never reached the
// password check), Reset to clear the key, or nothing to keep it counted.
//
// Memory is bounded by maxKeys. When full, expired entries are swept; live
// entries are never evicted (so flooding the limiter with new keys cannot
// clear a block), and a new key that does not fit is simply not tracked.
// Callers therefore combine limiters (e.g. per IP and per username) so that
// an untracked key is still covered by another one.
type Limiter struct {
	mu        sync.Mutex
	max       int
	window    time.Duration
	maxKeys   int
	now       func() time.Time
	entries   map[string]*limitEntry
	lastSweep time.Time
}

type limitEntry struct {
	count int
	start time.Time
}

// sweepInterval bounds how often a full limiter scans for expired entries,
// so a flood of new keys cannot make every request pay for a full scan.
const sweepInterval = time.Second

// NewLimiter returns a limiter allowing max attempts per key per window,
// tracking at most maxKeys keys. now may be nil for time.Now.
func NewLimiter(max int, window time.Duration, maxKeys int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{max: max, window: window, maxKeys: maxKeys, now: now, entries: map[string]*limitEntry{}}
}

// Reservation is one counted attempt. Its methods are safe to call on a nil
// or untracked reservation and at most one of Refund and Reset takes effect.
type Reservation struct {
	l       *Limiter
	key     string
	start   time.Time
	settled bool
}

// Reserve atomically checks whether key may make another attempt and, if
// so, counts it. When it may not, ok is false and wait says how long until
// the window ends.
func (l *Limiter) Reserve(key string) (res *Reservation, wait time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[key]
	if e != nil && !now.Before(e.start.Add(l.window)) {
		delete(l.entries, key)
		e = nil
	}
	if e == nil {
		if len(l.entries) >= l.maxKeys {
			l.sweepLocked(now)
		}
		if len(l.entries) >= l.maxKeys {
			return &Reservation{}, 0, true // untracked; other limiters still apply
		}
		e = &limitEntry{start: now}
		l.entries[key] = e
	}
	if e.count >= l.max {
		return nil, e.start.Add(l.window).Sub(now), false
	}
	e.count++
	return &Reservation{l: l, key: key, start: e.start}, 0, true
}

// Refund un-counts the attempt (if its window is still current).
func (r *Reservation) Refund() {
	if r == nil || r.l == nil || r.settled {
		return
	}
	r.settled = true
	l := r.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entries[r.key]; e != nil && e.start.Equal(r.start) && e.count > 0 {
		e.count--
		if e.count == 0 {
			delete(l.entries, r.key)
		}
	}
}

// Reset forgets the key's attempts in the reservation's window.
func (r *Reservation) Reset() {
	if r == nil || r.l == nil || r.settled {
		return
	}
	r.settled = true
	l := r.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entries[r.key]; e != nil && e.start.Equal(r.start) {
		delete(l.entries, r.key)
	}
}

// Reset forgets key.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// Count returns the attempts counted for key in its current window.
func (l *Limiter) Count(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil || !l.now().Before(e.start.Add(l.window)) {
		return 0
	}
	return e.count
}

// Len returns the number of tracked keys.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func (l *Limiter) sweepLocked(now time.Time) {
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < sweepInterval && now.After(l.lastSweep) {
		return
	}
	l.lastSweep = now
	for k, e := range l.entries {
		if !now.Before(e.start.Add(l.window)) {
			delete(l.entries, k)
		}
	}
}
