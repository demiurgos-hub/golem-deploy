package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScan(t *testing.T) {
	content := []byte("ok\tline\r\n" +
		"x := \"\u202e\" // hidden\n" +
		"a\u2028b\x00c\x1b[31m\n" +
		"\u200f\u2066\u061c ok é ☃\n" +
		"\xff\xfe invalid utf-8 is not this check's business\n")
	got := Scan("f.go", content)
	var want = []string{
		"f.go:2:7: U+202E RIGHT-TO-LEFT OVERRIDE",
		"f.go:3:2: U+2028 LINE SEPARATOR",
		"f.go:3:4: U+0000 NUL",
		"f.go:3:6: U+001B ESCAPE",
		"f.go:4:1: U+200F RIGHT-TO-LEFT MARK",
		"f.go:4:2: U+2066 LEFT-TO-RIGHT ISOLATE",
		"f.go:4:3: U+061C ARABIC LETTER MARK",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("finding %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRunReportsAndSkips(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	clean := write("clean.ts", "const s = \"\\u202e\"; // escaped is fine\n")
	bad := write("bad.md", "text \u2029 more\n")
	image := write("logo.png", "\x89PNG\x00\x00")

	var out, errOut bytes.Buffer
	if code := run([]string{clean, image}, &out, &errOut); code != 0 {
		t.Fatalf("clean files: exit %d, %s%s", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := run([]string{clean, bad}, &out, &errOut); code != 1 {
		t.Fatalf("bad file: exit %d, %s", code, out.String())
	}
	if !strings.Contains(out.String(), "bad.md:1:6: U+2029 PARAGRAPH SEPARATOR") {
		t.Fatalf("output %q", out.String())
	}
}
