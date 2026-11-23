package git

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// commitRandom commits name with n incompressible bytes.
func (g *gitServer) commitRandom(name string, n int) {
	data := make([]byte, n)
	_, _ = rand.Read(data)
	g.commit(name, string(data))
}

// TestG3_CloneSizeLimit: a clone that grows past MaxRepoSize fails with
// ReasonSizeLimit, leaves no checkout and no staging behind.
func TestG3_CloneSizeLimit(t *testing.T) {
	g := newGitServer(t)
	g.commitRandom("big.dat", 3<<20)
	g.push()
	r, base := g.runner(t, func(o *Options) { o.MaxRepoSize = 1 << 20; o.sizeInterval = 50 * time.Millisecond })
	dir := filepath.Join(base, "repositories", "1")
	var log bytes.Buffer
	_, err := r.Clone(context.Background(), Remote{URL: g.url("repo.git"), Auth: httpsAuth(srvPass)}, dir, &log)
	var ge *Error
	if !errors.As(err, &ge) || ge.Reason != ReasonSizeLimit || !ge.Unchanged {
		t.Fatalf("clone: %v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "size limit of 1 MiB") {
		t.Fatalf("log lacks explanation:\n%s", log.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("checkout left behind")
	}
	assertNoLeftovers(t, filepath.Join(base, "tmp"))

	// The same repository fits a larger limit.
	r2, base2 := g.runner(t, func(o *Options) { o.MaxRepoSize = 64 << 20 })
	if _, err := r2.Clone(context.Background(), Remote{URL: g.url("repo.git"), Auth: httpsAuth(srvPass)}, filepath.Join(base2, "repositories", "1"), &log); err != nil {
		t.Fatalf("clone under a larger limit: %v", err)
	}
}

// TestG3_PullSizeLimit: a fetch that pushes the checkout past the limit
// fails with ReasonSizeLimit, leaves the checkout at its previous commit
// and removes the partial download.
func TestG3_PullSizeLimit(t *testing.T) {
	g := newGitServer(t)
	r, base := g.runner(t, func(o *Options) { o.MaxRepoSize = 2 << 20; o.sizeInterval = 50 * time.Millisecond })
	dir := filepath.Join(base, "repositories", "1")
	var log bytes.Buffer
	ctx := context.Background()
	co, err := r.Clone(ctx, Remote{URL: g.url("repo.git"), Auth: httpsAuth(srvPass)}, dir, &log)
	if err != nil {
		t.Fatal(err)
	}
	g.commitRandom("big.dat", 4<<20)
	g.push()
	_, err = r.Pull(ctx, Remote{URL: g.url("repo.git"), Branch: "main", Auth: httpsAuth(srvPass)}, dir, &log)
	var ge *Error
	if !errors.As(err, &ge) || ge.Reason != ReasonSizeLimit || !ge.Unchanged {
		t.Fatalf("pull: %v\n%s", err, log.String())
	}
	if h, _ := r.HeadCommit(ctx, dir); h != co.Commit {
		t.Fatal("failed pull moved HEAD")
	}
	if tmp, _ := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "tmp_*")); len(tmp) != 0 {
		t.Fatalf("partial pack files left: %v", tmp)
	}
	assertNoLeftovers(t, filepath.Join(base, "tmp"))
}

// TestG3_FreeSpaceFloor: a clone is refused before git runs when the data
// filesystem has less free space than MinFreeSpace.
func TestG3_FreeSpaceFloor(t *testing.T) {
	g := newGitServer(t)
	r, base := g.runner(t, func(o *Options) { o.MinFreeSpace = 1 << 62 })
	if _, err := freeSpace(t.TempDir()); err != nil {
		t.Skipf("free space not available here: %v", err)
	}
	var log bytes.Buffer
	_, err := r.Clone(context.Background(), Remote{URL: g.url("repo.git"), Auth: httpsAuth(srvPass)}, filepath.Join(base, "repositories", "1"), &log)
	var ge *Error
	if !errors.As(err, &ge) || ge.Reason != ReasonDiskSpace || !ge.Unchanged {
		t.Fatalf("clone: %v", err)
	}
	g.mu.Lock()
	hits := g.goodAuth
	g.mu.Unlock()
	if hits != 0 || strings.Contains(log.String(), "$ git") {
		t.Fatal("git ran despite the free-space floor")
	}
}
