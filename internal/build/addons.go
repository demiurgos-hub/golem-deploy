package build

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/extract"
	"github.com/demiurgos-hub/golem-deploy/internal/logsafe"
	"github.com/demiurgos-hub/golem-deploy/internal/staticfiles"
)

// Addon is a static add-on: a ZIP of the static store extracted at Target
// below the game root.
type Addon struct {
	Name   string
	Target string
}

// MaxAddons bounds the add-ons of one project.
const MaxAddons = 16

// ValidateAddons checks add-on names (static store names ending in .zip)
// and targets (clean relative paths, "." for the game root) and returns
// them normalised (empty target becomes "."). Exact duplicates are refused.
func ValidateAddons(in []Addon) ([]Addon, error) {
	if len(in) > MaxAddons {
		return nil, &FieldError{Field: "static_addons", Msg: fmt.Sprintf("at most %d static add-ons are allowed", MaxAddons)}
	}
	out := make([]Addon, 0, len(in))
	seen := map[Addon]bool{}
	for i, a := range in {
		field := fmt.Sprintf("static_addons[%d]", i)
		if err := staticfiles.ValidateName(a.Name); err != nil {
			return nil, &FieldError{Field: field + ".name", Msg: err.Error()}
		}
		if !strings.HasSuffix(strings.ToLower(a.Name), ".zip") {
			return nil, &FieldError{Field: field + ".name", Msg: "a static add-on must be a .zip file"}
		}
		t, err := cleanPath("target", a.Target, true)
		if err != nil {
			return nil, &FieldError{Field: field + ".target", Msg: "target must be a relative path below the game root of letters, digits, '.', '_', '-' and '/' (no '..', no .git)"}
		}
		n := Addon{Name: a.Name, Target: t}
		key := Addon{Name: staticfiles.NameKey(n.Name), Target: n.Target}
		if seen[key] {
			return nil, &FieldError{Field: field, Msg: "the same add-on and target appear twice"}
		}
		seen[key] = true
		out = append(out, n)
	}
	return out, nil
}

// FieldError is an invalid request field (for the HTTP layer).
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Msg }

// stageAddons extracts every add-on ZIP of staticDir with lim (the total
// size limit applies to all add-ons together) and merges the files into
// dest at their targets. Extraction happens in fresh directories below
// tmpRoot (same file system as dest). Two add-ons may not provide the same
// file. Problems with the add-ons are *UserError values.
func stageAddons(ctx context.Context, addons []Addon, staticDir, tmpRoot, dest string, lim extract.Limits) (int64, error) {
	addons, err := ValidateAddons(addons)
	if err != nil {
		return 0, userErr("the project's static add-ons are invalid: %s", err.Error())
	}
	var total int64
	left := lim
	for _, a := range addons {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		src := filepath.Join(staticDir, a.Name)
		fi, err := os.Lstat(src)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return 0, userErr("the static add-on %q does not exist in the static store; upload it or remove it from the project", a.Name)
		case err != nil:
			return 0, err
		case !fi.Mode().IsRegular():
			return 0, userErr("the static add-on %q is not a regular file", a.Name)
		}
		extracted, err := extract.Zip(ctx, src, tmpRoot, left)
		if err != nil {
			if errors.Is(err, extract.ErrUnsafe) {
				return 0, userErr("the static add-on %q was refused: %s", a.Name, clip(logsafe.String(strings.TrimPrefix(err.Error(), extract.ErrUnsafe.Error()+": ")), 200))
			}
			if errors.Is(err, staticfiles.ErrNoSpace) {
				return 0, userErr("there is not enough free disk space to extract the static add-on %q", a.Name)
			}
			return 0, err
		}
		err = mergeTree(ctx, extracted.Dir, filepath.Join(dest, filepath.FromSlash(a.Target)), a.Name)
		_ = os.RemoveAll(extracted.Dir)
		if err != nil {
			return 0, err
		}
		left.MaxEntries -= extracted.Entries
		left.MaxTotalBytes -= extracted.Bytes
		total += extracted.Bytes
	}
	return total, nil
}

// mergeTree moves the regular files and directories of src into dst
// (created as needed). An existing file at a
// destination path, or a file where a directory is needed, is a conflict.
func mergeTree(ctx context.Context, src, dst, name string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		conflict := func() error {
			return userErr("the static add-on %q provides %s, which an earlier add-on already provides", name, safePath(filepath.ToSlash(rel)))
		}
		switch {
		case d.IsDir():
			if fi, err := os.Lstat(target); err == nil {
				if !fi.IsDir() {
					return conflict()
				}
				return nil
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				if fi, serr := os.Lstat(filepath.Dir(target)); serr == nil && !fi.IsDir() {
					return conflict()
				}
				return err
			}
			return nil
		case d.Type().IsRegular():
			if _, err := os.Lstat(target); err == nil {
				return conflict()
			}
			return os.Rename(p, target)
		}
		return fmt.Errorf("unexpected entry type in extracted add-on")
	})
}
