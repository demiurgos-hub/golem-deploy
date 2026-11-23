package git

import (
	"bytes"
	"io"
	"net/url"
	"regexp"
	"sort"
	"sync"
)

// userinfoRE matches "scheme://user:password@" so the password part can be
// redacted from any output line.
var userinfoRE = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^\s/@:]*):[^\s/@]*@`)

const redacted = "***"

// scrubber redacts secrets and URL passwords from text before it reaches
// the job log. It is line-buffered so a secret split across writes is still
// found; call Flush at the end. It also keeps a tail of the scrubbed output
// for error classification.
type scrubber struct {
	mu      sync.Mutex
	w       io.Writer
	secrets [][]byte
	maxLen  int
	buf     []byte
	over    bool // the current line exceeded maxLine and is being dropped
	tail    []byte
}

const (
	maxLine = 64 << 10
	maxTail = 8 << 10
)

func newScrubber(w io.Writer, secrets []string) *scrubber {
	s := &scrubber{w: w}
	seen := map[string]bool{}
	for _, v := range secrets {
		for _, form := range []string{v, url.QueryEscape(v), url.PathEscape(v)} {
			if form == "" || seen[form] {
				continue
			}
			seen[form] = true
			s.secrets = append(s.secrets, []byte(form))
			s.maxLen = max(s.maxLen, len(form))
		}
	}
	// Longest first, so a secret that contains another is fully replaced.
	sort.Slice(s.secrets, func(i, j int) bool { return len(s.secrets[i]) > len(s.secrets[j]) })
	return s
}

// scrub returns line with secrets and URL passwords replaced.
func (s *scrubber) scrub(line []byte) []byte {
	for _, sec := range s.secrets {
		line = bytes.ReplaceAll(line, sec, []byte(redacted))
	}
	return userinfoRE.ReplaceAll(line, []byte("${1}:"+redacted+"@"))
}

// scrubString is scrub for strings.
func (s *scrubber) scrubString(v string) string { return string(s.scrub([]byte(v))) }

func (s *scrubber) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexAny(p, "\r\n")
		if i < 0 {
			s.appendPartial(p)
			break
		}
		end := i + 1
		if p[i] == '\r' && end < len(p) && p[end] == '\n' {
			end++
		}
		s.appendPartial(p[:end])
		p = p[end:]
		s.emit()
	}
	return n, nil
}

// appendPartial adds to the current line, dropping what exceeds maxLine.
func (s *scrubber) appendPartial(p []byte) {
	if s.over {
		if c := p[len(p)-1]; c == '\n' || c == '\r' {
			s.buf = append(s.buf, c)
		}
		return
	}
	room := maxLine - len(s.buf)
	if len(p) <= room {
		s.buf = append(s.buf, p...)
		return
	}
	s.buf = append(s.buf, p[:room]...)
	s.over = true
	if c := p[len(p)-1]; c == '\n' || c == '\r' {
		s.buf = append(s.buf, c)
	}
}

func (s *scrubber) emit() {
	if len(s.buf) == 0 {
		return
	}
	line := s.buf
	if s.over {
		// The cut may have split a secret: drop enough of the end that no
		// partial secret survives.
		end := line[len(line)-1]
		keep := max(0, maxLine-s.maxLen-1)
		line = append(append([]byte{}, line[:min(keep, len(line))]...), []byte(" [line truncated]")...)
		if end == '\n' || end == '\r' {
			line = append(line, end)
		}
	}
	out := s.scrub(line)
	_, _ = s.w.Write(out)
	s.tail = append(s.tail, out...)
	if len(s.tail) > maxTail {
		s.tail = append([]byte{}, s.tail[len(s.tail)-maxTail:]...)
	}
	s.buf = s.buf[:0]
	s.over = false
}

// Flush writes a pending partial line.
func (s *scrubber) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit()
}

// resetTail forgets the recorded output (called before each command).
func (s *scrubber) resetTail() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tail = s.tail[:0]
}

// Tail returns the last scrubbed output.
func (s *scrubber) Tail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.tail)
}

// println writes a scrubbed line.
func (s *scrubber) println(text string) {
	_, _ = s.Write([]byte(text + "\n"))
}
