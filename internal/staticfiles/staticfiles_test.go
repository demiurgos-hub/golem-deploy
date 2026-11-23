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
