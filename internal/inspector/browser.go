package inspector

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed" // the browser scripts
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The daemon's own pages and scripts on the Inspector listener. Browsers
// hold their session key in the sessionStorage of the Inspector's origin
// (scheme, host and port): the listener sets and reads no cookie, because
// game servers are served on other ports of the same host name and cookies
// are not bound to a port.
const (
	// ownPrefix holds the daemon's paths; nothing under it reaches the hub.
	ownPrefix = "/_golem-deploy/"
	// EnterPath is the entry page; the ticket is in the fragment
	// ("#t=<ticket>"), never in a request.
	EnterPath = "/_golem-deploy/enter"
	// SessionPath exchanges a ticket for a session key.
	SessionPath = "/_golem-deploy/session"
	enterJSPath = "/_golem-deploy/enter.js"
	bootJSPath  = "/_golem-deploy/boot.js"
	// EndedPath says that the Inspector session ended.
	EndedPath = "/_golem-deploy/ended"
	// SessionHeader is set to "ended" on a 401 for a request without a
	// valid session; the boot script then removes the key and leaves.
	SessionHeader = "X-Golem-Deploy-Session"
	// WebSocketProtocol is the subprotocol marker the boot script offers
	// with the key subprotocol (WebSocketKeyPrefix + key); a successful
	// upgrade answers with it.
	WebSocketProtocol  = "golem-deploy.inspector"
	WebSocketKeyPrefix = "golem-deploy.key."
	// bootTag is inserted directly after the opening <head> tag of every
	// HTML document from the hub.
	bootTag = `<script src="/_golem-deploy/boot.js"></script>`
	// maxDocument bounds an HTML document read for the insertion.
	maxDocument = 2 << 20
	// maxSessionBody bounds the body of a ticket exchange.
	maxSessionBody = 1 << 10
)

// The scripts, plain ES5 and ASCII only (frontend/src/test tests them in
// jsdom).
var (
	//go:embed boot.js
	bootJS string
	//go:embed enter.js
	enterJS string
)

// errDocument: an HTML document from the hub cannot carry the boot script
// (no <head>, too large, compressed or partial). It is not served.
var errDocument = errors.New("inspector: the hub's page cannot carry the boot script")

const pageStyle = `body{font-family:system-ui,sans-serif;max-width:36rem;margin:4rem auto;padding:0 1rem;line-height:1.5;color:#222;background:#fff}` +
	`@media (prefers-color-scheme:dark){body{color:#ddd;background:#111}}`

// writePage writes one of the daemon's small static pages. With script,
// the page loads that same-origin script and may call its own origin.
func writePage(w http.ResponseWriter, status int, title, text, script string) {
	sum := sha256.Sum256([]byte(pageStyle))
	csp := "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	tag := ""
	if script != "" {
		csp += "; script-src 'self'; connect-src 'self'"
		tag = `<script src="` + script + `"></script>`
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">`+
		`<title>`+html.EscapeString(title)+`</title><style>`+pageStyle+`</style>`+tag+`</head><body><h1 id="title">`+html.EscapeString(title)+
		`</h1><p id="text">`+html.EscapeString(text)+`</p></body></html>`)
}

// page writes a static page without a script.
func (s *Service) page(w http.ResponseWriter, status int, title, text string) {
	writePage(w, status, title, text, "")
}

// serveScript serves one of the embedded scripts.
func serveScript(w http.ResponseWriter, r *http.Request, body string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		hubError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, body)
	}
}

// handleOwn serves the daemon's paths under ownPrefix. None needs a
// session.
func (s *Service) handleOwn(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case SessionPath:
		s.handleSessionEntry(w, r)
		return
	case bootJSPath:
		serveScript(w, r, bootJS)
		return
	case enterJSPath:
		serveScript(w, r, enterJS)
		return
	case EnterPath, EndedPath:
	default:
		hubError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		hubError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.URL.Path == EnterPath {
		writePage(w, http.StatusOK, "Opening the Inspector", "Opening the Inspector. This needs JavaScript.", enterJSPath)
		return
	}
	s.page(w, http.StatusOK, "Inspector session ended",
		"Your Inspector session has ended. Open the Inspector again from Golem Deploy.")
}

// sessionEntry is the answer of a ticket exchange.
type sessionEntry struct {
	Key      string `json:"key"`
	TargetID string `json:"target_id,omitempty"`
}

// handleSessionEntry exchanges a ticket for a session key (POST
// /_golem-deploy/session, JSON {"ticket": "..."}, the Inspector's own
// Origin). It consumes the ticket, checks the account, logs in at the hub
// for the new session and answers the key; only its hash is kept. Failed
// tickets are limited per client address.
func (s *Service) handleSessionEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		hubError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !s.originOK(r) {
		hubError(w, http.StatusForbidden, "origin_not_allowed")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		hubError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSessionBody))
	if err != nil {
		hubError(w, http.StatusRequestEntityTooLarge, "body_too_large")
		return
	}
	var req struct {
		Ticket string `json:"ticket"`
	}
	if json.Unmarshal(raw, &req) != nil {
		hubError(w, http.StatusBadRequest, "bad_request")
		return
	}
	res, wait, ok := s.entryLimit.Reserve(s.cfg.Policy.ClientIP(r))
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		hubError(w, http.StatusTooManyRequests, "too_many_attempts")
		return
	}
	tk, ok := s.tickets.consume(req.Ticket)
	if !ok {
		hubError(w, http.StatusForbidden, "link_invalid")
		return
	}
	acct, err := s.cfg.Store.GetAccount(r.Context(), tk.accountID)
	if err != nil || !acct.Approved || acct.TokenVersion != tk.tokenVersion {
		hubError(w, http.StatusForbidden, "account_invalid")
		return
	}
	res.Refund()
	if _, _, serving := s.agentTarget(); !serving {
		hubError(w, http.StatusServiceUnavailable, "not_running")
		return
	}
	lctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	sid, err := s.hubLogin(lctx)
	cancel()
	if err != nil {
		s.logger.Warn("could not open an Inspector session at the hub", "account_id", acct.ID, "err", err)
		hubError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	key, _, evicted, err := s.sessions.create(acct.ID, acct.TokenVersion, sid)
	if err != nil {
		s.logoutHub(sid)
		hubError(w, http.StatusInternalServerError, "internal")
		return
	}
	for _, old := range evicted {
		s.endSession(old)
	}
	s.logger.Info("inspector session started", "account_id", acct.ID)
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(sessionEntry{Key: key, TargetID: tk.target})
}

// presentedKey returns the session key of a request: the bearer header,
// or for a WebSocket upgrade the key subprotocol offered with the marker.
// offered reports whether the upgrade offered the marker, which the
// answer must then select.
func presentedKey(r *http.Request) (key string, offered bool) {
	wsKey, keys := "", 0
	if isWebSocket(r) {
		for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
			for _, part := range strings.Split(v, ",") {
				part = strings.TrimSpace(part)
				if part == WebSocketProtocol {
					offered = true
				} else if k, ok := strings.CutPrefix(part, WebSocketKeyPrefix); ok {
					wsKey = k
					keys++
				}
			}
		}
	}
	if scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(tok), offered
	}
	if offered && keys == 1 {
		return wsKey, true
	}
	return "", offered
}

// injectBoot inserts the boot script into an HTML document from the hub,
// so it runs before the Inspector's own scripts. A document that cannot
// carry it is refused with errDocument (fail closed).
func injectBoot(resp *http.Response) error {
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt != "text/html" && mt != "application/xhtml+xml" {
		return nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return nil
	}
	if resp.Request != nil && resp.Request.Method == http.MethodHead {
		if resp.ContentLength >= 0 {
			resp.Header.Set("Content-Length", strconv.FormatInt(resp.ContentLength+int64(len(bootTag)), 10))
		}
		return nil
	}
	if resp.StatusCode == http.StatusPartialContent {
		return errDocument
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		return errDocument
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument+1))
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	if len(raw) > maxDocument {
		return errDocument
	}
	at := headEnd(raw)
	if at < 0 {
		return errDocument
	}
	out := make([]byte, 0, len(raw)+len(bootTag))
	out = append(append(append(out, raw[:at]...), bootTag...), raw[at:]...)
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	resp.Header.Del("Content-Encoding")
	return nil
}

// headEnd returns the index just after the first opening <head> tag
// (ASCII case-insensitive, with or without attributes), or -1.
func headEnd(doc []byte) int {
	for i := 0; i+5 < len(doc); i++ {
		if doc[i] != '<' || !asciiLowerIs(doc[i+1:i+5], "head") {
			continue
		}
		switch doc[i+5] {
		case '>', ' ', '\t', '\n', '\r', '\f', '/':
		default:
			continue // <header>, <heading>, ...
		}
		j := bytes.IndexByte(doc[i+5:], '>')
		if j < 0 {
			return -1
		}
		return i + 5 + j + 1
	}
	return -1
}

// asciiLowerIs reports whether b, with ASCII letters lowered, is want.
func asciiLowerIs(b []byte, want string) bool {
	if len(b) != len(want) {
		return false
	}
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != want[i] {
			return false
		}
	}
	return true
}
