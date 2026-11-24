package logsafe

import (
	"strings"
	"testing"
)

func TestWriter(t *testing.T) {
	var out strings.Builder
	w := NewWriter(&out, "> ", "/data/tmp/build-1", "<build-dir>", "/data", "<data>")
	_, _ = w.Write([]byte("step 1 in /data/tmp/bu"))
	_, _ = w.Write([]byte("ild-1/x\r\nprogress\r50%\n\x1b[31mred\u202e\xff\tok\n"))
	_, _ = w.Write([]byte("tail without newline /data/y"))
	_ = w.Close()
	want := "> step 1 in <build-dir>/x\n> progress\n> 50%\n> \\x1b[31mred\\u202e\uFFFD\tok\n> tail without newline <data>/y\n"
	if out.String() != want {
		t.Fatalf("got  %q\nwant %q", out.String(), want)
	}
}

func TestWriterCapsLines(t *testing.T) {
	var out strings.Builder
	w := NewWriter(&out, "")
	_, _ = w.Write([]byte(strings.Repeat("x", 3*MaxLine) + "\nnext\n"))
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], TruncatedMarker) || len(lines[0]) != MaxLine+len(TruncatedMarker) || lines[1] != "next" {
		t.Fatalf("%d lines, first %d bytes", len(lines), len(lines[0]))
	}
}

func TestString(t *testing.T) {
	if got := String("a\nb\x00\u2028"); got != `a\x0ab\x00\u2028` {
		t.Fatalf("%q", got)
	}
}
