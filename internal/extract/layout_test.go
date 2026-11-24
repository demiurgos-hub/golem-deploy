package extract

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/staticfiles"
)

// rawEntry is one entry of a hand-built archive.
type rawEntry struct {
	name       string // central directory name
	localName  string // local header name (default name)
	offset     uint32 // local header offset (set by the builder unless fixed)
	extra      []byte // local extra field
	method     uint16
	crc        uint32
	compressed uint32
	size       uint32
}

func le16(b *bytes.Buffer, v uint16) { _ = binary.Write(b, binary.LittleEndian, v) }
func le32(b *bytes.Buffer, v uint32) { _ = binary.Write(b, binary.LittleEndian, v) }

func localHeader(e rawEntry) []byte {
	var b bytes.Buffer
	name := e.localName
	if name == "" {
		name = e.name
	}
	le32(&b, sigLocal)
	le16(&b, 20)
	le16(&b, 0)
	le16(&b, e.method)
	le16(&b, 0)
	le16(&b, 0)
	le32(&b, e.crc)
	le32(&b, e.compressed)
	le32(&b, e.size)
	le16(&b, uint16(len(name)))
	le16(&b, uint16(len(e.extra)))
	b.WriteString(name)
	b.Write(e.extra)
	return b.Bytes()
}

// centralAndEnd appends the central directory for entries (at their
// offsets) and the end record to body.
func centralAndEnd(body []byte, entries []rawEntry) []byte {
	var cd bytes.Buffer
	for _, e := range entries {
		le32(&cd, sigCentral)
		le16(&cd, 20)
		le16(&cd, 20)
		le16(&cd, 0)
		le16(&cd, e.method)
		le16(&cd, 0)
		le16(&cd, 0)
		le32(&cd, e.crc)
		le32(&cd, e.compressed)
		le32(&cd, e.size)
		le16(&cd, uint16(len(e.name)))
		le16(&cd, 0)
		le16(&cd, 0)
		le16(&cd, 0)
		le16(&cd, 0)
		le32(&cd, 0o100644<<16)
		le32(&cd, e.offset)
		cd.WriteString(e.name)
	}
	out := append([]byte{}, body...)
	cdOff := len(out)
	out = append(out, cd.Bytes()...)
	var end bytes.Buffer
	le32(&end, sigEOCD)
	le16(&end, 0)
	le16(&end, 0)
	le16(&end, uint16(len(entries)))
	le16(&end, uint16(len(entries)))
	le32(&end, uint32(cd.Len()))
	le32(&end, uint32(cdOff))
	le16(&end, 0)
	return append(out, end.Bytes()...)
}

func deflate(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w, _ := flate.NewWriter(&b, flate.BestCompression)
	_, _ = w.Write(data)
	_ = w.Close()
	return b.Bytes()
}

func save(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.zip")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// overlapping builds the reviewer's archive: n entries whose local headers
// are nested in each other's extra fields so that every entry's data is the
// same compressed bytes. archive/zip accepts it as n distinct files.
func overlapping(t *testing.T, n int, plain []byte) []byte {
	t.Helper()
	comp := deflate(t, plain)
	crc := crc32.ChecksumIEEE(plain)
	entries := make([]rawEntry, n)
	const hdrLen = localLen + 4 // 4-byte names
	for i := range entries {
		entries[i] = rawEntry{name: fmt.Sprintf("f%03d", i), offset: uint32(i * hdrLen), method: zip.Deflate,
			crc: crc, compressed: uint32(len(comp)), size: uint32(len(plain))}
	}
	// Header i quotes headers i+1..n-1 in its extra field.
	var tail []byte
	for i := n - 1; i >= 0; i-- {
		e := entries[i]
		e.extra = tail
		tail = localHeader(e)
	}
	body := append(tail, comp...)
	return centralAndEnd(body, entries)
}

func TestZipRejectsCentralDirectoryBeforeLayout(t *testing.T) {
	archive := centralAndEnd(nil, []rawEntry{{name: "a"}, {name: "b"}})
	lim := DefaultLimits()
	lim.MaxEntries = 1
	if _, err := Zip(t.Context(), save(t, archive), t.TempDir(), lim); !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "remaining build entry limit") {
		t.Fatalf("entry cap must precede local record parsing: %v", err)
	}
	archive = make([]byte, maxCentralBytes+1+eocdLen)
	eocd := archive[len(archive)-eocdLen:]
	binary.LittleEndian.PutUint32(eocd, sigEOCD)
	binary.LittleEndian.PutUint32(eocd[12:], maxCentralBytes+1)
	if _, err := Zip(t.Context(), save(t, archive), t.TempDir(), DefaultLimits()); !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "metadata limit") {
		t.Fatalf("metadata cap: %v", err)
	}
}

func TestZipHonorsCancellationAndDiskReserve(t *testing.T) {
	path := writeZip(t, zentry{name: "asset", content: []byte("asset")})
	root := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Zip(ctx, path, root, DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled extraction: %v", err)
	}
	lim := DefaultLimits()
	lim.ReserveBytes = math.MaxInt64
	if _, err := Zip(t.Context(), path, root, lim); !errors.Is(err, staticfiles.ErrNoSpace) {
		t.Fatalf("disk reserve: %v", err)
	}
	assertEmpty(t, root)
}

// D7: overlapping entries are rejected although each is small enough and
// archive/zip lists them as distinct files.
func TestD7_OverlappingEntries(t *testing.T) {
	plain := make([]byte, 1<<10)
	archive := overlapping(t, 100, plain)
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(zr.File) != 100 {
		t.Fatalf("archive/zip view: %v", err)
	}
	if rc, err := zr.File[57].Open(); err != nil {
		t.Fatalf("entry not readable through archive/zip: %v", err)
	} else {
		rc.Close()
	}
	root := t.TempDir()
	_, err = Zip(context.Background(), save(t, archive), root, DefaultLimits())
	if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("got %v", err)
	}
	assertEmpty(t, root)

	// At scale (the reviewer's 58 KB -> 320 MiB): rejected too.
	big := overlapping(t, 1000, make([]byte, 320<<10))
	t.Logf("overlapping archive: %d bytes declaring %d MiB", len(big), 1000*320/1024)
	if _, err := Zip(context.Background(), save(t, big), root, DefaultLimits()); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("got %v", err)
	}
	assertEmpty(t, root)
}

// D7: the local header must name the same path as the central directory.
func TestD7_LocalNameMismatch(t *testing.T) {
	data := []byte("hello")
	e := rawEntry{name: "good.txt", localName: "../e.txt", method: zip.Store, crc: crc32.ChecksumIEEE(data),
		compressed: uint32(len(data)), size: uint32(len(data))}
	archive := centralAndEnd(append(localHeader(e), data...), []rawEntry{e})
	root := t.TempDir()
	if _, err := Zip(context.Background(), save(t, archive), root, DefaultLimits()); !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "different name") {
		t.Fatalf("got %v", err)
	}
	assertEmpty(t, root)
}

// D7: quine-style archives whose entry data reaches into the central
// directory (so the archive can contain itself) are rejected.
func TestD7_QuineStyleEntryCoversDirectory(t *testing.T) {
	data := []byte("hello")
	e := rawEntry{name: "self.zip", method: zip.Store, crc: crc32.ChecksumIEEE(data), compressed: uint32(len(data)) + 200, size: uint32(len(data)) + 200}
	archive := centralAndEnd(append(localHeader(e), data...), []rawEntry{e})
	root := t.TempDir()
	if _, err := Zip(context.Background(), save(t, archive), root, DefaultLimits()); !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "central directory") {
		t.Fatalf("got %v", err)
	}
	assertEmpty(t, root)
}

// D7: nested archives (42.zip style) are not expanded recursively: the
// inner archive is one ordinary file.
func TestD7_NestedArchiveIsNotExpanded(t *testing.T) {
	inner := writeZip(t, zentry{name: "zeros", content: make([]byte, 900<<10)})
	innerBytes, _ := os.ReadFile(inner)
	var outer bytes.Buffer
	zw := zip.NewWriter(&outer)
	for i := 0; i < 10; i++ {
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("layer/%d.zip", i), Method: zip.Store})
		_, _ = w.Write(innerBytes)
	}
	_ = zw.Close()
	root := t.TempDir()
	dir, err := Zip(context.Background(), save(t, outer.Bytes()), root, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	_ = filepath.Walk(dir.Dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
			if !strings.HasSuffix(p, ".zip") {
				t.Errorf("unexpected file %s", p)
			}
		}
		return nil
	})
	if total != int64(10*len(innerBytes)) {
		t.Fatalf("extracted %d bytes, want %d", total, 10*len(innerBytes))
	}
}

// D7: output is bounded by MaxRatio times the real archive size, also for
// archives whose per-entry ratios stay below the limit.
func TestD7_RatioAgainstArchiveSize(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxRatio = 10
	// Many small, highly compressible entries: each below ratioMinBytes.
	var es []zentry
	for i := 0; i < 8; i++ {
		es = append(es, zentry{name: fmt.Sprintf("f%d", i), content: make([]byte, 512<<10)})
	}
	root := t.TempDir()
	if _, err := Zip(context.Background(), writeZip(t, es...), root, lim); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("got %v", err)
	}
	assertEmpty(t, root)
	if b := ratioBudget(lim, 100); b != ratioMinBytes {
		t.Fatalf("budget for a tiny archive %d", b)
	}
	if b := ratioBudget(Limits{MaxTotalBytes: 50, MaxRatio: 10}, 1000); b != 50 {
		t.Fatalf("budget capped by total %d", b)
	}
}
