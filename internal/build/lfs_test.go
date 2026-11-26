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
	// Only the lfs section is validated during a clone or pull: a mistake
	// in another key is reported when a build starts, as before.
	t.Run("other keys are not validated", func(t *testing.T) {
		for _, body := range []string{"root: ../x\n", "bogus: 1\nlfs:\n  include: [a]\n", "server:\n  module: /abs\n"} {
			got, err := LFSInclude(writeRepo(t, map[string]string{ConfigFile: body}))
			if err != nil {
				t.Fatalf("%q: %v", body, err)
			}
			if strings.Contains(body, "lfs:") != (len(got) == 1) {
				t.Fatalf("%q: %q", body, got)
			}
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
		"null item":         {map[string]string{ConfigFile: "lfs:\n  include: [content/**, null]\n"}, "lfs.include must be a list of strings"},
		"number item":       {map[string]string{ConfigFile: "lfs:\n  include: [7]\n"}, "lfs.include must be a list of strings"},
		"null include":      {map[string]string{ConfigFile: "lfs:\n  include:\n"}, "at least one pattern"},
		"unknown lfs key":   {map[string]string{ConfigFile: "lfs:\n  exclude: [a]\n"}, "golem-deploy.yaml: lfs has an unknown key"},
		"lfs not a mapping": {map[string]string{ConfigFile: "lfs: [a]\n"}, "golem-deploy.yaml: lfs must be a mapping"},
		"stored in Git LFS": {map[string]string{ConfigFile: "version https://git-lfs.github.com/spec/v1\noid sha256:49612254ff41de93d273dea55a5740a4ec192b88aeac815caf24eb4485a8c7f2\nsize 120\n"}, "golem-deploy.yaml is stored in Git LFS"},
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
