package inspector

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// Ticket limits.
const (
	// TicketTTL is how long an entry link works.
	TicketTTL = 60 * time.Second
	// maxTicketsPerAccount and maxTickets bound unconsumed tickets.
	maxTicketsPerAccount = 8
	maxTickets           = 1024
)

// errTooManyTickets: the account (or the installation) holds the maximum
// of unconsumed tickets.
var errTooManyTickets = errors.New("inspector: too many open entry links; wait a minute")

// ticket is a one-time entry into the Inspector for an account. Only its
// SHA-256 is kept.
type ticket struct {
	accountID    int64
	tokenVersion int64
	target       string
	expires      time.Time
}

// ticketStore holds unconsumed tickets in memory (a daemon restart drops
// them).
type ticketStore struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[[32]byte]ticket
}

func newTicketStore(now func() time.Time) *ticketStore {
	return &ticketStore{now: now, m: map[[32]byte]ticket{}}
}

func (t *ticketStore) sweepLocked(now time.Time) {
	for k, v := range t.m {
		if !now.Before(v.expires) {
			delete(t.m, k)
		}
	}
}

// issue returns a new ticket (43 URL-safe characters) and its expiry.
func (t *ticketStore) issue(accountID, tokenVersion int64, target string) (string, time.Time, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweepLocked(now)
	n := 0
	for _, v := range t.m {
		if v.accountID == accountID {
			n++
		}
	}
	if n >= maxTicketsPerAccount || len(t.m) >= maxTickets {
		return "", time.Time{}, errTooManyTickets
	}
	tok := base64.RawURLEncoding.EncodeToString(raw[:])
	exp := now.Add(TicketTTL)
	t.m[sha256.Sum256([]byte(tok))] = ticket{accountID: accountID, tokenVersion: tokenVersion, target: target, expires: exp}
	return tok, exp, nil
}

// consume returns the ticket and removes it; ok is false for an unknown,
// used or expired ticket.
func (t *ticketStore) consume(tok string) (ticket, bool) {
	if len(tok) != 43 {
		return ticket{}, false
	}
	key := sha256.Sum256([]byte(tok))
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.m[key]
	if !ok {
		return ticket{}, false
	}
	delete(t.m, key)
	if !t.now().Before(v.expires) {
		return ticket{}, false
	}
	return v, true
}

// clear drops every ticket.
func (t *ticketStore) clear() {
	t.mu.Lock()
	t.m = map[[32]byte]ticket{}
	t.mu.Unlock()
}
