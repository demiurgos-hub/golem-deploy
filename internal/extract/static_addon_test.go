package extract

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeTestZip(t *testing.T, path string, files map[string]os.FileMode) {
	t.Helper()
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	for name, mode := range files {
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, "content-"+name); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractStaticAddonIntoContext(t *testing.T) {
	tmp := t.TempDir()
	contextDir := filepath.Join(tmp, "context")
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "primary.bin"), []byte("primary"), 0o644); err != nil {
		t.Fatal(err)
	}

	zipPath := filepath.Join(tmp, "unity-addon.zip")
	writeTestZip(t, zipPath, map[string]os.FileMode{
		"Server.x86_64": 0o755,
		"data/config":   0o644,
	})

	if err := ExtractStaticAddonIntoContext(zipPath, contextDir, "secondary-server"); err != nil {
		t.Fatalf("extract failed: %v", err)
	}

	extracted := filepath.Join(contextDir, "secondary-server", "Server.x86_64")
	info, err := os.Stat(extracted)
	if err != nil {
		t.Fatalf("expected extracted executable: %v", err)
	}
	if runtime.GOOS != "windows" {
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("expected executable bits to be preserved, got %v", info.Mode())
		}
	}

	// ZIP itself must not be present in the build context
	if _, err := os.Stat(filepath.Join(contextDir, "unity-addon.zip")); !os.IsNotExist(err) {
		t.Fatal("zip archive must not remain in the build context")
	}
	if _, err := os.Stat(filepath.Join(contextDir, "secondary-server", "unity-addon.zip")); !os.IsNotExist(err) {
		t.Fatal("zip archive must not remain under extract_to")
	}

	// Existing destination should fail (no silent overwrite)
	if err := ExtractStaticAddonIntoContext(zipPath, contextDir, "secondary-server"); err == nil {
		t.Fatal("expected conflict error for existing destination")
	}

	// Traversal destinations rejected
	if err := ExtractStaticAddonIntoContext(zipPath, contextDir, "../escape"); err == nil {
		t.Fatal("expected traversal rejection")
	}
}

func TestExtractStaticAddonAtContextRoot(t *testing.T) {
	tmp := t.TempDir()
	contextDir := filepath.Join(tmp, "context")
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		t.Fatal(err)
	}
	primaryPath := filepath.Join(contextDir, "primary.bin")
	if err := os.WriteFile(primaryPath, []byte("primary"), 0o644); err != nil {
		t.Fatal(err)
	}

	zipPath := filepath.Join(tmp, "unity-addon.zip")
	writeTestZip(t, zipPath, map[string]os.FileMode{
		"X/Server.x86_64": 0o755,
		"X/data/config":   0o644,
	})

	if err := ExtractStaticAddonIntoContext(zipPath, contextDir, "."); err != nil {
		t.Fatalf("root extract failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(contextDir, "X", "Server.x86_64")); err != nil {
		t.Fatalf("expected archive folder at context root: %v", err)
	}
	if contents, err := os.ReadFile(primaryPath); err != nil || string(contents) != "primary" {
		t.Fatalf("primary build file changed: contents=%q err=%v", contents, err)
	}

	conflictZip := filepath.Join(tmp, "conflict.zip")
	writeTestZip(t, conflictZip, map[string]os.FileMode{
		"primary.bin": 0o644,
		"new-file":    0o644,
	})
	if err := ExtractStaticAddonIntoContext(conflictZip, contextDir, ""); err == nil {
		t.Fatal("expected root extraction conflict")
	}
	if _, err := os.Stat(filepath.Join(contextDir, "new-file")); !os.IsNotExist(err) {
		t.Fatal("conflicting root extraction must not add any archive entries")
	}
	if contents, err := os.ReadFile(primaryPath); err != nil || string(contents) != "primary" {
		t.Fatalf("conflicting root extraction changed primary file: contents=%q err=%v", contents, err)
	}
}

func TestExtractZipRejectsZipSlip(t *testing.T) {
	tmp := t.TempDir()
	zipPath := filepath.Join(tmp, "slip.zip")

	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	w, err := zw.Create("../evil.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "nope"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(tmp, "out")
	if err := ExtractZipArchiveOnly(zipPath, dest); err == nil {
		t.Fatal("expected zip-slip rejection")
	}
	if _, err := os.Stat(filepath.Join(tmp, "evil.txt")); !os.IsNotExist(err) {
		t.Fatal("zip-slip file must not be written outside destination")
	}
}
