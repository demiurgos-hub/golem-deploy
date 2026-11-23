package utils

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEmptyZip(t *testing.T, zipPath string) {
	t.Helper()
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	if err := writer.Close(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateExtractToPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "simple", input: "secondary-server", want: "secondary-server"},
		{name: "nested", input: "addons/unity", want: filepath.Join("addons", "unity")},
		{name: "trimmed", input: "  secondary-server  ", want: "secondary-server"},
		{name: "cleaned dots", input: "foo/./bar", want: filepath.Join("foo", "bar")},
		{name: "empty uses root", input: "", want: "."},
		{name: "whitespace uses root", input: "  ", want: "."},
		{name: "dot uses root", input: ".", want: "."},
		{name: "dotdot", input: "..", wantErr: true},
		{name: "traversal", input: "../etc", wantErr: true},
		{name: "traversal nested", input: "foo/../../etc", wantErr: true},
		{name: "resolved traversal", input: "foo/../bar", wantErr: true},
		{name: "windows traversal", input: `..\etc`, wantErr: true},
		{name: "absolute unix", input: "/tmp/evil", wantErr: true},
		{name: "absolute windows", input: `C:\evil`, wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ValidateExtractToPath(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveStaticZipArchive(t *testing.T) {
	tmp := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	if err := os.MkdirAll(StaticDir, 0o755); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(StaticDir, "unity-addon.zip")
	writeEmptyZip(t, zipPath)
	if err := os.WriteFile(filepath.Join(StaticDir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(StaticDir, "invalid.zip"), []byte("not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	abs, err := ResolveStaticZipArchive("unity-addon.zip")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, _ := filepath.Abs(zipPath)
	if abs != want {
		t.Fatalf("got %q, want %q", abs, want)
	}

	if _, err := ResolveStaticZipArchive("notes.txt"); err == nil || !strings.Contains(err.Error(), ".zip") {
		t.Fatalf("expected non-zip error, got %v", err)
	}
	if _, err := ResolveStaticZipArchive("../unity-addon.zip"); err == nil {
		t.Fatal("expected traversal rejection")
	}
	if _, err := ResolveStaticZipArchive("missing.zip"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %v", err)
	}
	if _, err := ResolveStaticZipArchive("invalid.zip"); err == nil || !strings.Contains(err.Error(), "not a valid ZIP") {
		t.Fatalf("expected invalid ZIP error, got %v", err)
	}
}

func TestCopyStaticFilesToDirExcludesAddons(t *testing.T) {
	tmp := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	if err := os.MkdirAll(StaticDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(StaticDir, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(StaticDir, "addon.zip"), []byte("zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(tmp, "context")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CopyStaticFilesToDir(target, "addon.zip"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(target, "keep.txt")); err != nil {
		t.Fatalf("expected keep.txt to be copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "addon.zip")); !os.IsNotExist(err) {
		t.Fatal("addon.zip should be excluded from loose static copy")
	}
}
