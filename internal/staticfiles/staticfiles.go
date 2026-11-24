// Package staticfiles is the static file store (datadir static/): files
// uploaded by developers for later builds, such as ZIP add-ons. Uploads are
// streamed to temporary files in the same directory (Stage), then checked
// and moved into place together (Commit): every file of one upload becomes
// visible, or none does. The store enforces a total size quota, a file
// count cap and a free-space reserve on the data directory's file system,
// always against the bytes actually received.
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
	"sync"
	"time"
)

// Limits.
const (
	// MaxFileBytes is the per-file upload limit.
	MaxFileBytes = 1 << 30
	// MaxFilesPerUpload bounds the files of one upload request.
	MaxFilesPerUpload = 32
	// DefaultMaxFiles caps the number of stored files.
	DefaultMaxFiles = 2000
	// DefaultReserveBytes is the free space uploads must leave on the
	// file system that holds the store.
	DefaultReserveBytes = 1 << 30
	// DefaultUploadsPerKey caps concurrent uploads per account.
	DefaultUploadsPerKey = 2
	tempPrefix           = ".upload-"
	backupPrefix         = ".upload-replaced-"
)

// Errors.
var (
	ErrNotFound = errors.New("staticfiles: not found")
	ErrExists   = errors.New("staticfiles: file exists")
	ErrTooLarge = errors.New("staticfiles: file too large")
	// ErrQuota: the upload would exceed the store's size quota or file
	// count cap.
	ErrQuota = errors.New("staticfiles: quota exceeded")
	// ErrNoSpace: the upload would leave less than the reserve free on the
	// data directory's file system.
	ErrNoSpace = errors.New("staticfiles: not enough free disk space")
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

// Usage is the store's current consumption and limits.
type Usage struct {
	UsedBytes  int64
	QuotaBytes int64 // 0: unlimited
	Files      int
	MaxFiles   int
}

// Options configures Open.
type Options struct {
	// QuotaBytes caps the total size of stored files (0: unlimited).
	QuotaBytes int64
	// MaxFiles caps the number of stored files (0: DefaultMaxFiles).
	MaxFiles int
	// ReserveBytes is the free space to keep on the file system (0:
	// DefaultReserveBytes; negative: no check).
	ReserveBytes int64
	// UploadsPerKey caps concurrent uploads per key, e.g. per account
	// (0: DefaultUploadsPerKey).
	UploadsPerKey int
	// FreeSpace reports the bytes available to the daemon on the file
	// system holding dir, or a negative value when unknown (tests; default
	// the operating system's answer).
	FreeSpace func(dir string) (int64, error)
}

// Store is the static file store rooted at one directory.
type Store struct {
	dir string
	opt Options

	// commitMu serialises commits and removals, so quota checks are exact
	// and a commit's rollback only ever touches files that commit placed.
	commitMu sync.Mutex

	uploadsMu sync.Mutex
	uploads   map[string]int
}

// Open returns the store at dir (created 0700 if missing) and removes
// temporary files left by interrupted uploads.
func Open(dir string, opts ...Options) (*Store, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = DefaultMaxFiles
	}
	if o.ReserveBytes == 0 {
		o.ReserveBytes = DefaultReserveBytes
	}
	if o.UploadsPerKey <= 0 {
		o.UploadsPerKey = DefaultUploadsPerKey
	}
	if o.FreeSpace == nil {
		o.FreeSpace = freeSpace
	}
	if o.QuotaBytes < 0 {
		return nil, errors.New("static quota must not be negative")
	}
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
		// Backups are extra hard links of files that are still in place
		// (see Commit), so removing them never loses data.
		if strings.HasPrefix(e.Name(), tempPrefix) {
			_ = os.Remove(filepath.Join(abs, e.Name()))
		}
	}
	return &Store{dir: abs, opt: o, uploads: map[string]int{}}, nil
}

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

// BeginUpload reserves one of key's concurrent upload slots; ok is false
// when key already has the maximum in progress. release must be called
// when the upload ends.
func (s *Store) BeginUpload(key string) (release func(), ok bool) {
	s.uploadsMu.Lock()
	defer s.uploadsMu.Unlock()
	if s.uploads[key] >= s.opt.UploadsPerKey {
		return nil, false
	}
	s.uploads[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.uploadsMu.Lock()
			defer s.uploadsMu.Unlock()
			if s.uploads[key]--; s.uploads[key] <= 0 {
				delete(s.uploads, key)
			}
		})
	}, true
}

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

// sizes returns the size of every stored regular file with a valid name.
func (s *Store) sizes() (map[string]int64, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() || ValidateName(e.Name()) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed meanwhile
		}
		out[e.Name()] = info.Size()
	}
	return out, nil
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

// Usage reports the bytes and files stored and the limits.
func (s *Store) Usage() (Usage, error) {
	sizes, err := s.sizes()
	if err != nil {
		return Usage{}, err
	}
	u := Usage{QuotaBytes: s.opt.QuotaBytes, Files: len(sizes), MaxFiles: s.opt.MaxFiles}
	for _, n := range sizes {
		u.UsedBytes += n
	}
	return u, nil
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
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
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

// checkFree fails with ErrNoSpace when fewer than need+reserve bytes are
// free (when the free space is known and the reserve enabled).
func (s *Store) checkFree(need int64) error {
	if s.opt.ReserveBytes < 0 {
		return nil
	}
	free, err := s.opt.FreeSpace(s.dir)
	if err != nil || free < 0 {
		return nil // unknown: the quota still bounds the store
	}
	if free-need < s.opt.ReserveBytes {
		return ErrNoSpace
	}
	return nil
}

// Budget returns how many bytes one upload may stage in total before it
// is certain to exceed the quota: the whole quota (an upload may replace
// every stored file), or -1 when unlimited. It only cuts streaming short;
// Commit checks the exact figure against what is stored.
func (s *Store) Budget() int64 {
	if s.opt.QuotaBytes == 0 {
		return -1
	}
	return s.opt.QuotaBytes
}

// Staged is an upload written to a temporary file, not yet visible.
type Staged struct {
	Name string
	tmp  string
	size int64
}

// Size is the number of bytes received.
func (st *Staged) Size() int64 { return st.size }

// spaceCheckEvery is how often (in bytes) staging re-checks free space.
const spaceCheckEvery = 64 << 20

// Stage streams r into a temporary file in the store directory. More than
// max bytes yields ErrTooLarge, more than limit (the remaining quota
// budget, or -1 for none) ErrQuota, and running below the free-space
// reserve ErrNoSpace. The caller must Commit or Discard.
func (s *Store) Stage(name string, r io.Reader, maxBytes int64, limit ...int64) (*Staged, error) {
	if _, err := s.path(name); err != nil {
		return nil, err
	}
	if err := s.checkFree(0); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(s.dir, tempPrefix+"*")
	if err != nil {
		return nil, err
	}
	st := &Staged{Name: name, tmp: f.Name()}
	quota := int64(-1)
	if len(limit) > 0 {
		quota = limit[0]
	}
	var n int64
	buf := make([]byte, 256<<10)
	for err == nil {
		var k int
		k, err = r.Read(buf)
		if k > 0 {
			if n+int64(k) > maxBytes {
				err = ErrTooLarge
				break
			}
			if quota >= 0 && n+int64(k) > quota {
				err = ErrQuota
				break
			}
			if _, werr := f.Write(buf[:k]); werr != nil {
				err = werr
				break
			}
			before := n
			n += int64(k)
			if before/spaceCheckEvery != n/spaceCheckEvery {
				if cerr := s.checkFree(0); cerr != nil {
					err = cerr
					break
				}
			}
		}
	}
	if errors.Is(err, io.EOF) {
		err = nil
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

// Commit makes every staged upload of one request visible, or none.
//
// Under the store's commit lock it first validates everything: names,
// conflicts (an existing name fails the whole commit with ErrExists unless
// replace), the file count cap and the size quota (ErrQuota, computed from
// the bytes actually received, minus the files being replaced) and the
// free-space reserve (ErrNoSpace). Nothing has changed when any check
// fails. It then keeps a hard link of every file about to be replaced and
// moves the uploads into place (rename over an existing file; hard link
// for a new name, which also refuses a file created behind the store's
// back). If a step fails midway, the files already placed are removed and
// replaced files are restored from their links, so the store is as before;
// only if that restoration itself fails can part of the upload remain, and
// the error then says so. Commits are serialised, so a rollback never
// touches a file placed by another request.
func (s *Store) Commit(staged []*Staged, replace bool) ([]File, error) {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()

	finals := make([]string, len(staged))
	seen := map[string]bool{}
	var received int64
	for i, st := range staged {
		p, err := s.path(st.Name)
		if err != nil {
			return nil, err
		}
		if seen[st.Name] {
			return nil, &NameError{Msg: "the same file name appears twice in the upload"}
		}
		seen[st.Name] = true
		finals[i] = p
		received += st.size
	}
	current, err := s.sizes()
	if err != nil {
		return nil, err
	}
	var used, replaced int64
	for _, n := range current {
		used += n
	}
	files := len(current)
	for _, st := range staged {
		old, exists := current[st.Name]
		switch {
		case exists && !replace:
			return nil, fmt.Errorf("%w: %s", ErrExists, st.Name)
		case exists:
			replaced += old
		default:
			files++
		}
	}
	if files > s.opt.MaxFiles {
		return nil, fmt.Errorf("%w: more than %d files", ErrQuota, s.opt.MaxFiles)
	}
	if s.opt.QuotaBytes > 0 && used-replaced+received > s.opt.QuotaBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrQuota, s.opt.QuotaBytes)
	}
	// The uploads are already on disk; replacing frees the old bytes only
	// at the end, so require the reserve now.
	if err := s.checkFree(0); err != nil {
		return nil, err
	}

	// Keep a link to every file that will be replaced.
	backups := make([]string, len(staged))
	cleanupBackups := func() {
		for _, b := range backups {
			if b != "" {
				_ = os.Remove(b)
			}
		}
	}
	for i, st := range staged {
		if _, exists := current[st.Name]; !exists {
			continue
		}
		b, err := s.backupPath()
		if err == nil {
			err = os.Link(finals[i], b)
		}
		if err != nil {
			cleanupBackups()
			return nil, fmt.Errorf("prepare replacing %s: %w", st.Name, err)
		}
		backups[i] = b
	}

	placed := 0
	var commitErr error
	for i, st := range staged {
		if backups[i] != "" {
			commitErr = os.Rename(st.tmp, finals[i])
		} else {
			commitErr = os.Link(st.tmp, finals[i])
			if commitErr == nil {
				_ = os.Remove(st.tmp)
			}
		}
		if commitErr != nil {
			break
		}
		st.tmp = ""
		placed++
	}
	if commitErr != nil {
		var rbErr []error
		for i := 0; i < placed; i++ {
			var err error
			if backups[i] != "" {
				err = os.Rename(backups[i], finals[i])
				if err == nil {
					backups[i] = ""
				}
			} else {
				err = os.Remove(finals[i])
			}
			if err != nil {
				rbErr = append(rbErr, err)
			}
		}
		cleanupBackups()
		if len(rbErr) > 0 {
			return nil, fmt.Errorf("commit upload: %w; restoring the previous files failed, part of the upload may be visible: %w", commitErr, errors.Join(rbErr...))
		}
		if errors.Is(commitErr, fs.ErrExist) {
			return nil, fmt.Errorf("%w: %s", ErrExists, staged[placed].Name)
		}
		return nil, fmt.Errorf("commit upload: %w", commitErr)
	}
	cleanupBackups()
	out := make([]File, 0, len(staged))
	for i, st := range staged {
		info, err := os.Lstat(finals[i])
		if err != nil {
			return nil, err
		}
		out = append(out, s.describe(st.Name, info))
	}
	return out, nil
}

// backupPath returns an unused path for a replacement backup link.
func (s *Store) backupPath() (string, error) {
	f, err := os.CreateTemp(s.dir, backupPrefix+"*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}
