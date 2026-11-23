package extract

import (
	"archive/zip"
	"bytes"
	"errors"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type zentry struct {
	name    string
	mode    fs.FileMode
	content []byte
}

func writeZip(t *testing.T, entries ...zentry) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "a.zip")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("left behind in %s: %v", dir, entries)
	}
}

func TestZipExtractsWithMaskedModes(t *testing.T) {
	zp := writeZip(t,
		zentry{name: "Server.x86_64", mode: 0o4755 | fs.ModeSetuid, content: []byte("bin")},
		zentry{name: "data/", mode: fs.ModeDir | 0o777},
		zentry{name: "data/config", mode: 0o600 | fs.ModeSticky, content: []byte("cfg")},
		zentry{name: "data/world.bin", mode: 0o666, content: bytes.Repeat([]byte{1}, 1000)},
	)
	root := t.TempDir()
	dest, err := Zip(zp, root, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dest) != root || !strings.HasPrefix(filepath.Base(dest), "static-extract-") {
		t.Fatalf("destination %s is not a fresh staging dir under %s", dest, root)
	}
	b, err := os.ReadFile(filepath.Join(dest, "data", "config"))
	if err != nil || string(b) != "cfg" {
		t.Fatalf("%q %v", b, err)
	}
	if runtime.GOOS != "windows" {
		for name, want := range map[string]fs.FileMode{"Server.x86_64": 0o755, "data/config": 0o644, "data/world.bin": 0o644} {
			info, err := os.Stat(filepath.Join(dest, filepath.FromSlash(name)))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 || info.Mode().Perm()&^want != 0 {
				t.Errorf("%s mode %v, want at most %v", name, info.Mode(), want)
			}
		}
	}
}

// Ported from the legacy suite: zip-slip must not write outside.
func TestZipRejectsZipSlip(t *testing.T) {
	zp := writeZip(t, zentry{name: "../evil.txt", content: []byte("nope")})
	root := filepath.Join(t.TempDir(), "tmp")
	if _, err := Zip(zp, root, DefaultLimits()); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil.txt")); !os.IsNotExist(err) {
		t.Fatal("zip-slip file written outside the destination")
	}
	assertEmpty(t, root)
}

func TestZipRejectsUnsafeNames(t *testing.T) {
	names := []string{
		"/etc/passwd", "../x", "a/../../x", "a/../b", `..\evil`, `dir\file`, "C:/x", "c:x",
		"a/./b", "a//b", "./a", "CON", "a/nul.txt", "x.", "a /b", "trailing ", "a\x01b",
		strings.Repeat("d/", 40) + "f", strings.Repeat("n", 600),
	}
	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			zp := writeZip(t, zentry{name: n, content: []byte("x")})
			root := t.TempDir()
			if _, err := Zip(zp, root, DefaultLimits()); !errors.Is(err, ErrUnsafe) {
				t.Fatalf("%q: got %v", n, err)
			}
			assertEmpty(t, root)
		})
	}
}

func TestZipRejectsLinksAndCollisions(t *testing.T) {
	cases := map[string][]zentry{
		"symlink":        {{name: "link", mode: fs.ModeSymlink | 0o777, content: []byte("/etc/passwd")}},
		"device":         {{name: "dev", mode: fs.ModeDevice | 0o600}},
		"pipe":           {{name: "fifo", mode: fs.ModeNamedPipe | 0o600}},
		"duplicate":      {{name: "a.txt"}, {name: "a.txt"}},
		"case collision": {{name: "Readme.md"}, {name: "README.md"}},
		"file and dir":   {{name: "a"}, {name: "a/b"}},
		"dir dup case":   {{name: "Data/", mode: fs.ModeDir | 0o755}, {name: "data/", mode: fs.ModeDir | 0o755}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			zp := writeZip(t, entries...)
			root := t.TempDir()
			if _, err := Zip(zp, root, DefaultLimits()); !errors.Is(err, ErrUnsafe) {
				t.Fatalf("got %v", err)
			}
			assertEmpty(t, root)
		})
	}
}

func TestZipBombs(t *testing.T) {
	lim := DefaultLimits()
	t.Run("many entries", func(t *testing.T) {
		var es []zentry
		for i := 0; i < 60; i++ {
			es = append(es, zentry{name: "f" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26))})
		}
		l := lim
		l.MaxEntries = 50
		root := t.TempDir()
		if _, err := Zip(writeZip(t, es...), root, l); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("got %v", err)
		}
		assertEmpty(t, root)
	})
	t.Run("ratio", func(t *testing.T) {
		zp := writeZip(t, zentry{name: "zeros", content: make([]byte, 16<<20)})
		root := t.TempDir()
		if _, err := Zip(zp, root, lim); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("got %v", err)
		}
		assertEmpty(t, root)
	})
	t.Run("total size", func(t *testing.T) {
		l := lim
		l.MaxTotalBytes = 1500
		zp := writeZip(t, zentry{name: "a", content: make([]byte, 1000)}, zentry{name: "b", content: make([]byte, 1000)})
		if _, err := Zip(zp, t.TempDir(), l); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("file size", func(t *testing.T) {
		l := lim
		l.MaxFileBytes = 100
		zp := writeZip(t, zentry{name: "a", content: make([]byte, 101)})
		if _, err := Zip(zp, t.TempDir(), l); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("nested depth", func(t *testing.T) {
		l := lim
		l.MaxDepth = 3
		zp := writeZip(t, zentry{name: "a/b/c/d", content: []byte("x")})
		if _, err := Zip(zp, t.TempDir(), l); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("lying sizes", func(t *testing.T) {
		// A stored entry that declares 10 bytes but carries 5000.
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		data := bytes.Repeat([]byte("A"), 5000)
		w, err := zw.CreateRaw(&zip.FileHeader{Name: "liar", Method: zip.Store, CRC32: crc32.ChecksumIEEE(data), CompressedSize64: 5000, UncompressedSize64: 10})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
		_ = zw.Close()
		zp := filepath.Join(t.TempDir(), "liar.zip")
		_ = os.WriteFile(zp, buf.Bytes(), 0o600)
		root := t.TempDir()
		if _, err := Zip(zp, root, lim); err == nil {
			t.Fatal("lying entry extracted")
		}
		assertEmpty(t, root)
	})
}

func TestZipNotAZip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.zip")
	_ = os.WriteFile(p, []byte("not a zip"), 0o600)
	root := t.TempDir()
	if _, err := Zip(p, root, DefaultLimits()); err == nil {
		t.Fatal("accepted")
	}
	assertEmpty(t, root)
}
