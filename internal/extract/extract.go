package extract

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// ExtractZipArchiveOnly extracts a zip file into destDir. Nested archives are
// left as-is. Executable permission bits from ZIP entries are preserved via
// file.Mode().
func ExtractZipArchiveOnly(zipPath string, destDir string) error {
	// Create destination directory
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	// Open the zip file
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("failed to open zip file: %w", err)
	}
	defer reader.Close()

	// Extract each file
	for _, file := range reader.File {
		if err := extractZipFile(file, destDir); err != nil {
			return fmt.Errorf("failed to extract %s: %w", file.Name, err)
		}
	}

	return nil
}

// ExtractStaticAddonIntoContext extracts a static ZIP archive into contextDir/extractTo.
// Empty and "." destinations merge the archive's top-level entries into the context
// root. The ZIP itself is never copied into the build context, and existing paths
// are never overwritten.
func ExtractStaticAddonIntoContext(zipPath, contextDir, extractTo string) error {
	cleaned, err := utils.ValidateExtractToPath(extractTo)
	if err != nil {
		return err
	}

	fmt.Printf("Extracting static addon %s -> %s\n", filepath.Base(zipPath), cleaned)
	if cleaned == "." {
		return extractStaticAddonAtRoot(zipPath, contextDir)
	}

	destDir, err := utils.ValidatePathWithinDirectory(contextDir, cleaned)
	if err != nil {
		return fmt.Errorf("extract_to escapes the build context: %w", err)
	}

	if _, err := os.Stat(destDir); err == nil {
		return fmt.Errorf("extract destination already exists in build context: %s", cleaned)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to check extract destination %s: %w", cleaned, err)
	}

	if err := ExtractZipArchiveOnly(zipPath, destDir); err != nil {
		// Best-effort cleanup of a partial extract
		os.RemoveAll(destDir)
		return fmt.Errorf("failed to extract static addon %s: %w", filepath.Base(zipPath), err)
	}

	return nil
}

func extractStaticAddonAtRoot(zipPath, contextDir string) error {
	absContext, err := filepath.Abs(contextDir)
	if err != nil {
		return fmt.Errorf("failed to resolve build context: %w", err)
	}

	stagingDir, err := os.MkdirTemp(filepath.Dir(absContext), ".golem-deploy-static-addon-*")
	if err != nil {
		return fmt.Errorf("failed to create static addon staging directory: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	if err := ExtractZipArchiveOnly(zipPath, stagingDir); err != nil {
		return fmt.Errorf("failed to extract static addon %s: %w", filepath.Base(zipPath), err)
	}

	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return fmt.Errorf("failed to inspect static addon %s: %w", filepath.Base(zipPath), err)
	}

	for _, entry := range entries {
		targetPath := filepath.Join(absContext, entry.Name())
		if _, err := os.Lstat(targetPath); err == nil {
			return fmt.Errorf("static addon path already exists in build context: %s", entry.Name())
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("failed to check static addon path %s: %w", entry.Name(), err)
		}
	}

	movedPaths := make([]string, 0, len(entries))
	for _, entry := range entries {
		sourcePath := filepath.Join(stagingDir, entry.Name())
		targetPath := filepath.Join(absContext, entry.Name())
		if err := os.Rename(sourcePath, targetPath); err != nil {
			for _, movedPath := range movedPaths {
				_ = os.RemoveAll(movedPath)
			}
			return fmt.Errorf("failed to add static addon path %s to build context: %w", entry.Name(), err)
		}
		movedPaths = append(movedPaths, targetPath)
	}

	return nil
}

// extractZipFile extracts a single file from a zip archive
func extractZipFile(file *zip.File, destDir string) error {
	// ZIP paths conventionally use forward slashes. Normalize backslashes too so
	// malicious Windows-style traversal is rejected on every host platform.
	entryName := filepath.FromSlash(strings.ReplaceAll(file.Name, `\`, "/"))
	cleanedName := filepath.Clean(entryName)
	if cleanedName == "." || filepath.IsAbs(cleanedName) || filepath.VolumeName(cleanedName) != "" {
		return fmt.Errorf("invalid file path: %s", file.Name)
	}

	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("failed to resolve destination directory: %w", err)
	}
	filePath := filepath.Join(absDest, cleanedName)
	rel, err := filepath.Rel(absDest, filePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("invalid file path: %s", file.Name)
	}

	// Create directories if needed
	if file.FileInfo().IsDir() {
		return os.MkdirAll(filePath, file.Mode())
	}

	// Create parent directory
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		return err
	}

	// Create the file
	destFile, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
	if err != nil {
		return err
	}
	defer destFile.Close()

	// Open the file in the zip archive
	srcFile, err := file.Open()
	if err != nil {
		return err
	}
	defer srcFile.Close()

	// Copy contents
	_, err = io.Copy(destFile, srcFile)
	return err
}
