// Package backups turns a server's data directory into stored archives and
// back: it normalises the tar stream "docker cp" produces (everything in it
// was written by the game and is untrusted), reads the schema fingerprint
// from the entity snapshot header, stores archives below the data
// directory and rewrites a stored archive for "docker cp" into a fresh
// volume. It never interprets file content beyond the snapshot header.
package backups

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin"
)

// Limits of an archive besides the content size (--backup-max-mb).
const (
	// MaxEntries bounds the entries of a data directory: every tar header
	// counts, also those of skipped entries and the PAX or GNU records that
	// precede an entry.
	MaxEntries = 100000
	// EntryAllowance is how many bytes of the copy stream each entry may
	// take besides its content (header blocks, PAX or GNU records,
	// padding). The stream may hold at most the content limit plus this
	// allowance per entry; more is ErrTooLarge.
	EntryAllowance = 16 << 10
	// MaxPathBytes, MaxNameBytes and MaxDepth bound an entry's path.
	MaxPathBytes = 1024
	MaxNameBytes = 255
	MaxDepth     = 32
	// SnapshotFile is the entity snapshot Golem Engine keeps at the top of
	// its snapshot directory.
	SnapshotFile = "world.snapshot"
	// maxHeaderBytes is how much of the snapshot is read for its header:
	// magic, three uvarints and a fingerprint of at most 128 bytes.
	maxHeaderBytes = 4 + 3*10 + 128
)

// Errors of Normalize and Rewrite.
var (
	// ErrTooLarge: the content exceeds the size limit.
	ErrTooLarge = errors.New("backups: the data directory exceeds the size limit")
	// ErrTooManyEntries: more than MaxEntries entries.
	ErrTooManyEntries = errors.New("backups: the data directory has too many entries")
	// ErrDamaged: a stored archive does not read back as written.
	ErrDamaged = errors.New("backups: the archive is damaged")
	// ErrBadStream: the copy stream is not a tar archive of the directory.
	ErrBadStream = errors.New("backups: the copied data is not a valid archive")
)

// Skipped is an entry left out of a backup.
type Skipped struct {
	// Name is the entry's name, sanitised for logs (logsafe) and clipped.
	Name string
	// Reason says why: "symbolic link", "hard link", "named pipe",
	// "device", "unsupported type", "invalid path", "duplicate",
	// "outside a kept directory".
	Reason string
}

// Result describes a normalised archive.
type Result struct {
	// Files and Dirs count the kept entries; ContentBytes is the size of
	// the kept files.
	Files        int
	Dirs         int
	ContentBytes int64
	// Skipped lists left-out entries (at most maxSkippedListed; SkippedCount
	// is the total).
	Skipped      []Skipped
	SkippedCount int
	// Fingerprint is the schema fingerprint of the top-level world.snapshot
	// ("" without one or when its header is not readable).
	Fingerprint string
}

const maxSkippedListed = 50

func (r *Result) skip(name, reason string, redact func(string) string) {
	r.SkippedCount++
	if len(r.Skipped) < maxSkippedListed {
		if redact != nil {
			name = redact(name)
		}
		r.Skipped = append(r.Skipped, Skipped{Name: safeName(name), Reason: reason})
	}
}

// errBudget: the copy stream is longer than its content limit plus
// EntryAllowance per entry.
var errBudget = fmt.Errorf("%w: the copy stream exceeds its byte budget", ErrTooLarge)

// budgetReader fails with errBudget once more than limit() bytes were read.
type budgetReader struct {
	r     io.Reader
	read  int64
	limit func() int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	room := b.limit() - b.read
	if room <= 0 {
		return 0, errBudget
	}
	if int64(len(p)) > room {
		p = p[:room]
	}
	n, err := b.r.Read(p)
	b.read += int64(n)
	return n, err
}

// metaRecords counts the PAX and GNU records the tar reader merged into h:
// each was a header of its own in the stream.
func metaRecords(h *tar.Header) int {
	n := 0
	if len(h.PAXRecords) > 0 {
		n++
	}
	if h.Format&tar.FormatGNU != 0 {
		if len(h.Name) > 100 {
			n++ // a GNU long name record
		}
		if len(h.Linkname) > 100 {
			n++ // a GNU long link record
		}
	}
	return n
}

// streamErr maps an error reading the copy stream.
func streamErr(err error) error {
	if errors.Is(err, errBudget) {
		return errBudget
	}
	return fmt.Errorf("%w: %w", ErrBadStream, err)
}

// cleanRel validates a relative entry path: valid UTF-8 without control
// characters, clean, not absolute, no "." or ".." segments, bounded.
func cleanRel(p string) bool {
	if p == "" || len(p) > MaxPathBytes || !utf8.ValidString(p) || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return false
		}
	}
	segs := strings.Split(p, "/")
	if len(segs) > MaxDepth {
		return false
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || len(s) > MaxNameBytes {
			return false
		}
	}
	return true
}

// Modes stored for kept entries: the owner can always read and write,
// group and others never write, and special bits are dropped.
func fileMode(m int64) int64 { return m&0o755 | 0o600 }
func dirMode(m int64) int64  { return m&0o755 | 0o700 }

// modTime keeps a plausible modification time, otherwise the Unix epoch.
func modTime(t time.Time) time.Time {
	if t.Before(time.Unix(0, 0)) || t.After(time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)) {
		return time.Unix(0, 0).UTC()
	}
	return t.UTC().Truncate(time.Second)
}

// headWriter keeps the first max bytes written to it.
type headWriter struct {
	buf bytes.Buffer
	max int
}

func (h *headWriter) Write(p []byte) (int, error) {
	if room := h.max - h.buf.Len(); room > 0 {
		h.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// tree tracks kept directories and names so every kept entry sits in a
// kept directory and appears once.
type tree struct {
	names map[string]bool
	dirs  map[string]bool
}

func newTree() *tree { return &tree{names: map[string]bool{}, dirs: map[string]bool{".": true}} }

// Normalize reads the tar stream of "docker cp <helper>:<dir> -" (entries
// start with base + "/") and writes a gzip-compressed tar of the kept
// entries to w. Only regular files and directories are kept; links,
// pipes, devices and entries with unsafe paths are skipped and reported.
// Kept entries get uid and gid 0, masked modes and no extended attributes.
// Every entry's content counts against maxContent, also a skipped one's,
// and is refused before it is read: more is ErrTooLarge, like a stream
// longer than maxContent plus EntryAllowance per entry. More than
// MaxEntries headers is ErrTooManyEntries. Both stop at once.
func Normalize(r io.Reader, base string, maxContent int64, w io.Writer) (Result, error) {
	return NormalizeRedacted(r, base, maxContent, nil, w)
}

// NormalizeRedacted is Normalize with redact applied to the raw name of
// every listed skipped entry before it is sanitised (nil: none).
func NormalizeRedacted(r io.Reader, base string, maxContent int64, redact func(string) string, w io.Writer) (Result, error) {
	var res Result
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	entries := 0
	br := &budgetReader{r: r, limit: func() int64 { return maxContent + int64(entries+2)*EntryAllowance }}
	tr := tar.NewReader(br)
	t := newTree()
	var data int64 // content of every entry so far, kept or not
	var head *headWriter
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, streamErr(err)
		}
		entries += 1 + metaRecords(h)
		if entries > MaxEntries {
			return res, ErrTooManyEntries
		}
		if h.Size < 0 || h.Size > maxContent-data {
			return res, ErrTooLarge
		}
		data += h.Size
		name := strings.TrimSuffix(h.Name, "/")
		if name == base {
			continue // the directory itself
		}
		rel, ok := strings.CutPrefix(name, base+"/")
		if !ok {
			res.skip(name, "invalid path", redact)
			continue
		}
		kind := ""
		switch h.Typeflag {
		case tar.TypeReg: // the reader reports old-style regular files as TypeReg too
		case tar.TypeDir:
		case tar.TypeSymlink:
			kind = "symbolic link"
		case tar.TypeLink:
			kind = "hard link"
		case tar.TypeFifo:
			kind = "named pipe"
		case tar.TypeChar, tar.TypeBlock:
			kind = "device"
		default:
			kind = "unsupported type"
		}
		switch {
		case kind != "":
			res.skip(rel, kind, redact)
			continue
		case !cleanRel(rel):
			res.skip(rel, "invalid path", redact)
			continue
		case t.names[rel]:
			res.skip(rel, "duplicate", redact)
			continue
		case !t.dirs[path.Dir(rel)]:
			res.skip(rel, "outside a kept directory", redact)
			continue
		}
		t.names[rel] = true
		out := &tar.Header{Name: rel, ModTime: modTime(h.ModTime), Format: tar.FormatPAX}
		if h.Typeflag == tar.TypeDir {
			t.dirs[rel] = true
			out.Name += "/"
			out.Typeflag, out.Mode = tar.TypeDir, dirMode(h.Mode)
			if err := tw.WriteHeader(out); err != nil {
				return res, err
			}
			res.Dirs++
			continue
		}
		out.Typeflag, out.Mode, out.Size = tar.TypeReg, fileMode(h.Mode), h.Size
		if err := tw.WriteHeader(out); err != nil {
			return res, err
		}
		var dst io.Writer = tw
		if rel == SnapshotFile {
			head = &headWriter{max: maxHeaderBytes}
			dst = io.MultiWriter(tw, head)
		}
		n, err := io.Copy(dst, io.LimitReader(tr, h.Size))
		if err != nil {
			return res, streamErr(err) // a write error (no space) stays matchable
		}
		if n != h.Size {
			return res, fmt.Errorf("%w: short file", ErrBadStream)
		}
		res.ContentBytes += n
		res.Files++
	}
	if err := tw.Close(); err != nil {
		return res, err
	}
	if err := gz.Close(); err != nil {
		return res, err
	}
	if head != nil {
		res.Fingerprint = ParseSnapshotHeader(head.buf.Bytes())
	}
	return res, nil
}

// snapshotMagic starts every Golem Engine entity snapshot.
var snapshotMagic = []byte{'G', 'L', 'M', 0x01}

// ParseSnapshotHeader returns the schema fingerprint of a world.snapshot
// header: the magic, a uvarint format version (1 or 2), a uvarint
// timestamp, a uvarint length and that many bytes of fingerprint. Anything
// unexpected, or a fingerprint that is not a plausible one, gives "".
func ParseSnapshotHeader(b []byte) string {
	if !bytes.HasPrefix(b, snapshotMagic) {
		return ""
	}
	b = b[len(snapshotMagic):]
	version, n := uvarint(b)
	if n <= 0 || (version != 1 && version != 2) {
		return ""
	}
	b = b[n:]
	if _, n = uvarint(b); n <= 0 {
		return ""
	}
	b = b[n:]
	length, n := uvarint(b)
	if n <= 0 || length == 0 || length > 128 || uint64(len(b)-n) < length {
		return ""
	}
	return gameadmin.Fingerprint(string(b[n : n+int(length)]))
}

// uvarint decodes a canonical uvarint of at most 10 bytes (n <= 0 on error).
func uvarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i := 0; i < len(b) && i < 10; i++ {
		c := b[i]
		if i == 9 && c > 1 {
			return 0, -1
		}
		if c < 0x80 {
			if i > 0 && c == 0 {
				return 0, -1 // not canonical
			}
			return x | uint64(c)<<s, i + 1
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, 0
}

// Rewrite reads a stored archive (gzip tar written by Normalize) and
// writes a plain tar for "docker cp - <helper>:<dir>" with every entry
// owned by uid and gid. The archive is checked again entry by entry: it
// must contain only clean regular files and directories, exactly
// wantFiles files and wantContent bytes, otherwise ErrDamaged.
func Rewrite(r io.Reader, uid, gid int, wantFiles int, wantContent int64, w io.Writer) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDamaged, err)
	}
	gz.Multistream(false)
	tr := tar.NewReader(gz)
	tw := tar.NewWriter(w)
	t := newTree()
	files, entries := 0, 0
	var content int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrDamaged, err)
		}
		entries++
		rel := strings.TrimSuffix(h.Name, "/")
		if entries > MaxEntries || !cleanRel(rel) || t.names[rel] || !t.dirs[path.Dir(rel)] {
			return ErrDamaged
		}
		t.names[rel] = true
		out := &tar.Header{Name: h.Name, ModTime: modTime(h.ModTime), Uid: uid, Gid: gid, Format: tar.FormatPAX}
		switch h.Typeflag {
		case tar.TypeDir:
			t.dirs[rel] = true
			out.Typeflag, out.Mode = tar.TypeDir, dirMode(h.Mode)
			if err := tw.WriteHeader(out); err != nil {
				return err
			}
			continue
		case tar.TypeReg:
		default:
			return ErrDamaged
		}
		if h.Size < 0 || h.Size > wantContent-content {
			return ErrDamaged
		}
		out.Typeflag, out.Mode, out.Size = tar.TypeReg, fileMode(h.Mode), h.Size
		if err := tw.WriteHeader(out); err != nil {
			return err
		}
		n, err := io.Copy(tw, io.LimitReader(tr, h.Size))
		if err != nil {
			return err
		}
		if n != h.Size {
			return ErrDamaged
		}
		content += n
		files++
	}
	if files != wantFiles || content != wantContent {
		return ErrDamaged
	}
	return tw.Close()
}
