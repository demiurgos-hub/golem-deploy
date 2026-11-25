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
// (closing an open WebSocket stream). ok is false once the session ended.
func (s *session) track(parent context.Context) (ctx context.Context, release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	s.nextID++
	id := s.nextID
	s.streams[id] = cancel
	return ctx, func() {
		cancel()
		s.mu.Lock()
		delete(s.streams, id)
		s.mu.Unlock()
	}, true
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

// create adds a session and returns its key. Sessions it pushed
// out (the account's oldest beyond the limit, the oldest overall beyond
// the total) are returned so the caller ends them.
func (st *sessionStore) create(accountID, tokenVersion int64, hubSID string) (string, *session, []*session, error) {
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
	var evicted []*session
	var mine []*session
	for _, o := range st.m {
		if o.accountID == accountID {
			mine = append(mine, o)
		}
	}
	slices.SortFunc(mine, func(a, b *session) int { return a.created.Compare(b.created) })
	for len(mine) >= maxSessionsPerAccount {
		delete(st.m, mine[0].key)
		evicted = append(evicted, mine[0])
		mine = mine[1:]
	}
	for len(st.m) >= maxSessions {
		var oldest *session
		for _, o := range st.m {
			if oldest == nil || o.created.Before(oldest.created) {
				oldest = o
			}
		}
		delete(st.m, oldest.key)
		evicted = append(evicted, oldest)
	}
	st.m[s.key] = s
	return k, s, evicted, nil
}

// lookup returns the live session of a key (nil when unknown or expired;
// an expired session is removed and returned in expired so the caller
// ends it).
func (st *sessionStore) lookup(k string) (s *session, expired *session) {
	if len(k) != 43 {
		return nil, nil
	}
	key := sha256.Sum256([]byte(k))
	st.mu.Lock()
	defer st.mu.Unlock()
	s = st.m[key]
	if s == nil {
		return nil, nil
	}
	if !st.now().Before(s.expires) {
		delete(st.m, key)
		return nil, s
	}
	return s, nil
}

// remove drops s from the store (it reports whether s was present).
func (st *sessionStore) remove(s *session) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.m[s.key] != s {
		return false
	}
	delete(st.m, s.key)
	return true
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

// drain removes and returns every session.
func (st *sessionStore) drain() []*session {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]*session, 0, len(st.m))
	for _, s := range st.m {
		out = append(out, s)
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
