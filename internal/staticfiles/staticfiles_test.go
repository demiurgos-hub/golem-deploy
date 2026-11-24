package staticfiles

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestValidateName(t *testing.T) {
	for name, ok := range map[string]bool{
		"addon.zip": true, "A_b-1.tar.gz": true, "x": true, strings.Repeat("a", 128): true, "a..b": true,
		"": false, ".hidden": false, "-x": false, "a/b": false, `a\b`: false, "..": false, "../x": false,
		"a b": false, "trailing.": false, "CON": false, "con.zip": false, "LPT1.txt": false, "nul": false,
		"COM9": false, strings.Repeat("a", 129): false, "a:b": false, "ü.zip": false, "a\x00": false,
	} {
		if err := ValidateName(name); (err == nil) != ok {
			t.Errorf("%q: err=%v, want ok=%v", name, err, ok)
		}
	}
}

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "static"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func put(t *testing.T, s *Store, name, content string, replace bool) error {
	t.Helper()
	st, err := s.Stage(name, strings.NewReader(content), MaxFileBytes)
	if err != nil {
		return err
	}
	defer st.Discard()
	_, err = s.Commit([]*Staged{st}, replace)
	return err
}

func assertNoTemps(t *testing.T, s *Store) {
	t.Helper()
	entries, _ := os.ReadDir(s.Dir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestStageCommitListRemove(t *testing.T) {
	s := openStore(t)
	if err := put(t, s, "addon.zip", "PK\x03\x04rest", false); err != nil {
		t.Fatal(err)
	}
	if err := put(t, s, "notes.txt", "hello", false); err != nil {
		t.Fatal(err)
	}
	if err := put(t, s, "fake.zip", "not a zip", false); err != nil {
		t.Fatal(err)
	}
	if err := put(t, s, "notes.txt", "other", false); !errors.Is(err, ErrExists) {
		t.Fatalf("overwrite without replace: %v", err)
	}
	if err := put(t, s, "notes.txt", "replaced", true); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil || len(list) != 3 {
		t.Fatalf("%+v %v", list, err)
	}
	want := map[string]bool{"addon.zip": true, "fake.zip": false, "notes.txt": false}
	for _, f := range list {
		if f.IsZip != want[f.Name] {
			t.Errorf("%s is_zip %v", f.Name, f.IsZip)
		}
	}
	f, info, err := s.OpenFile("notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "replaced" || info.Size() != 8 {
		t.Fatalf("%q", b)
	}
	if err := s.Remove("notes.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("notes.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, _, err := s.OpenFile("../static/addon.zip"); err == nil {
		t.Fatal("traversal accepted")
	}
	assertNoTemps(t, s)
}

func TestCommitIsAllOrNothing(t *testing.T) {
	s := openStore(t)
	if err := put(t, s, "b.txt", "old", false); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Stage("a.txt", strings.NewReader("a"), 10)
	b, _ := s.Stage("b.txt", strings.NewReader("b"), 10)
	defer a.Discard()
	defer b.Discard()
	if _, err := s.Commit([]*Staged{a, b}, false); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v", err)
	}
	if _, _, err := s.OpenFile("a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatal("first file of a failed commit remained")
	}
}

type failingReader struct{ n int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, errors.New("connection reset")
	}
	k := min(len(p), r.n)
	r.n -= k
	return k, nil
}

func TestInterruptedAndOversizedUploadsLeaveNothing(t *testing.T) {
	s := openStore(t)
	if _, err := s.Stage("big.bin", &failingReader{n: 4096}, MaxFileBytes); err == nil {
		t.Fatal("interrupted upload staged")
	}
	if _, err := s.Stage("big.bin", bytes.NewReader(make([]byte, 101)), 100); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	assertNoTemps(t, s)
	if list, _ := s.List(); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
	// A crash mid-upload leaves a temp file; reopening cleans it.
	if err := os.WriteFile(filepath.Join(s.Dir(), tempPrefix+"123"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	assertNoTemps(t, s2)
}

func TestConcurrentUploadsOfSameName(t *testing.T) {
	s := openStore(t)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = put(t, s, "race.zip", strings.Repeat(string(rune('a'+i)), 1000), false)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrExists):
			t.Fatalf("unexpected error %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d uploads won, want exactly 1", ok)
	}
	f, _, err := s.OpenFile("race.zip")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if len(b) != 1000 || strings.Count(string(b), string(b[0])) != 1000 {
		t.Fatal("file content mixed from several uploads")
	}
	assertNoTemps(t, s)
}

func openWith(t *testing.T, o Options) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "static"), o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// D6: the quota counts bytes actually received (not declared sizes), the
// file count is capped and the free-space reserve is kept.
func TestD6_QuotaCountReserve(t *testing.T) {
	s := openWith(t, Options{QuotaBytes: 10, MaxFiles: 2, ReserveBytes: -1})
	if err := put(t, s, "a", "123456", false); err != nil {
		t.Fatal(err)
	}
	if err := put(t, s, "b", "12345", false); !errors.Is(err, ErrQuota) {
		t.Fatalf("over quota: %v", err)
	}
	// Streaming stops at the quota even when the sender lies about sizes.
	if _, err := s.Stage("c", strings.NewReader(strings.Repeat("x", 11)), MaxFileBytes, s.Budget()); !errors.Is(err, ErrQuota) {
		t.Fatalf("stage over quota: %v", err)
	}
	if err := put(t, s, "a", "1234567890", true); err != nil {
		t.Fatalf("replace within quota: %v", err)
	}
	s2 := openWith(t, Options{MaxFiles: 1, ReserveBytes: -1})
	_ = put(t, s2, "a", "1", false)
	if err := put(t, s2, "b", "1", false); !errors.Is(err, ErrQuota) {
		t.Fatalf("file count: %v", err)
	}
	free := int64(100)
	s3 := openWith(t, Options{ReserveBytes: 50, FreeSpace: func(string) (int64, error) { return free, nil }})
	if err := put(t, s3, "a", "1", false); err != nil {
		t.Fatal(err)
	}
	free = 49
	if err := put(t, s3, "b", "1", false); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("reserve: %v", err)
	}
	if u, _ := s.Usage(); u.UsedBytes != 10 || u.Files != 1 || u.QuotaBytes != 10 {
		t.Fatalf("usage %+v", u)
	}
	assertNoTemps(t, s)
	assertNoTemps(t, s3)
}

// D6: concurrent uploads are capped per key.
func TestD6_UploadSlots(t *testing.T) {
	s := openWith(t, Options{})
	r1, ok1 := s.BeginUpload("1")
	r2, ok2 := s.BeginUpload("1")
	_, ok3 := s.BeginUpload("1")
	r4, ok4 := s.BeginUpload("2")
	if !ok1 || !ok2 || ok3 || !ok4 {
		t.Fatal(ok1, ok2, ok3, ok4)
	}
	r1()
	r1() // idempotent
	if _, ok := s.BeginUpload("1"); !ok {
		t.Fatal("slot not released")
	}
	r2()
	r4()
}

// D13: a commit that fails half-way restores the previous files; replaced
// files are kept as hard links until the end, so a crash never loses one.
func TestD13_CommitRollback(t *testing.T) {
	s := openStore(t)
	if err := put(t, s, "a.txt", "old", false); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.Dir(), "c.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Stage("a.txt", strings.NewReader("new"), 10)
	b, _ := s.Stage("b.txt", strings.NewReader("b"), 10)
	c, _ := s.Stage("c.txt", strings.NewReader("c"), 10)
	defer a.Discard()
	defer b.Discard()
	defer c.Discard()
	if _, err := s.Commit([]*Staged{a, b, c}, true); err == nil {
		t.Fatal("commit over a directory succeeded")
	}
	f, _, err := s.OpenFile("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != "old" {
		t.Fatalf("a.txt = %q", got)
	}
	if _, _, err := s.OpenFile("b.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatal("b.txt left behind")
	}
	a.Discard()
	b.Discard()
	c.Discard()
	assertNoTemps(t, s)
	// A leftover replacement link (crash) is swept at startup and the file
	// it duplicates stays.
	if err := os.Link(filepath.Join(s.Dir(), "a.txt"), filepath.Join(s.Dir(), backupPrefix+"1")); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	assertNoTemps(t, s2)
	f2, _, err := s2.OpenFile("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	f2.Close()
}
