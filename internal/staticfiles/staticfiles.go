// Package staticfiles is the static file store (datadir static/): files
// uploaded by developers for later builds, such as ZIP add-ons. Uploads are
// streamed to a temporary file in the same directory and moved into place
// atomically; existing files are never overwritten unless asked.
package staticfiles

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Limits.
const (
	// MaxFileBytes is the per-file upload limit.
	MaxFileBytes = 1 << 30
	// MaxFilesPerUpload bounds the files of one upload request.
	MaxFilesPerUpload = 32
	tempPrefix        = ".upload-"
)

// Errors.
var (
	ErrNotFound = errors.New("staticfiles: not found")
	ErrExists   = errors.New("staticfiles: file exists")
	ErrTooLarge = errors.New("staticfiles: file too large")
)

// NameError is an invalid file name; it is safe to show to users.
type NameError struct{ Msg string }

func (e *NameError) Error() string { return e.Msg }

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// windowsReserved reports whether name is a Windows device name (with or
// without an extension), e.g. CON, nul.txt, COM1.zip.
func windowsReserved(name string) bool {
	base, _, _ := strings.Cut(strings.ToUpper(name), ".")
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9' {
		return true
	}
	return false
}

// ValidateName checks a static file name: ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$,
// not ending with a dot and not a Windows reserved device name.
func ValidateName(name string) error {
	switch {
	case !nameRE.MatchString(name):
		return &NameError{Msg: "file names must be 1-128 characters of A-Z, a-z, 0-9, '.', '_' and '-', starting with a letter or digit"}
	case strings.HasSuffix(name, "."):
		return &NameError{Msg: "file names must not end with a dot"}
	case windowsReserved(name):
		return &NameError{Msg: "file name is a reserved device name"}
	case !filepath.IsLocal(name):
		return &NameError{Msg: "invalid file name"}
	}
	return nil
}

// File describes a stored file.
type File struct {
	Name       string
	SizeBytes  int64
	ModifiedAt time.Time
	IsZip      bool
}

// Store is the static file store rooted at one directory.
type Store struct {
	dir string
}

// Open returns the store at dir (created 0700 if missing) and removes
// temporary files left by interrupted uploads.
func Open(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			_ = os.Remove(filepath.Join(abs, e.Name()))
		}
	}
	return &Store{dir: abs}, nil
}

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

// path validates name and returns its absolute path inside the store.
func (s *Store) path(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	p := filepath.Join(s.dir, name)
	rel, err := filepath.Rel(s.dir, p)
	if err != nil || rel != name {
		return "", &NameError{Msg: "invalid file name"}
	}
	return p, nil
}

func isZip(path, name string) bool {
	if !strings.EqualFold(filepath.Ext(name), ".zip") {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sig := make([]byte, 4)
	if _, err := io.ReadFull(f, sig); err != nil {
		return false
	}
	return bytes.Equal(sig, []byte("PK\x03\x04")) || bytes.Equal(sig, []byte("PK\x05\x06"))
}

func (s *Store) describe(name string, info fs.FileInfo) File {
	return File{Name: name, SizeBytes: info.Size(), ModifiedAt: info.ModTime().UTC(), IsZip: isZip(filepath.Join(s.dir, name), name)}
}

// List returns the stored regular files with valid names, sorted by name.
func (s *Store) List() ([]File, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := []File{}
	for _, e := range entries {
		if !e.Type().IsRegular() || ValidateName(e.Name()) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed meanwhile
		}
		out = append(out, s.describe(e.Name(), info))
	}
	slices.SortFunc(out, func(a, b File) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// OpenFile opens a stored regular file for reading.
func (s *Store) OpenFile(name string) (*os.File, fs.FileInfo, error) {
	p, err := s.path(name)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	return f, info, nil
}

// Remove deletes a stored file.
func (s *Store) Remove(name string) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// Staged is an upload written to a temporary file, not yet visible.
type Staged struct {
	Name string
	tmp  string
	size int64
}

// Stage streams r into a temporary file in the store directory. More than
// max bytes yields ErrTooLarge. The caller must Commit or Discard.
func (s *Store) Stage(name string, r io.Reader, max int64) (*Staged, error) {
	if _, err := s.path(name); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(s.dir, tempPrefix+"*")
	if err != nil {
		return nil, err
	}
	st := &Staged{Name: name, tmp: f.Name()}
	n, err := io.Copy(f, io.LimitReader(r, max+1))
	if err == nil && n > max {
		err = ErrTooLarge
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(st.tmp)
		return nil, err
	}
	st.size = n
	return st, nil
}

// Discard removes a staged upload (safe after Commit).
func (st *Staged) Discard() {
	if st != nil && st.tmp != "" {
		_ = os.Remove(st.tmp)
	}
}

// Commit moves staged uploads into place. Without replace, an existing
// file of the same name (checked atomically with a hard link) fails the
// whole commit with ErrExists and files committed by this call are removed
// again. With replace, files are atomically renamed over existing ones.
func (s *Store) Commit(staged []*Staged, replace bool) ([]File, error) {
	var placed []string
	rollback := func() {
		if !replace {
			for _, p := range placed {
				_ = os.Remove(p)
			}
		}
	}
	for _, st := range staged {
		final, err := s.path(st.Name)
		if err != nil {
			rollback()
			return nil, err
		}
		if replace {
			err = os.Rename(st.tmp, final)
		} else {
			err = os.Link(st.tmp, final)
			if err == nil {
				_ = os.Remove(st.tmp)
			}
		}
		if err != nil {
			rollback()
			if errors.Is(err, fs.ErrExist) {
				return nil, fmt.Errorf("%w: %s", ErrExists, st.Name)
			}
			return nil, err
		}
		st.tmp = ""
		placed = append(placed, final)
	}
	out := make([]File, 0, len(staged))
	for i, st := range staged {
		info, err := os.Lstat(placed[i])
		if err != nil {
			return nil, err
		}
		out = append(out, s.describe(st.Name, info))
	}
	return out, nil
}
