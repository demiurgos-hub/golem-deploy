// Package extract unpacks static ZIP add-ons into fresh staging
// directories with hard limits: total and per-file size, entry count,
// compression ratio (per entry and against the real archive size), path
// depth and length. The archive's own records are checked first: entries
// whose data overlaps (several entries sharing compressed bytes) or whose
// local header names another path are rejected. Entry paths are never
// trusted:
// absolute, traversing, backslashed, device-like and colliding paths
// (including case-insensitive collisions) are rejected, only regular files
// and directories are accepted and file modes are reduced to 0644/0755.
// The destination is always a new directory created by Zip itself, never an
// existing tree such as a repository checkout.
package extract

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/staticfiles"
)

// Limits bounds one extraction.
type Limits struct {
	MaxTotalBytes int64 // sum of uncompressed file sizes
	MaxFileBytes  int64 // one file's uncompressed size
	MaxEntries    int   // files and directories
	// MaxRatio bounds uncompressed/compressed size, checked for the whole
	// archive, for each entry larger than ratioMinBytes and against the
	// archive file's real size (declared and actually written bytes).
	MaxRatio     int64
	MaxDepth     int   // path segments
	MaxPathLen   int   // bytes of an entry path
	ReserveBytes int64 // unallocated space to leave on the staging filesystem
}

// DefaultLimits suits game add-ons (a few GiB of assets at most).
func DefaultLimits() Limits {
	return Limits{
		MaxTotalBytes: 4 << 30,
		MaxFileBytes:  2 << 30,
		MaxEntries:    20000,
		MaxRatio:      200,
		MaxDepth:      32,
		MaxPathLen:    512,
		ReserveBytes:  staticfiles.DefaultReserveBytes,
	}
}

// ratioMinBytes exempts small entries from the per-entry ratio check
// (small runs of zeros legitimately compress very well).
const ratioMinBytes = 1 << 20

// ErrUnsafe is wrapped by every rejection of archive content.
var ErrUnsafe = errors.New("unsafe or oversized archive")

func unsafe(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsafe, fmt.Sprintf(format, a...))
}

// cleanEntry validates an entry name and returns it in slash form without
// a trailing slash.
func cleanEntry(name string, lim Limits) (string, error) {
	if name == "" || len(name) > lim.MaxPathLen {
		return "", unsafe("entry path is empty or too long")
	}
	if strings.ContainsAny(name, "\\:\x00") {
		return "", unsafe("entry %q contains a backslash, colon or NUL", clip(name))
	}
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" || strings.HasPrefix(trimmed, "/") {
		return "", unsafe("entry %q is absolute", clip(name))
	}
	segs := strings.Split(trimmed, "/")
	if len(segs) > lim.MaxDepth {
		return "", unsafe("entry %q is nested too deeply", clip(name))
	}
	for _, s := range segs {
		switch {
		case s == "" || s == "." || s == "..":
			return "", unsafe("entry %q has an empty, '.' or '..' segment", clip(name))
		case strings.HasSuffix(s, ".") || strings.HasSuffix(s, " "):
			return "", unsafe("entry %q has a segment ending in a dot or space", clip(name))
		case reservedDevice(s):
			return "", unsafe("entry %q uses a reserved device name", clip(name))
		}
		for _, r := range s {
			if r < 0x20 || r == 0x7f {
				return "", unsafe("entry %q contains control characters", clip(name))
			}
		}
	}
	if path.Clean(trimmed) != trimmed || !filepath.IsLocal(filepath.FromSlash(trimmed)) {
		return "", unsafe("entry %q is not a local path", clip(name))
	}
	return trimmed, nil
}

func reservedDevice(seg string) bool {
	base, _, _ := strings.Cut(strings.ToUpper(seg), ".")
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9'
}

func clip(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

type entry struct {
	f    *zip.File
	path string
	dir  bool
	exec bool
}

// ratioBudget is the most output an archive of archiveSize bytes may
// produce: MaxRatio times its real size (at least ratioMinBytes), and never
// more than MaxTotalBytes.
func ratioBudget(lim Limits, archiveSize int64) int64 {
	b := lim.MaxTotalBytes
	if lim.MaxRatio > 0 && archiveSize > 0 && archiveSize <= b/lim.MaxRatio {
		b = max(archiveSize*lim.MaxRatio, ratioMinBytes)
	}
	return min(b, lim.MaxTotalBytes)
}

// plan validates the central directory before anything is written.
func plan(files []*zip.File, lim Limits, archiveSize int64) ([]entry, error) {
	if len(files) > lim.MaxEntries {
		return nil, unsafe("archive has more than %d entries", lim.MaxEntries)
	}
	var total, compressed uint64
	seen := map[string]bool{} // lower-cased paths
	isFile := map[string]bool{}
	entries := make([]entry, 0, len(files))
	for _, f := range files {
		p, err := cleanEntry(f.Name, lim)
		if err != nil {
			return nil, err
		}
		mode := f.Mode()
		dir := mode.IsDir() || strings.HasSuffix(f.Name, "/")
		if !dir && !mode.IsRegular() {
			return nil, unsafe("entry %q is not a regular file or directory (links and devices are not allowed)", clip(f.Name))
		}
		key := strings.ToLower(p)
		if seen[key] {
			return nil, unsafe("entry %q collides with another entry (paths are compared ignoring case)", clip(f.Name))
		}
		seen[key] = true
		if !dir {
			isFile[key] = true
			if f.UncompressedSize64 > uint64(lim.MaxFileBytes) {
				return nil, unsafe("entry %q exceeds the %d byte file limit", clip(f.Name), lim.MaxFileBytes)
			}
			if f.UncompressedSize64 > ratioMinBytes && f.UncompressedSize64 > uint64(lim.MaxRatio)*max(f.CompressedSize64, 1) {
				return nil, unsafe("entry %q has a suspicious compression ratio", clip(f.Name))
			}
			total += f.UncompressedSize64
			compressed += f.CompressedSize64
			if total > uint64(lim.MaxTotalBytes) {
				return nil, unsafe("archive exceeds the %d byte total limit", lim.MaxTotalBytes)
			}
		}
		entries = append(entries, entry{f: f, path: p, dir: dir, exec: mode&0o111 != 0})
	}
	if total > ratioMinBytes && total > uint64(lim.MaxRatio)*max(compressed, 1) {
		return nil, unsafe("archive has a suspicious compression ratio")
	}
	if total > uint64(ratioBudget(lim, archiveSize)) {
		return nil, unsafe("archive expands to more than %d times its size", lim.MaxRatio)
	}
	// A file may not also be a parent directory of another entry.
	for key := range seen {
		for dir := path.Dir(key); dir != "."; dir = path.Dir(dir) {
			if isFile[dir] {
				return nil, unsafe("entry %q is both a file and a directory", clip(dir))
			}
		}
	}
	return entries, nil
}

// Zip extracts the archive at zipPath into a new directory created with
// os.MkdirTemp under tmpRoot and returns that directory. On any error
// nothing is left behind. The caller removes the directory when done.
func Zip(ctx context.Context, zipPath, tmpRoot string, lim Limits) (Result, error) {
	fail := func(err error) (Result, error) { return Result{}, err }
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	f, err := os.Open(zipPath)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	size := info.Size()
	reader := contextReaderAt{ctx, f}
	// Bound both the declared and actual metadata before archive/zip
	// allocates its File records. Read from the same open file throughout.
	cd, cdOffset, err := readCentral(reader, size, lim.MaxEntries)
	if err != nil {
		return fail(err)
	}
	if err := checkLayout(reader, cd, cdOffset); err != nil {
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		return fail(err)
	}
	r, err := zip.NewReader(reader, size)
	if err != nil {
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		return fail(unsafe("invalid ZIP archive"))
	}
	if len(cd) != len(r.File) {
		return fail(unsafe("central directory entry count mismatch"))
	}
	for i, entry := range cd {
		zf := r.File[i]
		if string(entry.name) != zf.Name || entry.compressed != zf.CompressedSize64 || entry.uncompressed != zf.UncompressedSize64 {
			return fail(unsafe("central directory mismatch at entry %d", i))
		}
	}
	entries, err := plan(r.File, lim, size)
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return fail(err)
	}
	var total int64
	for _, e := range entries {
		if !e.dir {
			total += int64(e.f.UncompressedSize64)
		}
	}
	if err := staticfiles.CheckFreeSpace(tmpRoot, total, lim.ReserveBytes); err != nil {
		return fail(err)
	}
	dest, err := os.MkdirTemp(tmpRoot, "static-extract-")
	if err != nil {
		return fail(err)
	}
	written, err := extractAll(ctx, entries, dest, lim, ratioBudget(lim, size))
	if err != nil {
		_ = os.RemoveAll(dest)
		return fail(err)
	}
	return Result{Dir: dest, Entries: len(entries), Bytes: written}, nil
}

// Result reports the consumed build budget and the extracted directory.
type Result struct {
	Dir     string
	Entries int
	Bytes   int64
}

type contextReaderAt struct {
	ctx context.Context
	io.ReaderAt
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func (r contextReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReaderAt.ReadAt(p, off)
}

// extractAll writes the planned entries; budget bounds the bytes actually
// written in total.
func extractAll(ctx context.Context, entries []entry, dest string, lim Limits, budget int64) (int64, error) {
	var written int64
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		target := filepath.Join(dest, filepath.FromSlash(e.path))
		if rel, err := filepath.Rel(dest, target); err != nil || !filepath.IsLocal(rel) {
			return 0, unsafe("entry %q escapes the destination", clip(e.path))
		}
		if e.dir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return 0, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return 0, err
		}
		mode := fs.FileMode(0o644)
		if e.exec {
			mode = 0o755
		}
		n, err := writeFile(ctx, e.f, target, mode, min(lim.MaxFileBytes, budget-written), dest, lim.ReserveBytes)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, ctx.Err()
}

// writeFile copies one entry, never trusting its declared size: at most
// limit bytes are accepted.
func writeFile(ctx context.Context, f *zip.File, target string, mode fs.FileMode, limit int64, dest string, reserve int64) (int64, error) {
	src, err := f.Open()
	if err != nil {
		return 0, fmt.Errorf("open entry %q: %w", clip(f.Name), err)
	}
	defer src.Close()
	// O_EXCL: the staging tree is fresh, so an existing path means two
	// entries resolved to the same file.
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return 0, unsafe("entry %q collides with another entry", clip(f.Name))
		}
		return 0, err
	}
	reader := io.LimitReader(contextReader{ctx, src}, limit+1)
	var n int64
	for err == nil {
		if err = ctx.Err(); err != nil {
			break
		}
		const chunk = 64 << 20
		need := min(chunk, max(0, int64(f.UncompressedSize64)-n))
		if err = staticfiles.CheckFreeSpace(dest, need, reserve); err != nil {
			break
		}
		var copied int64
		copied, err = io.CopyN(dst, reader, chunk)
		n += copied
		if errors.Is(err, io.EOF) {
			err = nil
			break
		}
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if errors.Is(err, zip.ErrFormat) || errors.Is(err, zip.ErrChecksum) {
			return n, unsafe("entry %q is corrupt or larger than declared", clip(f.Name))
		}
		return n, err
	}
	if n > limit {
		return n, unsafe("entry %q exceeds the size limit", clip(f.Name))
	}
	return n, nil
}
