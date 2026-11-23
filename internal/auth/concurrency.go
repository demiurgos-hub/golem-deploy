package auth

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// ErrBusy is returned by Slots.Acquire when no slot frees up in time.
var ErrBusy = errors.New("auth: too many concurrent password operations")

// Slots bounds concurrent expensive work (bcrypt). Acquire waits a short,
// fixed time for a slot and then gives up with ErrBusy, so a flood of
// requests is turned away quickly instead of queueing without bound.
type Slots struct {
	ch       chan struct{}
	wait     time.Duration
	acquired atomic.Uint64
}

// DefaultSlotCount is the number of concurrent bcrypt operations allowed:
// one per CPU, at least two.
func DefaultSlotCount() int { return max(runtime.NumCPU(), 2) }

// NewSlots returns a semaphore of n slots (at least 1) whose Acquire waits
// at most wait.
func NewSlots(n int, wait time.Duration) *Slots {
	return &Slots{ch: make(chan struct{}, max(n, 1)), wait: wait}
}

// Acquire takes a slot. It fails with ErrBusy after the configured wait, or
// with ctx's error if ctx ends first. release must be called exactly once
// (extra calls are ignored).
func (s *Slots) Acquire(ctx context.Context) (release func(), err error) {
	select {
	case s.ch <- struct{}{}:
	default:
		t := time.NewTimer(s.wait)
		defer t.Stop()
		select {
		case s.ch <- struct{}{}:
		case <-t.C:
			return nil, ErrBusy
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.acquired.Add(1)
	var once sync.Once
	return func() { once.Do(func() { <-s.ch }) }, nil
}

// Acquired counts successful Acquire calls (for tests and diagnostics).
func (s *Slots) Acquired() uint64 { return s.acquired.Load() }

// InFlight caps concurrent operations per key (e.g. one login per username
// at a time). Keys are removed when their count drops to zero, so memory is
// bounded by the number of operations in progress.
type InFlight struct {
	mu  sync.Mutex
	max int
	n   map[string]int
}

// NewInFlight returns a cap of max concurrent operations per key.
func NewInFlight(max int) *InFlight {
	return &InFlight{max: max, n: map[string]int{}}
}

// Acquire starts an operation for key, or reports false when key already
// has the maximum in progress. release must be called when the operation
// ends (extra calls are ignored).
func (f *InFlight) Acquire(key string) (release func(), ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n[key] >= f.max {
		return nil, false
	}
	f.n[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.n[key] <= 1 {
				delete(f.n, key)
			} else {
				f.n[key]--
			}
		})
	}, true
}

// Len returns the number of keys with operations in progress.
func (f *InFlight) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.n)
}

// KnownIPs remembers, per username, the client addresses that completed a
// successful login during this process's lifetime. Such an address bypasses
// the per-username login limit, so a legitimate user keeps working from
// their usual address while someone else hammers their username. Memory is
// bounded: at most perUser addresses per username (least recently used
// dropped first) and at most maxUsers usernames (least recently used
// username dropped first). Entries are only added after a correct password,
// so an attacker cannot flood it without credentials.
type KnownIPs struct {
	mu       sync.Mutex
	perUser  int
	maxUsers int
	tick     uint64
	users    map[string]*knownUser
}

type knownUser struct {
	ips  []string // most recently used first
	used uint64
}

// NewKnownIPs returns an empty set with the given bounds.
func NewKnownIPs(perUser, maxUsers int) *KnownIPs {
	return &KnownIPs{perUser: max(perUser, 1), maxUsers: max(maxUsers, 1), users: map[string]*knownUser{}}
}

// Add records a successful login of user from ip.
func (k *KnownIPs) Add(user, ip string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.tick++
	u := k.users[user]
	if u == nil {
		if len(k.users) >= k.maxUsers {
			k.evictLocked()
		}
		u = &knownUser{}
		k.users[user] = u
	}
	u.used = k.tick
	for i, v := range u.ips {
		if v == ip {
			copy(u.ips[1:i+1], u.ips[:i])
			u.ips[0] = ip
			return
		}
	}
	if len(u.ips) < k.perUser {
		u.ips = append(u.ips, "")
	}
	copy(u.ips[1:], u.ips[:len(u.ips)-1])
	u.ips[0] = ip
}

// Contains reports whether ip has logged in successfully as user.
func (k *KnownIPs) Contains(user, ip string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	u := k.users[user]
	if u == nil {
		return false
	}
	for _, v := range u.ips {
		if v == ip {
			return true
		}
	}
	return false
}

func (k *KnownIPs) evictLocked() {
	var oldest string
	var oldestUsed uint64
	first := true
	for name, u := range k.users {
		if first || u.used < oldestUsed {
			oldest, oldestUsed, first = name, u.used, false
		}
	}
	delete(k.users, oldest)
}

// Count reports how many operations are in progress for key.
func (f *InFlight) Count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n[key]
}
