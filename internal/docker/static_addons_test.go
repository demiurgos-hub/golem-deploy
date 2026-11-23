package docker

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

func writeEmptyTestZip(t *testing.T, zipPath string) {
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

func TestValidateStaticAddons(t *testing.T) {
	tmp := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	if err := os.MkdirAll(utils.StaticDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEmptyTestZip(t, filepath.Join(utils.StaticDir, "a.zip"))
	writeEmptyTestZip(t, filepath.Join(utils.StaticDir, "b.zip"))
	if err := os.WriteFile(filepath.Join(utils.StaticDir, "readme.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cleaned, paths, err := ValidateStaticAddons([]StaticAddon{
		{Filename: "a.zip", ExtractTo: "secondary-server"},
		{Filename: "b.zip", ExtractTo: "addons/extra"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cleaned) != 2 || len(paths) != 2 {
		t.Fatalf("expected 2 addons, got %d / %d", len(cleaned), len(paths))
	}
	if cleaned[1].ExtractTo != filepath.Join("addons", "extra") {
		t.Fatalf("expected cleaned nested path, got %q", cleaned[1].ExtractTo)
	}

	if _, _, err := ValidateStaticAddons([]StaticAddon{
		{Filename: "missing.zip", ExtractTo: "x"},
	}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected missing archive error, got %v", err)
	}

	if _, _, err := ValidateStaticAddons([]StaticAddon{
		{Filename: "readme.txt", ExtractTo: "x"},
	}); err == nil || !strings.Contains(err.Error(), ".zip") {
		t.Fatalf("expected non-zip error, got %v", err)
	}

	if _, _, err := ValidateStaticAddons([]StaticAddon{
		{Filename: "a.zip", ExtractTo: "same"},
		{Filename: "b.zip", ExtractTo: "same"},
	}); err == nil || !strings.Contains(err.Error(), "duplicate extract_to") {
		t.Fatalf("expected duplicate extract_to error, got %v", err)
	}

	rootAddons, _, err := ValidateStaticAddons([]StaticAddon{
		{Filename: "a.zip", ExtractTo: ""},
		{Filename: "b.zip", ExtractTo: "."},
	})
	if err != nil {
		t.Fatalf("multiple root add-ons should be allowed: %v", err)
	}
	if rootAddons[0].ExtractTo != "." || rootAddons[1].ExtractTo != "." {
		t.Fatalf("root destinations should normalize to dot: %+v", rootAddons)
	}

	if _, _, err := ValidateStaticAddons([]StaticAddon{
		{Filename: "a.zip", ExtractTo: "one"},
		{Filename: "a.zip", ExtractTo: "two"},
	}); err == nil || !strings.Contains(err.Error(), "duplicate archive") {
		t.Fatalf("expected duplicate archive error, got %v", err)
	}

	if _, _, err := ValidateStaticAddons([]StaticAddon{
		{Filename: "a.zip", ExtractTo: "foo"},
		{Filename: "b.zip", ExtractTo: filepath.Join("foo", "bar")},
	}); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Fatalf("expected nested extract_to error, got %v", err)
	}
}

func TestStaticAddonFilenames(t *testing.T) {
	addons := []StaticAddon{
		{Filename: "unity.zip", ExtractTo: "secondary-server"},
		{Filename: ""},
	}
	if names := StaticAddonFilenames(addons); len(names) != 1 || names[0] != "unity.zip" {
		t.Fatalf("unexpected filenames: %v", names)
	}
	if names := StaticAddonFilenames(nil); names != nil {
		t.Fatalf("expected nil for no add-ons, got %v", names)
	}
}

func TestValidateStaticAddonsEmpty(t *testing.T) {
	if cleaned, paths, err := ValidateStaticAddons(nil); err != nil || cleaned != nil || paths != nil {
		t.Fatalf("no static add-ons should validate as a no-op: cleaned=%v paths=%v err=%v", cleaned, paths, err)
	}
}
