// Package logsafe makes untrusted process output (docker build output, which
// includes whatever a game repository's build steps print) safe to store in
// job logs and show to users: one line at a time, control and
// bidirectional characters escaped visibly, invalid UTF-8 replaced, lines
// capped, a fixed prefix per line and literal strings (host paths)
// replaced before anything is written.
package logsafe

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// MaxLine bounds one sanitised line; the rest of a longer line is dropped
// and TruncatedMarker appended.
const MaxLine = 16 << 10

// TruncatedMarker ends a line that was cut at MaxLine.
const TruncatedMarker = " [line truncated]"

// Writer is an io.Writer that splits a stream into lines, replaces the
// configured literals in each complete line, escapes C0 controls (except
// tab), DEL, C1 controls, the Unicode line/paragraph separators and
// bidirectional controls visibly (ESC as \x1b, NEL as \u0085, and so on),
// replaces invalid UTF-8
// with U+FFFD, caps each line at MaxLine bytes and prefixes it. Carriage
// returns end a line too (progress output). Close flushes a final partial
// line. Safe for concurrent use.
type Writer struct {
	w        io.Writer
	prefix   string
	replacer *strings.Replacer
	line     []byte
	cut      bool
	mu       chan struct{}
}

// NewWriter returns a Writer writing to w. replace holds old/new pairs
// applied to every complete line before it is escaped (for example a host
// path and a placeholder); the longest match wins at each position.
func NewWriter(w io.Writer, prefix string, replace ...string) *Writer {
	s := &Writer{w: w, prefix: string(AppendSafe(nil, []byte(prefix), MaxLine-len(TruncatedMarker))), mu: make(chan struct{}, 1)}
	var pairs []string
	for i := 0; i+1 < len(replace); i += 2 {
		if replace[i] != "" {
			pairs = append(pairs, replace[i], replace[i+1])
		}
	}
	if len(pairs) > 0 {
		s.replacer = strings.NewReplacer(pairs...)
	}
	return s
}

func (s *Writer) lock()   { s.mu <- struct{}{} }
func (s *Writer) unlock() { <-s.mu }

// keep is how many raw bytes of a line are retained: enough for MaxLine
// output bytes after the longest replacement shrinks it, plus a partial
// character.
const keep = 2*MaxLine + utf8.UTFMax

// Write implements io.Writer. It never fails because of the content.
func (s *Writer) Write(p []byte) (int, error) {
	s.lock()
	defer s.unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexAny(p, "\r\n")
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if !s.cut {
			room := keep - len(s.line)
			if len(chunk) > room {
				s.line = append(s.line, chunk[:max(room, 0)]...)
				s.cut = true
			} else {
				s.line = append(s.line, chunk...)
			}
		}
		if i < 0 {
			break
		}
		// "\r\n" ends one line, not two.
		if p[i] == '\r' && i+1 < len(p) && p[i+1] == '\n' {
			i++
		}
		if err := s.emit(); err != nil {
			return 0, err
		}
		p = p[i+1:]
	}
	return n, nil
}

func (s *Writer) emit() error {
	raw := s.line
	if s.replacer != nil {
		raw = []byte(s.replacer.Replace(string(raw)))
	}
	out := make([]byte, 0, len(s.prefix)+min(len(raw), MaxLine)+32)
	out = append(out, s.prefix...)
	prefixLen := len(out)
	out, truncated := appendSafe(out, raw, MaxLine-prefixLen)
	if s.cut || truncated {
		out, _ = appendSafe(out[:prefixLen], raw, MaxLine-prefixLen-len(TruncatedMarker))
		out = append(out, TruncatedMarker...)
	}
	out = append(out, '\n')
	s.line, s.cut = s.line[:0], false
	_, err := s.w.Write(out)
	return err
}

// Close flushes a final line without a newline.
func (s *Writer) Close() error {
	s.lock()
	defer s.unlock()
	if len(s.line) == 0 && !s.cut {
		return nil
	}
	return s.emit()
}

// AppendSafe appends at most limit bytes of the sanitised form of raw to out.
func AppendSafe(out, raw []byte, limit int) []byte {
	out, _ = appendSafe(out, raw, limit)
	return out
}

func appendSafe(out, raw []byte, limit int) ([]byte, bool) {
	end := len(out) + limit
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		start := len(out)
		i += size
		switch {
		case r == utf8.RuneError && size == 1:
			out = append(out, "�"...)
		case r == '\t':
			out = append(out, '\t')
		case r < 0x20 || r == 0x7f:
			out = fmt.Appendf(out, `\x%02x`, r)
		case r >= 0x80 && r <= 0x9f,
			r == 0x2028, r == 0x2029,
			r >= 0x202a && r <= 0x202e,
			r >= 0x2066 && r <= 0x2069,
			r == 0x200e, r == 0x200f, r == 0x061c:
			out = fmt.Appendf(out, `\u%04x`, r)
		default:
			out = utf8.AppendRune(out, r)
		}
		if len(out) > end {
			return out[:start], true
		}
	}
	return out, false
}

// String returns the sanitised form of s on one line (newlines escaped),
// capped at MaxLine rendered bytes.
func String(s string) string {
	return string(AppendSafe(nil, []byte(s), MaxLine))
}
