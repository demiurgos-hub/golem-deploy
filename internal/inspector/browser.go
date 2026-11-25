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
	// HTML document from the hub (see headEnd).
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
// (no leading <head> tag, too large, compressed or partial). It is not
// served.
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
		reject(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
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
		reject(w, r, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		reject(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
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
// Origin). Failed tickets are limited per client address; the limiter is
// asked before the body is read. It consumes the ticket, checks the
// account, logs in at the hub for the new session and answers the key;
// only its hash is kept. The ticket is accepted only under the generation
// (admission and secrets) it was issued under: when it is consumed, when
// the hub snapshot is taken and when the session is registered.
func (s *Service) handleSessionEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		reject(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !s.originOK(r) {
		reject(w, r, http.StatusForbidden, "origin_not_allowed")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		reject(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	res, wait, ok := s.entryLimit.Reserve(s.cfg.Policy.ClientIP(r))
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		reject(w, r, http.StatusTooManyRequests, "too_many_attempts")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSessionBody))
	if err != nil {
		reject(w, r, http.StatusRequestEntityTooLarge, "body_too_large")
		return
	}
	var req struct {
		Ticket string `json:"ticket"`
	}
	if json.Unmarshal(raw, &req) != nil {
		hubError(w, http.StatusBadRequest, "bad_request")
		return
	}
	cur := s.hubNow().gen
	tk, ok := s.tickets.consume(req.Ticket)
	if !ok || tk.gen != cur {
		hubError(w, http.StatusForbidden, "link_invalid")
		return
	}
	acct, err := s.cfg.Store.GetAccount(r.Context(), tk.accountID)
	if err != nil || !acct.Approved || acct.TokenVersion != tk.tokenVersion {
		hubError(w, http.StatusForbidden, "account_invalid")
		return
	}
	res.Refund()
	if s.hooks.afterTicketConsumed != nil {
		s.hooks.afterTicketConsumed()
	}
	v, serving := s.view()
	if v.gen != tk.gen {
		// The secrets were replaced or the Inspector turned off since the
		// link was issued.
		hubError(w, http.StatusForbidden, "link_invalid")
		return
	}
	if !serving {
		hubError(w, http.StatusServiceUnavailable, "not_running")
		return
	}
	lctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	sid, err := s.hubLogin(lctx, v)
	cancel()
	if err != nil {
		s.logger.Warn("could not open an Inspector session at the hub", "account_id", acct.ID, "err", err)
		hubError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if s.hooks.afterEntryLogin != nil {
		s.hooks.afterEntryLogin()
	}
	var key string
	var evicted []*session
	var createErr error
	if !s.admit(tk.gen, func() bool {
		key, _, evicted, createErr = s.sessions.create(acct.ID, acct.TokenVersion, sid)
		return createErr == nil
	}) {
		s.logoutHub(sid)
		if createErr != nil {
			hubError(w, http.StatusInternalServerError, "internal")
			return
		}
		hubError(w, http.StatusServiceUnavailable, "not_running")
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

// headEnd returns the index just after the opening <head> tag of a
// document whose leading content is exactly, in this order: an optional
// UTF-8 byte order mark, optional whitespace, at most one doctype, optional
// whitespace, an optional <html ...> tag, optional whitespace and then the
// <head ...> tag. Comments are not accepted anywhere before it: browsers
// end some malformed comments earlier than "-->", so markup they execute
// could come before the boot script. Tag names are ASCII case-insensitive,
// and tags end where a browser ends them (tagEnd). Any other document
// returns -1.
func headEnd(doc []byte) int {
	i := 0
	if bytes.HasPrefix(doc, []byte("\xef\xbb\xbf")) {
		i = 3 // UTF-8 byte order mark
	}
	space := func() {
		for i < len(doc) && isHTMLSpace(doc[i]) {
			i++
		}
	}
	space()
	if i+9 <= len(doc) && doc[i] == '<' && doc[i+1] == '!' && asciiLowerIs(doc[i+2:i+9], "doctype") {
		// A doctype ends at its first ">", also inside quotes.
		end := bytes.IndexByte(doc[i:], '>')
		if end < 0 {
			return -1
		}
		i += end + 1
		space()
	}
	if isTag(doc[i:], "html") {
		end := tagEnd(doc[i:], len("html"))
		if end < 0 {
			return -1
		}
		i += end
		space()
	}
	if !isTag(doc[i:], "head") {
		return -1
	}
	end := tagEnd(doc[i:], len("head"))
	if end < 0 {
		return -1
	}
	return i + end
}

func isHTMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

// isTag reports whether b starts with the opening tag name (a "<", the
// name, then whitespace, "/" or ">").
func isTag(b []byte, name string) bool {
	n := len(name)
	if len(b) < n+2 || b[0] != '<' || !asciiLowerIs(b[1:1+n], name) {
		return false
	}
	c := b[1+n]
	return isHTMLSpace(c) || c == '/' || c == '>'
}

// tagEnd returns the index just after the ">" that ends the start tag at
// the start of b (whose name is nameLen bytes long), following the HTML
// tokenizer: a quote starts a quoted value only right after "=" (and
// whitespace), where a ">" inside it does not end the tag; anywhere else a
// quote is an ordinary character and the next ">" ends the tag. -1 when the
// tag does not end.
func tagEnd(b []byte, nameLen int) int {
	const (
		beforeName = iota
		name
		afterName
		beforeValue
		unquoted
		afterQuoted
		selfClosing
	)
	state := beforeName
	for i := 1 + nameLen; i < len(b); i++ {
		c := b[i]
		switch state {
		case beforeName:
			switch {
			case isHTMLSpace(c):
			case c == '/':
				state = selfClosing
			case c == '>':
				return i + 1
			default:
				state = name // also "=": it starts a name here
			}
		case name:
			switch {
			case isHTMLSpace(c):
				state = afterName
			case c == '/':
				state = selfClosing
			case c == '=':
				state = beforeValue
			case c == '>':
				return i + 1
			}
		case afterName:
			switch {
			case isHTMLSpace(c):
			case c == '/':
				state = selfClosing
			case c == '=':
				state = beforeValue
			case c == '>':
				return i + 1
			default:
				state = name
			}
		case beforeValue:
			switch {
			case isHTMLSpace(c):
			case c == '"' || c == '\'':
				end := bytes.IndexByte(b[i+1:], c)
				if end < 0 {
					return -1
				}
				i += 1 + end
				state = afterQuoted
			case c == '>':
				return i + 1
			default:
				state = unquoted
			}
		case unquoted:
			switch {
			case isHTMLSpace(c):
				state = beforeName
			case c == '>':
				return i + 1
			}
		case afterQuoted:
			switch {
			case isHTMLSpace(c):
				state = beforeName
			case c == '/':
				state = selfClosing
			case c == '>':
				return i + 1
			default:
				state = name // a missing space: a new attribute name
			}
		case selfClosing:
			if c == '>' {
				return i + 1
			}
			// Anything else is read again as the start of an attribute.
			i--
			state = beforeName
		}
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
