package build

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/extract"
)

func makeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b, err := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStageAddons(t *testing.T) {
	static := t.TempDir()
	makeZip(t, filepath.Join(static, "maps.zip"), map[string]string{"level1.json": "1", "tiles/a.png": "a"})
	makeZip(t, filepath.Join(static, "sounds.zip"), map[string]string{"sfx/boom.ogg": "boom"})
	makeZip(t, filepath.Join(static, "more.zip"), map[string]string{"tiles/b.png": "b"})
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "static")
	if err := os.Mkdir(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	n, err := stageAddons([]Addon{{"maps.zip", "assets/maps"}, {"sounds.zip", ""}, {"more.zip", "assets/maps"}}, static, tmp, dest, extract.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	got := readTree(t, dest)
	want := map[string]string{"assets/maps/level1.json": "1", "assets/maps/tiles/a.png": "a", "assets/maps/tiles/b.png": "b", "sfx/boom.ogg": "boom"}
	if len(got) != len(want) || n != 7 {
		t.Fatalf("got %v (%d bytes)", got, n)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("got %v", got)
		}
	}
	// Extraction directories are cleaned up; only the staging dir remains.
	entries, _ := os.ReadDir(tmp)
	if len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestStageAddonsRejects(t *testing.T) {
	static := t.TempDir()
	makeZip(t, filepath.Join(static, "a.zip"), map[string]string{"x/y.txt": "1"})
	makeZip(t, filepath.Join(static, "b.zip"), map[string]string{"x/y.txt": "2"})
	makeZip(t, filepath.Join(static, "c.zip"), map[string]string{"x": "file where a directory is"})
	makeZip(t, filepath.Join(static, "evil.zip"), map[string]string{"../../etc/passwd": "x"})
	makeZip(t, filepath.Join(static, "big.zip"), map[string]string{"big.bin": strings.Repeat("z", 4096)})
	if err := os.WriteFile(filepath.Join(static, "notzip.zip"), []byte("plain text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(static, "dir.zip"), 0o700); err != nil {
		t.Fatal(err)
	}
	small := extract.DefaultLimits()
	small.MaxTotalBytes = 1000
	cases := []struct {
		name   string
		addons []Addon
		lim    extract.Limits
		want   string
	}{
		{"missing", []Addon{{"nope.zip", "."}}, extract.DefaultLimits(), "does not exist in the static store"},
		{"not a zip", []Addon{{"notzip.zip", "."}}, extract.DefaultLimits(), "not a valid ZIP archive"},
		{"directory", []Addon{{"dir.zip", "."}}, extract.DefaultLimits(), "not a regular file"},
		{"same file twice", []Addon{{"a.zip", "."}, {"b.zip", "."}}, extract.DefaultLimits(), "already provides"},
		{"file where a directory is needed", []Addon{{"c.zip", "."}, {"a.zip", "."}}, extract.DefaultLimits(), "already provides"},
		{"traversal entry", []Addon{{"evil.zip", "."}}, extract.DefaultLimits(), "was refused"},
		{"total size", []Addon{{"big.zip", "."}}, small, "refused"},
		{"bad target", []Addon{{"a.zip", "../x"}}, extract.DefaultLimits(), "invalid"},
		{"bad name", []Addon{{"../a.zip", "."}}, extract.DefaultLimits(), "invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			dest := filepath.Join(tmp, "static")
			_ = os.Mkdir(dest, 0o700)
			_, err := stageAddons(tc.addons, static, tmp, dest, tc.lim)
			wantUserErr(t, err, tc.want)
			if strings.Contains(err.Error(), static) || strings.Contains(err.Error(), tmp) {
				t.Fatalf("message contains a host path: %v", err)
			}
		})
	}
}

func TestValidateAddons(t *testing.T) {
	got, err := ValidateAddons([]Addon{{"a.zip", ""}, {"b.ZIP", "./assets/"}})
	if err != nil || got[0].Target != "." || got[1].Target != "assets" {
		t.Fatalf("%v %v", got, err)
	}
	bad := map[string][]Addon{
		"not zip":     {{"a.txt", "."}},
		"traversal":   {{"a.zip", "a/../.."}},
		"absolute":    {{"a.zip", "/app"}},
		"git":         {{"a.zip", ".git"}},
		"name path":   {{"dir/a.zip", "."}},
		"newline":     {{"a.zip", "a\nb"}},
		"duplicate":   {{"a.zip", "x"}, {"a.zip", "x/"}},
		"empty name":  {{"", "."}},
		"device name": {{"CON.zip", "."}},
	}
	for name, in := range bad {
		var fe *FieldError
		if _, err := ValidateAddons(in); !errors.As(err, &fe) {
			t.Errorf("%s: %v", name, err)
		}
	}
	many := make([]Addon, MaxAddons+1)
	for i := range many {
		many[i] = Addon{Name: "a.zip", Target: strings.Repeat("x", i+1)}
	}
	if _, err := ValidateAddons(many); err == nil {
		t.Error("too many add-ons accepted")
	}
}
