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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits bounds one extraction.
type Limits struct {
	MaxTotalBytes int64 // sum of uncompressed file sizes
	MaxFileBytes  int64 // one file's uncompressed size
	MaxEntries    int   // files and directories
	// MaxRatio bounds uncompressed/compressed size, checked for the whole
	// archive, for each entry larger than ratioMinBytes and against the
	// archive file's real size (declared and actually written bytes).
	MaxRatio   int64
	MaxDepth   int // path segments
	MaxPathLen int // bytes of an entry path
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
func Zip(zipPath, tmpRoot string, lim Limits) (string, error) {
	r, err := zip.OpenReader(zipPath)
	if errors.Is(err, zip.ErrInsecurePath) {
		if r != nil {
			_ = r.Close()
		}
		return "", unsafe("archive contains insecure paths")
	}
	if err != nil {
		return "", fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()
	size, err := checkRecords(zipPath, r.File)
	if err != nil {
		return "", err
	}
	entries, err := plan(r.File, lim, size)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return "", err
	}
	dest, err := os.MkdirTemp(tmpRoot, "static-extract-")
	if err != nil {
		return "", err
	}
	if err := extractAll(entries, dest, lim, ratioBudget(lim, size)); err != nil {
		_ = os.RemoveAll(dest)
		return "", err
	}
	return dest, nil
}

// checkRecords validates the archive's own records against archive/zip's
// view (see layout.go) and returns the archive's size.
func checkRecords(zipPath string, files []*zip.File) (int64, error) {
	f, err := os.Open(zipPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	cd, cdOffset, err := readCentral(f, info.Size())
	if err != nil {
		return 0, err
	}
	if len(cd) != len(files) {
		return 0, unsafe("central directory entry count mismatch")
	}
	for i := range cd {
		if string(cd[i].name) != files[i].Name {
			return 0, unsafe("central directory mismatch at entry %d", i)
		}
	}
	if err := checkLayout(f, cd, cdOffset); err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// extractAll writes the planned entries; budget bounds the bytes actually
// written in total.
func extractAll(entries []entry, dest string, lim Limits, budget int64) error {
	var written int64
	for _, e := range entries {
		target := filepath.Join(dest, filepath.FromSlash(e.path))
		if rel, err := filepath.Rel(dest, target); err != nil || !filepath.IsLocal(rel) {
			return unsafe("entry %q escapes the destination", clip(e.path))
		}
		if e.dir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if e.exec {
			mode = 0o755
		}
		n, err := writeFile(e.f, target, mode, min(lim.MaxFileBytes, budget-written))
		if err != nil {
			return err
		}
		written += n
	}
	return nil
}

// writeFile copies one entry, never trusting its declared size: at most
// limit bytes are accepted.
func writeFile(f *zip.File, target string, mode fs.FileMode, limit int64) (int64, error) {
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
	n, err := io.Copy(dst, io.LimitReader(src, limit+1))
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
