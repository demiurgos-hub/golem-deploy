package inspector

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"slices"
	"sync"
	"time"
)

// Session limits.
const (
	// SessionMaxAge is the lifetime of an Inspector session.
	SessionMaxAge = 8 * time.Hour
	// maxSessionsPerAccount and maxSessions bound live sessions; a new
	// session beyond the account's limit ends that account's oldest.
	maxSessionsPerAccount = 16
	maxSessions           = 1024
	// SweepInterval is how often sessions are checked against their
	// accounts, so open streams of sessions that became invalid close.
	SweepInterval = 30 * time.Second
)

// session is a daemon-held Inspector session of one browser tab, found by
// the SHA-256 of its key (32 random bytes, base64url), which only the tab
// holds. hubSID is the hub session the daemon logged in for it; it never
// reaches the browser.
type session struct {
	key          [32]byte
	accountID    int64
	tokenVersion int64
	created      time.Time
	expires      time.Time

	// loginMu serialises hub logins for the session; mu guards the
	// fields below.
	loginMu   sync.Mutex
	mu        sync.Mutex
	hubSID    string
	lastLogin time.Time
	streams   map[uint64]context.CancelFunc
	nextID    uint64
	ended     bool
}

// hubSession returns the current hub session id.
func (s *session) hubSession() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hubSID
}

// track registers a proxied request so ending the session cancels it
// (closing an open WebSocket stream). The request also ends when the
// session expires: expiry is that instant on the wall clock, computed once
// from now, and is the deadline of ctx. ok is false once the session ended
// or expired; the check and the registration are one step under the
// session's lock.
func (s *session) track(parent context.Context, now time.Time) (ctx context.Context, expiry time.Time, release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended || !now.Before(s.expires) {
		return nil, time.Time{}, nil, false
	}
	expiry = time.Now().Add(s.expires.Sub(now))
	ctx, cancel := context.WithDeadline(parent, expiry)
	s.nextID++
	id := s.nextID
	s.streams[id] = cancel
	return ctx, expiry, func() {
		cancel()
		s.mu.Lock()
		delete(s.streams, id)
		s.mu.Unlock()
	}, true
}

// isEnded reports whether the session ended.
func (s *session) isEnded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

// end marks the session ended, cancels its requests and returns the hub
// session id to log out (empty when it had ended already).
func (s *session) end() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return ""
	}
	s.ended = true
	for _, cancel := range s.streams {
		cancel()
	}
	s.streams = nil
	sid := s.hubSID
	s.hubSID = ""
	return sid
}

// streamCount returns the number of tracked requests (tests).
func (s *session) streamCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

// sessionStore holds the live sessions in memory: a daemon restart ends
// them all.
type sessionStore struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[[32]byte]*session
}

func newSessionStore(now func() time.Time) *sessionStore {
	return &sessionStore{now: now, m: map[[32]byte]*session{}}
}

// Every session leaves the store already ended (end runs under the store's
// lock), so a drain never misses one that is on its way out. The methods
// that remove sessions return their hub session ids for the caller to log
// out.

// create adds a session and returns its key. Sessions it pushed out (the
// account's oldest beyond the limit, the oldest overall beyond the total)
// are ended; their hub session ids are returned.
func (st *sessionStore) create(accountID, tokenVersion int64, hubSID string) (string, *session, []string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, nil, err
	}
	k := base64.RawURLEncoding.EncodeToString(raw[:])
	now := st.now()
	s := &session{key: sha256.Sum256([]byte(k)), accountID: accountID, tokenVersion: tokenVersion, created: now,
		expires: now.Add(SessionMaxAge), hubSID: hubSID, lastLogin: now, streams: map[uint64]context.CancelFunc{}}
	st.mu.Lock()
	defer st.mu.Unlock()
	var evicted []string
	var mine []*session
	for _, o := range st.m {
		if o.accountID == accountID {
			mine = append(mine, o)
		}
	}
	slices.SortFunc(mine, func(a, b *session) int { return a.created.Compare(b.created) })
	for len(mine) >= maxSessionsPerAccount {
		evicted = append(evicted, mine[0].end())
		delete(st.m, mine[0].key)
		mine = mine[1:]
	}
	for len(st.m) >= maxSessions {
		var oldest *session
		for _, o := range st.m {
			if oldest == nil || o.created.Before(oldest.created) {
				oldest = o
			}
		}
		evicted = append(evicted, oldest.end())
		delete(st.m, oldest.key)
	}
	st.m[s.key] = s
	return k, s, evicted, nil
}

// lookup returns the live session of a key (nil when unknown or expired;
// an expired session is ended and removed, and its hub session id is
// returned in expiredSID).
func (st *sessionStore) lookup(k string) (s *session, expiredSID string) {
	if len(k) != 43 {
		return nil, ""
	}
	key := sha256.Sum256([]byte(k))
	st.mu.Lock()
	defer st.mu.Unlock()
	s = st.m[key]
	if s == nil {
		return nil, ""
	}
	if !st.now().Before(s.expires) {
		sid := s.end()
		delete(st.m, key)
		return nil, sid
	}
	return s, ""
}

// remove ends s and drops it from the store; it returns the hub session id
// to log out (empty when s had ended already).
func (st *sessionStore) remove(s *session) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	sid := s.end()
	if st.m[s.key] == s {
		delete(st.m, s.key)
	}
	return sid
}

// all returns every live session.
func (st *sessionStore) all() []*session {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]*session, 0, len(st.m))
	for _, s := range st.m {
		out = append(out, s)
	}
	return out
}

// drain ends and removes every session and returns their hub session ids
// (one per session, empty when it had none).
func (st *sessionStore) drain() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]string, 0, len(st.m))
	for _, s := range st.m {
		out = append(out, s.end())
	}
	st.m = map[[32]byte]*session{}
	return out
}

// count returns the number of live sessions.
func (st *sessionStore) count() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.m)
}
