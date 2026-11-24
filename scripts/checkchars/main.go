// Command checkchars fails when a tracked text file contains characters
// that can hide or reorder what a reviewer sees: Unicode bidirectional
// controls (U+202A–U+202E, U+2066–U+2069, U+200E, U+200F, U+061C), the
// line and paragraph separators U+2028/U+2029, NUL, and other C0 control
// characters except tab, line feed and carriage return (a literal ESC can
// rewrite a terminal that prints the file).
//
// Run it from anywhere inside the repository (CI does too):
//
//	go run ./scripts/checkchars
//
// With arguments it checks those files instead of every tracked file.
// Tests that need such characters should write them as escapes ("\u202e",
// "\x1b") so the allowlist below stays empty.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// allowlist names tracked files that may contain the characters above,
// each with the reason. Keep it empty: prefer escapes in tests.
var allowlist = map[string]string{
	// "internal/example/testdata/bidi.txt": "fixture that must hold a literal U+202E",
}

// binaryExt are extensions of files that are not text and are skipped.
var binaryExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".ico": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".zip": true, ".gz": true, ".tgz": true, ".wasm": true, ".pdf": true,
	".mp3": true, ".ogg": true, ".wav": true, ".mp4": true, ".webm": true,
	".db": true, ".sqlite": true, ".exe": true,
}

// Finding is one forbidden character.
type Finding struct {
	Path      string
	Line, Col int // 1-based; Col counts characters
	Rune      rune
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d:%d: %s", f.Path, f.Line, f.Col, describe(f.Rune))
}

// forbidden reports whether r may not appear in a tracked text file.
func forbidden(r rune) bool {
	switch {
	case r == '\t', r == '\n', r == '\r':
		return false
	case r < 0x20, r == 0x7f:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069,
		r == 0x200e, r == 0x200f, r == 0x061c, r == 0x2028, r == 0x2029:
		return true
	}
	return false
}

var names = map[rune]string{
	0x0000: "NUL", 0x001b: "ESCAPE", 0x007f: "DELETE",
	0x061c: "ARABIC LETTER MARK", 0x200e: "LEFT-TO-RIGHT MARK", 0x200f: "RIGHT-TO-LEFT MARK",
	0x2028: "LINE SEPARATOR", 0x2029: "PARAGRAPH SEPARATOR",
	0x202a: "LEFT-TO-RIGHT EMBEDDING", 0x202b: "RIGHT-TO-LEFT EMBEDDING", 0x202c: "POP DIRECTIONAL FORMATTING",
	0x202d: "LEFT-TO-RIGHT OVERRIDE", 0x202e: "RIGHT-TO-LEFT OVERRIDE",
	0x2066: "LEFT-TO-RIGHT ISOLATE", 0x2067: "RIGHT-TO-LEFT ISOLATE", 0x2068: "FIRST STRONG ISOLATE",
	0x2069: "POP DIRECTIONAL ISOLATE",
}

func describe(r rune) string {
	name := names[r]
	if name == "" {
		name = "control character"
	}
	return fmt.Sprintf("U+%04X %s", r, name)
}

// Scan returns the forbidden characters in content. Invalid UTF-8 is not
// reported (bytes that do not decode are skipped one at a time), but a NUL
// byte always is.
func Scan(path string, content []byte) []Finding {
	var out []Finding
	line, col := 1, 0
	for i := 0; i < len(content); {
		r, size := utf8.DecodeRune(content[i:])
		i += size
		col++
		if r == '\n' {
			line, col = line+1, 0
			continue
		}
		if r != utf8.RuneError && forbidden(r) {
			out = append(out, Finding{Path: path, Line: line, Col: col, Rune: r})
		}
	}
	return out
}

func trackedFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	return files, nil
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not inside a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func run(args []string, stdout, stderr io.Writer) int {
	root := "" // arguments are paths as given
	files := args
	if len(files) == 0 {
		var err error
		if root, err = repoRoot(); err != nil {
			fmt.Fprintln(stderr, "checkchars:", err)
			return 2
		}
		if files, err = trackedFiles(root); err != nil {
			fmt.Fprintln(stderr, "checkchars:", err)
			return 2
		}
	}
	w := bufio.NewWriter(stdout)
	defer w.Flush()
	found, checked := 0, 0
	for _, f := range files {
		rel := filepath.ToSlash(f)
		if binaryExt[strings.ToLower(filepath.Ext(rel))] {
			continue
		}
		if _, ok := allowlist[rel]; ok {
			continue
		}
		path := f
		if root != "" {
			path = filepath.Join(root, f)
		}
		content, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue // deleted in the working tree
		}
		if err != nil {
			fmt.Fprintln(stderr, "checkchars:", err)
			return 2
		}
		checked++
		for _, fi := range Scan(rel, content) {
			fmt.Fprintln(w, fi)
			found++
		}
	}
	if found > 0 {
		fmt.Fprintf(w, "checkchars: %d forbidden character(s); write them as escapes (\\u202e, \\x1b) or, for a fixture that needs them, add the file to the allowlist in scripts/checkchars/main.go\n", found)
		return 1
	}
	fmt.Fprintf(w, "checkchars: %d files clean\n", checked)
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
