package build

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// repoFS reads a few files of an untrusted checkout. Every access is
// contained to the checkout by os.Root, and every path component is
// checked with Lstat first: symbolic links and non-regular files are
// refused, reads are size-capped. The checkout is never written. Paths are
// clean slash-separated relative paths (validated by the caller).
type repoFS struct {
	root *os.Root
}

func openRepo(dir string) (*repoFS, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &repoFS{root: r}, nil
}

func (r *repoFS) Close() error { return r.root.Close() }

// errMissing marks a path that does not exist.
var errMissing = errors.New("does not exist")

// lstatChain checks every component of rel without following links and
// returns the final component's info. A symbolic link anywhere is a
// *UserError naming the path; a missing component is errMissing.
func (r *repoFS) lstatChain(rel string) (fs.FileInfo, error) {
	if rel == "." || rel == "" {
		return r.root.Lstat(".")
	}
	if !filepath.IsLocal(filepath.FromSlash(rel)) || path.Clean(rel) != rel {
		return nil, userErr("%s is not a path inside the repository", clip(rel, 120))
	}
	var info fs.FileInfo
	segs := strings.Split(rel, "/")
	for i := range segs {
		p := strings.Join(segs[:i+1], "/")
		fi, err := r.root.Lstat(filepath.FromSlash(p))
		if errors.Is(err, fs.ErrNotExist) || isNotDir(err) {
			return nil, errMissing
		}
		if err != nil {
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, userErr("%s is a symbolic link; symbolic links are not followed", clip(p, 120))
		}
		if i < len(segs)-1 && !fi.IsDir() {
			return nil, errMissing
		}
		info = fi
	}
	return info, nil
}

// isNotDir reports ENOTDIR-like errors (a file used as a directory).
func isNotDir(err error) bool {
	var pe *fs.PathError
	return errors.As(err, &pe) && strings.Contains(strings.ToLower(pe.Err.Error()), "not a directory")
}

// readFile reads the regular file rel of at most limit bytes. A missing
// file returns errMissing.
func (r *repoFS) readFile(rel string, limit int64) ([]byte, error) {
	fi, err := r.lstatChain(rel)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, userErr("%s is not a regular file", clip(rel, 120))
	}
	if fi.Size() > limit {
		return nil, userErr("%s is larger than %d KiB", clip(rel, 120), limit>>10)
	}
	f, err := r.root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Check what was opened, not what was looked at.
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || !os.SameFile(st, fi) {
		return nil, userErr("%s changed while it was read", clip(rel, 120))
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, userErr("%s is larger than %d KiB", clip(rel, 120), limit>>10)
	}
	return data, nil
}

// kind reports what rel is: missing, a regular file or a directory
// (symbolic links and other types are *UserError).
type entryKind int

const (
	kindMissing entryKind = iota
	kindFile
	kindDir
)

func (r *repoFS) kind(rel string) (entryKind, error) {
	fi, err := r.lstatChain(rel)
	if errors.Is(err, errMissing) {
		return kindMissing, nil
	}
	if err != nil {
		return kindMissing, err
	}
	switch {
	case fi.Mode().IsRegular():
		return kindFile, nil
	case fi.IsDir():
		return kindDir, nil
	}
	return kindMissing, userErr("%s is neither a regular file nor a directory", clip(rel, 120))
}

// maxDirEntries bounds directory listings.
const maxDirEntries = 4096

// readDir lists the directory rel (at most maxDirEntries entries; names
// only with their types as reported by the directory, links not followed).
func (r *repoFS) readDir(rel string) ([]fs.DirEntry, error) {
	fi, err := r.lstatChain(rel)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, errMissing
	}
	f, err := r.root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(maxDirEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maxDirEntries {
		return nil, userErr("%s has more than %d entries", clip(rel, 120), maxDirEntries)
	}
	return entries, nil
}

// join joins slash paths below base ("." is the root).
func join(base string, elem ...string) string {
	return path.Clean(path.Join(append([]string{base}, elem...)...))
}
