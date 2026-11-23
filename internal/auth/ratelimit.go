package auth

import (
	"sync"
	"time"
)

// Limiter counts events per key in a fixed window and blocks a key once it
// reaches the maximum, until the window that started with its first event
// ends. Memory is bounded: when maxKeys is reached, expired entries are
// swept and, if still full, the entry with the oldest window is evicted.
type Limiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	maxKeys int
	now     func() time.Time
	entries map[string]*limitEntry
}

type limitEntry struct {
	count int
	start time.Time
}

// NewLimiter returns a limiter allowing max events per key per window,
// tracking at most maxKeys keys. now may be nil for time.Now.
func NewLimiter(max int, window time.Duration, maxKeys int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{max: max, window: window, maxKeys: maxKeys, now: now, entries: map[string]*limitEntry{}}
}

// Check reports whether key may proceed and, if not, how long until it may.
func (l *Limiter) Check(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return true, 0
	}
	now := l.now()
	end := e.start.Add(l.window)
	if !now.Before(end) {
		delete(l.entries, key)
		return true, 0
	}
	if e.count >= l.max {
		return false, end.Sub(now)
	}
	return true, 0
}

// Add records one event for key.
func (l *Limiter) Add(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if e, ok := l.entries[key]; ok {
		if now.Before(e.start.Add(l.window)) {
			e.count++
			return
		}
		e.count, e.start = 1, now
		return
	}
	if len(l.entries) >= l.maxKeys {
		l.evictLocked(now)
	}
	l.entries[key] = &limitEntry{count: 1, start: now}
}

// Reset forgets key.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// Len returns the number of tracked keys.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func (l *Limiter) evictLocked(now time.Time) {
	var oldestKey string
	var oldest time.Time
	for k, e := range l.entries {
		if !now.Before(e.start.Add(l.window)) {
			delete(l.entries, k)
			continue
		}
		if oldestKey == "" || e.start.Before(oldest) {
			oldestKey, oldest = k, e.start
		}
	}
	if len(l.entries) >= l.maxKeys && oldestKey != "" {
		delete(l.entries, oldestKey)
	}
}
