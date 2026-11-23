package servers

import (
	"bytes"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
)

// MaxLogLine bounds one sanitised line of container output; the rest of a
// longer line is dropped and TruncatedMarker appended.
const MaxLogLine = 16 << 10

// TruncatedMarker ends a line that was cut at MaxLogLine.
const TruncatedMarker = " [line truncated]"

// JobLogPrefix starts every line of container output copied into a job log,
// so it cannot be mistaken for a message of the daemon.
const JobLogPrefix = "container | "

// sanitizer is an io.Writer that makes container output safe to show: it
// splits the stream into lines, replaces invalid UTF-8 with U+FFFD, escapes
// C0 controls (except tab and newline), DEL, C1 controls, ESC, the Unicode
// line/paragraph separators and bidirectional overrides visibly (\x1b,
// \u0085, \u202e, ...), caps each line at MaxLogLine bytes and prefixes each
// line with prefix. Multi-byte characters and lines split across writes are
// handled; Close flushes a final partial line. Not safe for concurrent use
// (os/exec serialises writes when one writer serves stdout and stderr).
type sanitizer struct {
	w      io.Writer
	prefix string
	line   []byte // pending raw bytes of the current line
	cut    bool   // the current line exceeded MaxLogLine
}

func newSanitizer(w io.Writer, prefix string) *sanitizer {
	return &sanitizer{w: w, prefix: prefix}
}

func (s *sanitizer) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if !s.cut {
			// Keep a few extra bytes so a character cut by the limit is
			// still decoded as one.
			room := MaxLogLine + utf8.UTFMax - len(s.line)
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
		if err := s.emit(); err != nil {
			return 0, err
		}
		p = p[i+1:]
	}
	return n, nil
}

// emit writes the pending line.
func (s *sanitizer) emit() error {
	out := make([]byte, 0, len(s.prefix)+len(s.line)+32)
	out = append(out, s.prefix...)
	out = appendSafe(out, s.line, MaxLogLine)
	if s.cut || len(s.line) > MaxLogLine {
		out = append(out, TruncatedMarker...)
	}
	out = append(out, '\n')
	s.line, s.cut = s.line[:0], false
	_, err := s.w.Write(out)
	return err
}

// Close flushes a final line without a newline.
func (s *sanitizer) Close() error {
	if len(s.line) == 0 && !s.cut {
		return nil
	}
	return s.emit()
}

// appendSafe appends the sanitised form of raw (decoded up to limit input
// bytes) to out.
func appendSafe(out, raw []byte, limit int) []byte {
	for i := 0; i < len(raw) && i < limit; {
		r, size := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && size == 1 && len(raw) > limit && !utf8.FullRune(raw[i:]) {
			break // a character cut off by the line limit, not invalid input
		}
		i += size
		switch {
		case r == utf8.RuneError && size == 1:
			out = append(out, "\uFFFD"...)
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
	}
	return out
}

// redact replaces, in container output, every line (of at least 4 bytes)
// of every env value with "[redacted]". It is best effort: a game that
// prints a value transformed (encoded, split, reversed) is not caught.
func redact(b []byte, env map[string]string) []byte {
	return []byte(docker.RedactValues(string(b), env))
}
