package utils

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	UploadDir     = "./uploads"
	StaticDir     = "./uploads/static"
	MaxUploadSize = 1024 * 1024 * 1024 // 1 GB
	MaxMemory     = 128 << 20          // 128 MB for storing file parts in memory before writing to disk
)

// dangerousCharsRegex matches characters that could cause security issues
// Blocks: path separators (/\), null bytes, control characters, and path traversal (..)
var dangerousCharsRegex = regexp.MustCompile(`[/\\<>:"|?*\x00-\x1f]|\.\.`)

// Init creates the uploads directory if it doesn't exist
func Init() error {
	dirs := []string{UploadDir, StaticDir}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0775); err != nil {
			return fmt.Errorf("could not create directory %s: %w", dir, err)
		}
	}
	return nil
}

// ValidatePathWithinDirectory validates that a file path is within a base directory
// and returns the absolute path if valid. This prevents path traversal attacks.
func ValidatePathWithinDirectory(baseDir, filename string) (string, error) {
	// Get absolute path of base directory
	absBaseDir, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve base directory: %w", err)
	}

	// Build the file path
	filePath := filepath.Join(baseDir, filename)
	absFilePath, err := filepath.Abs(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve file path: %w", err)
	}

	// Ensure the resolved path is within the base directory
	relPath, err := filepath.Rel(absBaseDir, absFilePath)
	if err != nil || (len(relPath) > 0 && relPath[0] == '.') {
		return "", fmt.Errorf("invalid path: file is outside base directory")
	}

	return absFilePath, nil
}

// CopyStaticFilesToDir copies all files from the static directory to a target directory.
// Filenames listed in excludeNames are skipped (used for static ZIP add-ons that are
// extracted into the build context instead of being copied as loose archives).
func CopyStaticFilesToDir(targetDir string, excludeNames ...string) error {
	// Check if static directory exists
	if _, err := os.Stat(StaticDir); os.IsNotExist(err) {
		return nil // No static files to copy, not an error
	}

	exclude := make(map[string]struct{}, len(excludeNames))
	for _, name := range excludeNames {
		if name != "" {
			exclude[name] = struct{}{}
		}
	}

	fmt.Printf("Copying static files from %s to %s...\n", StaticDir, targetDir)

	// Read all files in static directory
	entries, err := os.ReadDir(StaticDir)
	if err != nil {
		return fmt.Errorf("failed to read static directory: %w", err)
	}

	copiedCount := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue // Skip directories for now
		}
		if _, skip := exclude[entry.Name()]; skip {
			fmt.Printf("  Skipped (static addon): %s\n", entry.Name())
			continue
		}

		srcPath := filepath.Join(StaticDir, entry.Name())
		dstPath := filepath.Join(targetDir, entry.Name())

		// Copy the file
		if err := copyFile(srcPath, dstPath); err != nil {
			return fmt.Errorf("failed to copy %s: %w", entry.Name(), err)
		}
		copiedCount++
		fmt.Printf("  Copied: %s\n", entry.Name())
	}

	fmt.Printf("Copied %d static file(s)\n", copiedCount)
	return nil
}

// ValidateExtractToPath validates a relative extraction destination for use under a
// Docker build context. Empty and "." destinations resolve to the context root.
func ValidateExtractToPath(extractTo string) (string, error) {
	extractTo = strings.TrimSpace(extractTo)
	if extractTo == "" {
		return ".", nil
	}
	for _, r := range extractTo {
		if r < 0x20 {
			return "", fmt.Errorf("extract_to must not contain control characters")
		}
	}

	// ZIP/API paths use forward slashes. Normalize backslashes first so traversal
	// and absolute paths are rejected consistently on Linux and Windows.
	normalized := strings.ReplaceAll(extractTo, `\`, "/")
	if path.IsAbs(normalized) || strings.HasPrefix(normalized, "//") {
		return "", fmt.Errorf("extract_to must be a relative path")
	}
	if len(normalized) >= 2 && normalized[1] == ':' {
		return "", fmt.Errorf("extract_to must be a relative path")
	}

	for _, part := range strings.Split(normalized, "/") {
		if part == ".." {
			return "", fmt.Errorf("extract_to must not contain path traversal sequences")
		}
	}

	cleaned := path.Clean(normalized)
	if cleaned == "." {
		return ".", nil
	}

	return filepath.FromSlash(cleaned), nil
}

// ResolveStaticZipArchive validates that filename is a ZIP inside the static store
// and returns its absolute path.
func ResolveStaticZipArchive(filename string) (string, error) {
	if filename == "" || strings.TrimSpace(filename) == "" {
		return "", fmt.Errorf("static archive filename is required")
	}
	if filepath.Base(filename) != filename {
		return "", fmt.Errorf("invalid static archive filename: %s", filename)
	}
	// Reject path separators, control characters, and traversal sequences.
	if dangerousCharsRegex.MatchString(filename) {
		return "", fmt.Errorf("invalid static archive filename: %s", filename)
	}
	if !strings.EqualFold(filepath.Ext(filename), ".zip") {
		return "", fmt.Errorf("static archive must be a .zip file: %s", filename)
	}

	absPath, err := ValidatePathWithinDirectory(StaticDir, filename)
	if err != nil {
		return "", fmt.Errorf("invalid static archive path: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("static archive not found: %s", filename)
		}
		return "", fmt.Errorf("failed to access static archive %s: %w", filename, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("static archive is a directory: %s", filename)
	}

	reader, err := zip.OpenReader(absPath)
	if err != nil {
		return "", fmt.Errorf("static archive is not a valid ZIP file: %s: %w", filename, err)
	}
	if err := reader.Close(); err != nil {
		return "", fmt.Errorf("failed to close static archive %s: %w", filename, err)
	}
	return absPath, nil
}

// copyFile copies a single file from src to dst
func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
}

// SaveStaticFiles saves multiple uploaded files to the uploads/static directory
// If a file with the same name exists, it will be overwritten
func SaveStaticFiles(r *http.Request) ([]string, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, MaxUploadSize)
	if err := r.ParseMultipartForm(MaxMemory); err != nil {
		return nil, fmt.Errorf("file too large or invalid form: %w", err)
	}

	// Get all uploaded files (field name is "files")
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		return nil, fmt.Errorf("no files found with field name 'files'")
	}

	savedFiles := make([]string, 0, len(files))

	for _, handler := range files {
		file, err := handler.Open()
		if err != nil {
			return savedFiles, fmt.Errorf("failed to open file %s: %w", handler.Filename, err)
		}
		defer file.Close()

		filename := filepath.Base(handler.Filename)
		destPath := filepath.Join(StaticDir, filename)

		// Create/overwrite the file
		dst, err := os.Create(destPath)
		if err != nil {
			return savedFiles, fmt.Errorf("unable to create file %s: %w", filename, err)
		}
		defer dst.Close()

		if _, err := io.Copy(dst, file); err != nil {
			return savedFiles, fmt.Errorf("file copy error for %s: %w", filename, err)
		}

		savedFiles = append(savedFiles, filename)
	}

	return savedFiles, nil
}
