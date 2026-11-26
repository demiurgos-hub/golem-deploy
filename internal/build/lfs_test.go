package build

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLFSInclude(t *testing.T) {
	t.Run("patterns", func(t *testing.T) {
		dir := writeRepo(t, map[string]string{ConfigFile: "lfs:\n  include:\n    - content/**\n    - \"*.bytes\"\n"})
		got, err := LFSInclude(dir)
		if err != nil || !reflect.DeepEqual(got, []string{"content/**", "*.bytes"}) {
			t.Fatalf("%q %v", got, err)
		}
	})
	t.Run("no file", func(t *testing.T) {
		got, err := LFSInclude(writeRepo(t, map[string]string{"README.md": "x"}))
		if err != nil || got != nil {
			t.Fatalf("%q %v", got, err)
		}
	})
	t.Run("no key", func(t *testing.T) {
		got, err := LFSInclude(writeRepo(t, map[string]string{ConfigFile: "server:\n  module: x\n"}))
		if err != nil || got != nil {
			t.Fatalf("%q %v", got, err)
		}
	})
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"invalid pattern":   {map[string]string{ConfigFile: "lfs:\n  include: [\"a,b\"]\n"}, `golem-deploy.yaml: lfs.include pattern "a,b"`},
		"invalid other key": {map[string]string{ConfigFile: "root: ../x\n"}, "golem-deploy.yaml: root"},
		"not yaml":          {map[string]string{ConfigFile: "lfs: [\n"}, "golem-deploy.yaml is not valid"},
		"directory":         {map[string]string{ConfigFile + "/": ""}, "golem-deploy.yaml is not a regular file"},
		"too large":         {map[string]string{ConfigFile: "# " + strings.Repeat("x", maxConfigBytes)}, "golem-deploy.yaml is larger than"},
		"empty include":     {map[string]string{ConfigFile: "lfs:\n  include: []\n"}, "at least one pattern"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LFSInclude(writeRepo(t, tc.files))
			wantUserErr(t, err, tc.want)
		})
	}
	t.Run("missing checkout", func(t *testing.T) {
		_, err := LFSInclude(filepath.Join(t.TempDir(), "nope"))
		wantUserErr(t, err, "golem-deploy.yaml could not be read")
	})
}

// A golem-deploy.yaml that is a symbolic link is refused, wherever it
// points, like every file the daemon reads from a checkout.
func TestLFSIncludeRefusesSymlink(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(outside, "deploy.yaml")
	if err := os.WriteFile(target, []byte("lfs:\n  include: [a]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, link := range map[string]string{"outside": target, "inside": "real.yaml"} {
		t.Run(name, func(t *testing.T) {
			dir := writeRepo(t, map[string]string{"real.yaml": "lfs:\n  include: [a]\n"})
			symlink(t, link, filepath.Join(dir, ConfigFile))
			_, err := LFSInclude(dir)
			wantUserErr(t, err, "golem-deploy.yaml is a symbolic link")
		})
	}
}
