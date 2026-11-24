package backups

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type entry struct {
	name string
	typ  byte
	data string
	link string
	mode int64
}

// dockerTar builds a stream shaped like "docker cp <c>:/data -".
func dockerTar(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(h *tar.Header, data string) {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if data != "" {
			if _, err := tw.Write([]byte(data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(&tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 65532, Gid: 65532}, "")
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: mode, Uid: 65532, Gid: 65532, Linkname: e.link, ModTime: time.Unix(1700000000, 0),
			PAXRecords: map[string]string{"SCHILY.xattr.user.evil": "x"}, Format: tar.FormatPAX}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.data))
		}
		write(h, e.data)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func reg(name, data string) entry { return entry{name: "data/" + name, typ: tar.TypeReg, data: data} }
func dir(name string) entry       { return entry{name: "data/" + name + "/", typ: tar.TypeDir, mode: 0o755} }

// readArchive lists a normalised archive.
func readArchive(t *testing.T, b []byte) map[string]*tar.Header {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]*tar.Header{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = h
	}
}

// snapshotHeader builds a world.snapshot header.
func snapshotHeader(version uint64, fp string) string {
	b := []byte{'G', 'L', 'M', 1}
	b = binary.AppendUvarint(b, version)
	b = binary.AppendUvarint(b, 1732400000)
	b = binary.AppendUvarint(b, uint64(len(fp)))
	return string(append(b, fp...)) + "entity data follows"
}

const fp = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestNormalizeKeepsFilesAndDirsOnly(t *testing.T) {
	in := dockerTar(t,
		reg("world.snapshot", snapshotHeader(2, fp)),
		dir("content"),
		reg("content/a.bin", "aaaa"),
		entry{name: "data/link", typ: tar.TypeSymlink, link: "/etc/passwd"},
		entry{name: "data/hard", typ: tar.TypeLink, link: "data/content/a.bin"},
		entry{name: "data/fifo", typ: tar.TypeFifo},
		entry{name: "data/dev", typ: tar.TypeChar},
		entry{name: "data/blk", typ: tar.TypeBlock},
		entry{name: "data/../escape", typ: tar.TypeReg, data: "x"},
		entry{name: "/abs", typ: tar.TypeReg, data: "x"},
		entry{name: "data/a/../b", typ: tar.TypeReg, data: "x"},
		entry{name: "data/bad\x1b[31mname", typ: tar.TypeReg, data: "x"},
		entry{name: "data/missing/child", typ: tar.TypeReg, data: "x"},
		reg("content/a.bin", "dup"),
		entry{name: "data/setuid", typ: tar.TypeReg, data: "s", mode: 0o4777},
	)
	var out bytes.Buffer
	res, err := Normalize(bytes.NewReader(in), "data", 1<<20, &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || res.Dirs != 1 || res.Fingerprint != fp || res.SkippedCount != 11 {
		t.Fatalf("%+v", res)
	}
	got := readArchive(t, out.Bytes())
	if len(got) != 4 || got["world.snapshot"] == nil || got["content/"] == nil || got["content/a.bin"] == nil || got["setuid"] == nil {
		t.Fatalf("%v", got)
	}
	for name, h := range got {
		if h.Uid != 0 || h.Gid != 0 || len(h.PAXRecords) != 0 && h.PAXRecords["SCHILY.xattr.user.evil"] != "" || h.Mode&^0o755 != 0 {
			t.Errorf("%s: uid %d gid %d mode %o pax %v", name, h.Uid, h.Gid, h.Mode, h.PAXRecords)
		}
	}
	if got["setuid"].Mode != 0o755 || got["content/"].Mode != 0o755 {
		t.Fatalf("modes %o %o", got["setuid"].Mode, got["content/"].Mode)
	}
	reasons := map[string]string{}
	for _, s := range res.Skipped {
		reasons[s.Name] = s.Reason
		if strings.ContainsRune(s.Name, 0x1b) {
			t.Fatalf("unsanitised name %q", s.Name)
		}
	}
	for name, reason := range map[string]string{"link": "symbolic link", "hard": "hard link", "fifo": "named pipe", "dev": "device", "blk": "device",
		"a/../b": "invalid path", "missing/child": "outside a kept directory", "content/a.bin": "duplicate", "/abs": "invalid path"} {
		if reasons[name] != reason {
			t.Errorf("%s: %q (%v)", name, reasons[name], reasons)
		}
	}
}

func TestNormalizeSnapshotLinkHasNoFingerprint(t *testing.T) {
	in := dockerTar(t, entry{name: "data/world.snapshot", typ: tar.TypeSymlink, link: "/proc/self/environ"}, reg("other", "x"))
	res, err := Normalize(bytes.NewReader(in), "data", 1<<20, io.Discard)
	if err != nil || res.Fingerprint != "" || res.Files != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	// A snapshot below the top level is not the world snapshot.
	in = dockerTar(t, dir("sub"), reg("sub/world.snapshot", snapshotHeader(1, fp)))
	if res, _ := Normalize(bytes.NewReader(in), "data", 1<<20, io.Discard); res.Fingerprint != "" {
		t.Fatalf("%+v", res)
	}
}

func TestNormalizeLimits(t *testing.T) {
	in := dockerTar(t, reg("a", strings.Repeat("x", 600)), reg("b", strings.Repeat("y", 600)))
	if _, err := Normalize(bytes.NewReader(in), "data", 1000, io.Discard); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("%v", err)
	}
	if _, err := Normalize(bytes.NewReader(in), "data", 1200, io.Discard); err != nil {
		t.Fatal(err)
	}
	var many []entry
	for i := 0; i < MaxEntries+1; i++ {
		many = append(many, entry{name: "data/l" + strings.Repeat("x", i%7), typ: tar.TypeFifo})
	}
	if _, err := Normalize(bytes.NewReader(dockerTar(t, many...)), "data", 1<<20, io.Discard); !errors.Is(err, ErrTooManyEntries) {
		t.Fatalf("%v", err)
	}
	deep := "data/" + strings.Repeat("d/", MaxDepth) + "f"
	res, err := Normalize(bytes.NewReader(dockerTar(t, entry{name: deep, typ: tar.TypeReg, data: "x"})), "data", 1<<20, io.Discard)
	if err != nil || res.SkippedCount != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := Normalize(strings.NewReader("not a tar stream at all, just text that is long enough to fill a block"), "data", 1<<20, io.Discard); !errors.Is(err, ErrBadStream) {
		t.Fatalf("%v", err)
	}
}

func TestParseSnapshotHeader(t *testing.T) {
	cases := map[string]string{
		snapshotHeader(1, fp):                       fp,
		snapshotHeader(2, fp):                       fp,
		snapshotHeader(3, fp):                       "",
		snapshotHeader(1, ""):                       "",
		snapshotHeader(1, "bad\x00fp"):              "",
		snapshotHeader(1, strings.Repeat("a", 129)): "",
		"GLM\x02" + snapshotHeader(1, fp)[4:]:       "",
		snapshotHeader(1, fp)[:20]:                  "",
		"":                                          "",
		"GLM\x01\x80\x00\x00":                       "", // non-canonical uvarint
	}
	for in, want := range cases {
		if got := ParseSnapshotHeader([]byte(in)); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestRewriteRoundTripAndDamage(t *testing.T) {
	in := dockerTar(t, reg("world.snapshot", snapshotHeader(1, fp)), dir("c"), reg("c/x", "hello"))
	var archive bytes.Buffer
	res, err := Normalize(bytes.NewReader(in), "data", 1<<20, &archive)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Rewrite(bytes.NewReader(archive.Bytes()), 65532, 65532, res.Files, res.ContentBytes, &out); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&out)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
		if h.Uid != 65532 || h.Gid != 65532 || strings.HasPrefix(h.Name, "data/") {
			t.Fatalf("%+v", h)
		}
	}
	if n != 3 {
		t.Fatalf("%d entries", n)
	}
	if err := Rewrite(bytes.NewReader(archive.Bytes()), 1, 1, res.Files+1, res.ContentBytes, io.Discard); !errors.Is(err, ErrDamaged) {
		t.Fatalf("%v", err)
	}
	if err := Rewrite(strings.NewReader("garbage"), 1, 1, 0, 0, io.Discard); !errors.Is(err, ErrDamaged) {
		t.Fatalf("%v", err)
	}
	// A hand-made archive with a link is refused.
	var evil bytes.Buffer
	gz := gzip.NewWriter(&evil)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/"})
	_ = tw.Close()
	_ = gz.Close()
	if err := Rewrite(&evil, 1, 1, 0, 0, io.Discard); !errors.Is(err, ErrDamaged) {
		t.Fatalf("%v", err)
	}
}

func TestStoreCommitSweepVerifyDelete(t *testing.T) {
	dir := t.TempDir()
	free := int64(10 << 30)
	s, err := Open(filepath.Join(dir, "backups"), Options{CheckFree: func(_ string, need int64) error {
		if free-need < 1<<30 {
			return ErrNoSpace
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Write([]byte("archive")); err != nil {
		t.Fatal(err)
	}
	file, size, sum, err := p.Commit()
	if err != nil || size != 7 || !ValidFile(file) || len(sum) != 64 {
		t.Fatalf("%s %d %s %v", file, size, sum, err)
	}
	if err := s.Verify(file, 7, sum); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(file, 7, strings.Repeat("0", 64)); !errors.Is(err, ErrMissing) {
		t.Fatalf("%v", err)
	}
	// A crash leaves a temporary file and an unrecorded archive.
	p2, _ := s.Create()
	_, _ = p2.Write([]byte("partial"))
	_ = p2.f.Close() // the process ended
	stray, _ := s.Create()
	strayFile, _, _, _ := stray.Commit()
	unknown := filepath.Join(s.Dir(), "notes.txt")
	_ = os.WriteFile(unknown, []byte("x"), 0o600)
	removed, err := s.Sweep(map[string]bool{file: true})
	if err != nil || len(removed) != 2 {
		t.Fatalf("%v %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), strayFile)); !os.IsNotExist(err) {
		t.Fatal("stray archive kept")
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), file)); err != nil {
		t.Fatal("recorded archive removed")
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatal("unrelated file removed")
	}
	// In use: refused; released: deleted.
	release := s.Acquire(5)
	called := false
	if err := s.Delete(5, func() (string, error) { called = true; return file, nil }); !errors.Is(err, ErrInUse) || called {
		t.Fatalf("%v %v", err, called)
	}
	release()
	release()
	if err := s.Delete(5, func() (string, error) { return file, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), file)); !os.IsNotExist(err) {
		t.Fatal("archive kept")
	}
	// Free space below the reserve.
	free = 1<<30 - 1
	if p, err := s.Create(); !errors.Is(err, ErrNoSpace) {
		p.Discard()
		t.Fatalf("%v", err)
	}
	free = 1<<30 + checkEvery + 10
	p3, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	defer p3.Discard()
	free = 1 << 30
	if _, err := p3.Write(make([]byte, checkEvery+1)); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("%v", err)
	}
	if _, _, err := s.OpenFile("../escape.tar.gz"); !errors.Is(err, ErrMissing) {
		t.Fatalf("%v", err)
	}
}

// countingReader counts the bytes read from r.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// rawBlock is a ustar header block written by hand (the tar writer refuses
// to write some of the records a hostile stream can hold).
func rawBlock(name string, typ byte, size int) []byte {
	b := make([]byte, 512)
	copy(b[0:100], name)
	copy(b[100:108], "0000644\x00")
	copy(b[108:116], "0000000\x00")
	copy(b[116:124], "0000000\x00")
	copy(b[124:136], []byte(strings.Repeat("0", 11-len(strconvOct(size)))+strconvOct(size)+"\x00"))
	copy(b[136:148], "00000000000\x00")
	b[156] = typ
	copy(b[257:265], "ustar\x0000")
	copy(b[148:156], "        ")
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	chk := strconvOct(sum)
	copy(b[148:156], strings.Repeat("0", 6-len(chk))+chk+"\x00 ")
	return b
}

func strconvOct(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 8 {
		s = string(rune('0'+n%8)) + s
	}
	return s
}

func padded(b []byte) []byte {
	if r := len(b) % 512; r != 0 {
		b = append(b, make([]byte, 512-r)...)
	}
	return b
}

// One input budget: the content of a skipped entry counts and is refused
// before it is read, the PAX records before entries count towards the
// entry limit, and a stream of records that never reaches an entry stops
// once it exceeds the byte budget.
func TestNormalizeInputBudget(t *testing.T) {
	// A regular file with a name that is not allowed is still content.
	big := strings.Repeat("z", 5000)
	in := dockerTar(t, reg("a", "x"), entry{name: "data/../big", typ: tar.TypeReg, data: big})
	cr := &countingReader{r: bytes.NewReader(in)}
	if _, err := Normalize(cr, "data", 1000, io.Discard); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("%v", err)
	}
	if cr.n >= int64(len(in))-1000 {
		t.Fatalf("read %d of %d bytes: the oversized entry was drained", cr.n, len(in))
	}
	// An oversized header of a type without content is refused as well.
	var hb bytes.Buffer
	hb.Write(rawBlock("data/", tar.TypeDir, 0))
	hb.Write(rawBlock("data/fifo", tar.TypeFifo, 1<<30))
	hb.Write(make([]byte, 1024))
	if _, err := Normalize(bytes.NewReader(hb.Bytes()), "data", 1000, io.Discard); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("%v", err)
	}
	// Every entry here carries a PAX record: two headers each.
	var half []entry
	for i := 0; i < MaxEntries/2+1; i++ {
		half = append(half, entry{name: "data/p" + strings.Repeat("x", i%5), typ: tar.TypeFifo})
	}
	if _, err := Normalize(bytes.NewReader(dockerTar(t, half...)), "data", 1<<20, io.Discard); !errors.Is(err, ErrTooManyEntries) {
		t.Fatalf("%v", err)
	}
	// Records that pile up before any entry: stopped by the byte budget,
	// without reading the rest of the stream.
	var chain bytes.Buffer
	chain.Write(rawBlock("data/", tar.TypeDir, 0))
	rec := "4105 comment=" + strings.Repeat("c", 4091) + "\n"
	for i := 0; i < 200; i++ {
		chain.Write(rawBlock("PaxHeaders/x", tar.TypeXHeader, len(rec)))
		chain.Write(padded([]byte(rec)))
	}
	chain.Write(rawBlock("data/f", tar.TypeReg, 0))
	chain.Write(make([]byte, 1024))
	cr = &countingReader{r: bytes.NewReader(chain.Bytes())}
	if _, err := Normalize(cr, "data", 1000, io.Discard); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("%v", err)
	}
	if limit := int64(1000 + 3*EntryAllowance); cr.n > limit {
		t.Fatalf("read %d bytes, budget %d", cr.n, limit)
	}
}

// Names of skipped entries are redacted before they are sanitised.
func TestNormalizeRedactsSkippedNames(t *testing.T) {
	in := dockerTar(t, reg("a", "x"), entry{name: "data/sec-ret-value\x1b", typ: tar.TypeSymlink, link: "/"})
	res, err := NormalizeRedacted(bytes.NewReader(in), "data", 1<<20, func(s string) string { return strings.ReplaceAll(s, "sec-ret-value", "[redacted]") }, io.Discard)
	if err != nil || len(res.Skipped) != 1 || res.Skipped[0].Name != `[redacted]\x1b` {
		t.Fatalf("%+v %v", res, err)
	}
}

// Archives are opened once, without following links: a link or a
// directory under an archive's name is missing, and the verified open
// file is read from its start.
func TestOpenVerifiedDoesNotFollowLinks(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "backups"), Options{CheckFree: func(string, int64) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.Create()
	_, _ = p.Write([]byte("archive bytes"))
	file, size, sum, err := p.Commit()
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.OpenVerified(file, size, sum)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != "archive bytes" {
		t.Fatalf("%q", got)
	}
	if _, err := s.OpenVerified(file, size+1, sum); !errors.Is(err, ErrMissing) {
		t.Fatalf("%v", err)
	}
	dirName := strings.Repeat("d", 32) + ".tar.gz"
	if err := os.Mkdir(filepath.Join(s.Dir(), dirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.OpenFile(dirName); !errors.Is(err, ErrMissing) {
		t.Fatalf("directory: %v", err)
	}
	link := strings.Repeat("e", 32) + ".tar.gz"
	if err := os.Symlink(filepath.Join(s.Dir(), file), filepath.Join(s.Dir(), link)); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	if _, _, err := s.OpenFile(link); !errors.Is(err, ErrMissing) {
		t.Fatalf("link: %v", err)
	}
	if err := s.Verify(link, size, sum); !errors.Is(err, ErrMissing) {
		t.Fatalf("link: %v", err)
	}
}
