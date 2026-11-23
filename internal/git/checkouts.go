package git

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Prefixes of scratch entries under the temp directory; SweepTemp removes
// them at startup.
const (
	sessionPrefix = "git-"
	clonePrefix   = "clone-"
	removePrefix  = "remove-"
)

// ErrRemoveIncomplete means RemoveCheckout moved the checkout out of the
// repositories directory but could not delete all of it; the remains are in
// the temp directory and SweepTemp removes them at the next startup.
var ErrRemoveIncomplete = errors.New("checkout moved aside but not fully removed")

// CheckoutDir returns the checkout directory of project id below root:
// root/<id>. It is derived from the numeric id only, never from a name.
func CheckoutDir(root string, id int64) string {
	return filepath.Join(root, strconv.FormatInt(id, 10))
}

// containedCheckout resolves root (following symlinks) and returns the
// checkout path of id after verifying it is a direct child of root.
func containedCheckout(root string, id int64) (string, error) {
	if id <= 0 {
		return "", errors.New("invalid project id")
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve repositories dir: %w", err)
	}
	dir := CheckoutDir(base, id)
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel != strconv.FormatInt(id, 10) || strings.ContainsAny(rel, `/\`) {
		return "", errors.New("checkout path escapes the repositories dir")
	}
	return dir, nil
}

// RemoveCheckout deletes the checkout of project id below root. The
// directory is first renamed into tempDir (same filesystem), so after a
// successful return root/<id> is gone even if deleting the moved tree fails
// part-way; SweepTemp removes such leftovers at the next startup. A
// symlink at root/<id> is removed without being followed. A missing
// checkout is not an error.
func RemoveCheckout(root string, id int64, tempDir string) error {
	dir, err := containedCheckout(root, id)
	if err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return os.Remove(dir)
	}
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(tempDir, removePrefix)
	if err != nil {
		return err
	}
	moved := filepath.Join(staging, "repo")
	if err := os.Rename(dir, moved); err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("move checkout aside: %w", err)
	}
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("%w: %v", ErrRemoveIncomplete, err)
	}
	return nil
}

// SweepTemp removes scratch entries a previous run left in tempDir
// (operation directories with SSH keys, clone staging, removals).
func SweepTemp(tempDir string) error {
	entries, err := os.ReadDir(tempDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, sessionPrefix) || strings.HasPrefix(n, clonePrefix) || strings.HasPrefix(n, removePrefix) {
			if err := os.RemoveAll(filepath.Join(tempDir, n)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// SweepCheckouts removes checkout directories below root whose numeric name
// is not a project id for which keep returns true (left behind when a
// deletion failed half-way). Entries with non-numeric names are left alone.
// It returns the removed ids.
func SweepCheckouts(root, tempDir string, keep func(id int64) bool) ([]int64, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []int64
	var errs []error
	for _, e := range entries {
		id, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != e.Name() || keep(id) {
			continue
		}
		if err := RemoveCheckout(root, id, tempDir); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, id)
	}
	return removed, errors.Join(errs...)
}
