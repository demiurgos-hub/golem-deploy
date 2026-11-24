package backups

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/demiurgos-hub/golem-deploy/internal/logsafe"
	"github.com/demiurgos-hub/golem-deploy/internal/staticfiles"
)

// Store errors.
var (
	// ErrNoSpace: writing would leave less than the free-space reserve.
	ErrNoSpace = errors.New("backups: not enough free disk space")
	// ErrInUse: a running job reads the backup.
	ErrInUse = errors.New("backups: the backup is in use")
	// ErrMissing: the archive file is missing or differs from its record.
	ErrMissing = errors.New("backups: the archive is missing or damaged")
	// ErrFileKept: Delete removed the record but not the file (it is
	// removed by the next Sweep).
	ErrFileKept = errors.New("backups: the archive could not be removed yet")
)

// tempPrefix starts the name of an archive being written. Such files are
// never referenced by a row; Sweep removes them.
const tempPrefix = ".tmp-"

// checkEvery is how often (in bytes written) free space is checked again.
const checkEvery = 64 << 20

var fileRE = regexp.MustCompile(`^[0-9a-f]{32}\.tar\.gz$`)

// ValidFile reports whether name is a valid archive file name.
func ValidFile(name string) bool { return fileRE.MatchString(name) }

// Options configures a Store.
type Options struct {
	// CheckFree fails (with staticfiles.ErrNoSpace or ErrNoSpace) when
	// writing need more bytes to dir would leave less than the reserve
	// free. Default: staticfiles.CheckFreeSpace with
	// staticfiles.DefaultReserveBytes.
	CheckFree func(dir string, need int64) error
}

// Store keeps backup archives in one directory (0700, files 0600). An
// archive is written under a temporary name, synced and renamed to its
// final name before the caller records it, so a recorded backup always
// has its file; Sweep removes what a crash left. It also tracks which
// backups running jobs read, so they are not deleted under them.
type Store struct {
	dir       string
	checkFree func(dir string, need int64) error

	mu      sync.Mutex
	readers map[int64]int
}

// Open creates dir if needed and returns a Store.
func Open(dir string, o Options) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create backup directory: %w", err)
	}
	if o.CheckFree == nil {
		o.CheckFree = func(d string, need int64) error {
			return staticfiles.CheckFreeSpace(d, need, staticfiles.DefaultReserveBytes)
		}
	}
	return &Store{dir: dir, checkFree: o.CheckFree, readers: map[int64]int{}}, nil
}

// Dir returns the backup directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) free(need int64) error {
	if err := s.checkFree(s.dir, need); err != nil {
		if errors.Is(err, staticfiles.ErrNoSpace) || errors.Is(err, ErrNoSpace) {
			return ErrNoSpace
		}
		return err
	}
	return nil
}

// Pending is an archive being written.
type Pending struct {
	s       *Store
	f       *os.File
	tmp     string
	final   string
	sum     hash.Hash
	size    int64
	checked int64
	done    bool
}

// Create starts a new archive. It fails with ErrNoSpace when the free
// space is already below the reserve.
func (s *Store) Create() (*Pending, error) {
	if err := s.free(0); err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	name := hex.EncodeToString(b)
	tmp := filepath.Join(s.dir, tempPrefix+name)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Pending{s: s, f: f, tmp: tmp, final: name + ".tar.gz", sum: sha256.New()}, nil
}

// Write implements io.Writer: it hashes, counts and checks the free space
// every checkEvery bytes.
func (p *Pending) Write(b []byte) (int, error) {
	if p.size+int64(len(b)) > p.checked+checkEvery {
		if err := p.s.free(checkEvery); err != nil {
			return 0, err
		}
		p.checked = p.size
	}
	n, err := p.f.Write(b)
	p.sum.Write(b[:n])
	p.size += int64(n)
	return n, err
}

// Size is the number of bytes written so far.
func (p *Pending) Size() int64 { return p.size }

// Commit syncs the archive, moves it to its final name and returns that
// name with the archive's size and lower-case hex SHA-256. After Commit the
// caller records the backup; until then a crash leaves a file Sweep removes.
func (p *Pending) Commit() (file string, size int64, sum string, err error) {
	if p.done {
		return "", 0, "", errors.New("backups: archive already finished")
	}
	p.done = true
	err = p.f.Sync()
	if cerr := p.f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(p.tmp, filepath.Join(p.s.dir, p.final))
	}
	if err == nil {
		err = syncDir(p.s.dir)
	}
	if err != nil {
		_ = os.Remove(p.tmp)
		_ = os.Remove(filepath.Join(p.s.dir, p.final))
		return "", 0, "", err
	}
	return p.final, p.size, hex.EncodeToString(p.sum.Sum(nil)), nil
}

// Discard abandons the archive (no-op after Commit).
func (p *Pending) Discard() {
	if p.done {
		return
	}
	p.done = true
	_ = p.f.Close()
	_ = os.Remove(p.tmp)
}

func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil // directories cannot be synced there
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) path(file string) (string, error) {
	if !ValidFile(file) {
		return "", ErrMissing
	}
	return filepath.Join(s.dir, file), nil
}

// OpenFile opens a stored archive for reading.
func (s *Store) OpenFile(file string) (*os.File, fs.FileInfo, error) {
	p, err := s.path(file)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, ErrMissing
	}
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, ErrMissing
	}
	return f, info, nil
}

// Verify checks that the archive exists with the recorded size and
// SHA-256 (ErrMissing otherwise).
func (s *Store) Verify(file string, size int64, sum string) error {
	f, info, err := s.OpenFile(file)
	if err != nil {
		return err
	}
	defer f.Close()
	if info.Size() != size {
		return ErrMissing
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != sum {
		return ErrMissing
	}
	return nil
}

// Remove deletes a stored archive (a missing file is not an error).
func (s *Store) Remove(file string) error {
	p, err := s.path(file)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Sweep removes archives being written when the daemon stopped and
// archives no record references (keep holds the referenced names). Call
// it at startup, before any job runs. It returns the sanitised names it
// removed.
func (s *Store) Sweep(keep map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, e := range entries {
		name := e.Name()
		stray := strings.HasPrefix(name, tempPrefix) || (ValidFile(name) && !keep[name])
		if !stray || !e.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, safeName(name))
	}
	return removed, errors.Join(errs...)
}

// Acquire marks backup id as read by a running job until release is
// called; Delete refuses it meanwhile.
func (s *Store) Acquire(id int64) (release func()) {
	s.mu.Lock()
	s.readers[id]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.readers[id]--; s.readers[id] <= 0 {
				delete(s.readers, id)
			}
			s.mu.Unlock()
		})
	}
}

// InUse reports whether a running job reads backup id.
func (s *Store) InUse(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readers[id] > 0
}

// Delete runs deleteRow (which removes the record and returns its file
// name) unless a job reads backup id (ErrInUse), then removes the file.
// A file whose removal fails (ErrFileKept, the backup is deleted all the
// same) stays until the next Sweep.
func (s *Store) Delete(id int64, deleteRow func() (string, error)) error {
	s.mu.Lock()
	if s.readers[id] > 0 {
		s.mu.Unlock()
		return ErrInUse
	}
	file, err := deleteRow()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := s.Remove(file); err != nil {
		return fmt.Errorf("%w: %w", ErrFileKept, err)
	}
	return nil
}

// safeName renders an untrusted name for logs.
func safeName(name string) string {
	s := logsafe.String(name)
	if len(s) <= 200 {
		return s
	}
	cut := 200
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
