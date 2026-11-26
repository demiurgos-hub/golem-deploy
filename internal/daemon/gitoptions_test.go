package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/datadir"
)

// The git runner limits Git LFS downloads with the build package's reader
// of golem-deploy.yaml (lfs.include).
func TestGitOptionsReadLFSInclude(t *testing.T) {
	layout := datadir.Layout{Root: t.TempDir()}
	o := gitOptions(layout, 123)
	if o.TempDir != layout.Tmp() || o.KnownHostsFile != layout.KnownHosts() || o.MaxRepoSize != 123 || o.LFSInclude == nil {
		t.Fatalf("%+v", o)
	}
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "golem-deploy.yaml"), []byte("lfs:\n  include: [content/**]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := o.LFSInclude(checkout); err != nil || !slices.Equal(got, []string{"content/**"}) {
		t.Fatalf("%q %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "golem-deploy.yaml"), []byte("lfs:\n  include: [\"a,b\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := o.LFSInclude(checkout); err == nil {
		t.Fatal("invalid pattern accepted")
	}
}
