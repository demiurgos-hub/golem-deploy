package docker

import (
	"fmt"
	"os"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/extract"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// ValidateStaticAddons validates static archive add-ons for an image build request.
// It returns cleaned add-ons (normalized extract_to) and the resolved ZIP paths.
func ValidateStaticAddons(addons []StaticAddon) ([]StaticAddon, []string, error) {
	if len(addons) == 0 {
		return nil, nil, nil
	}

	cleaned := make([]StaticAddon, 0, len(addons))
	zipPaths := make([]string, 0, len(addons))
	seenFiles := make(map[string]struct{}, len(addons))
	seenDests := make(map[string]struct{}, len(addons))

	for i, addon := range addons {
		filename := strings.TrimSpace(addon.Filename)
		if filename == "" {
			return nil, nil, fmt.Errorf("static_addons[%d]: filename is required", i)
		}

		zipPath, err := utils.ResolveStaticZipArchive(filename)
		if err != nil {
			return nil, nil, fmt.Errorf("static_addons[%d]: %w", i, err)
		}

		extractTo, err := utils.ValidateExtractToPath(addon.ExtractTo)
		if err != nil {
			return nil, nil, fmt.Errorf("static_addons[%d]: %w", i, err)
		}

		if _, dup := seenFiles[filename]; dup {
			return nil, nil, fmt.Errorf("static_addons[%d]: duplicate archive filename %q", i, filename)
		}
		if _, dup := seenDests[extractTo]; dup && extractTo != "." {
			return nil, nil, fmt.Errorf("static_addons[%d]: duplicate extract_to %q", i, extractTo)
		}
		seenFiles[filename] = struct{}{}
		seenDests[extractTo] = struct{}{}

		cleaned = append(cleaned, StaticAddon{
			Filename:  filename,
			ExtractTo: extractTo,
		})
		zipPaths = append(zipPaths, zipPath)
	}

	// Reject nested extract destinations (e.g. "foo" and "foo/bar") to avoid mixed trees.
	for i := range cleaned {
		for j := range cleaned {
			if i == j {
				continue
			}
			a := cleaned[i].ExtractTo
			b := cleaned[j].ExtractTo
			if a == "." || b == "." {
				continue
			}
			sep := string(os.PathSeparator)
			if strings.HasPrefix(b, a+sep) {
				return nil, nil, fmt.Errorf("static_addons: extract_to %q is nested under %q", b, a)
			}
		}
	}

	return cleaned, zipPaths, nil
}

// ApplyStaticAddons extracts validated static ZIP add-ons into the build context.
// Archives are read from the static store and are not left in the context.
func ApplyStaticAddons(contextDir string, addons []StaticAddon, zipPaths []string) error {
	if len(addons) == 0 {
		return nil
	}
	if len(addons) != len(zipPaths) {
		return fmt.Errorf("internal error: static addon path count mismatch")
	}

	for i, addon := range addons {
		if err := extract.ExtractStaticAddonIntoContext(zipPaths[i], contextDir, addon.ExtractTo); err != nil {
			return err
		}
	}
	return nil
}

// StaticAddonFilenames returns the archive filenames for exclusion from loose static copies.
func StaticAddonFilenames(addons []StaticAddon) []string {
	if len(addons) == 0 {
		return nil
	}
	names := make([]string, 0, len(addons))
	for _, addon := range addons {
		if addon.Filename != "" {
			names = append(names, addon.Filename)
		}
	}
	return names
}
