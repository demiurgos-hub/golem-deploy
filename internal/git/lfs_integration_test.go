package git

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The tests in this file reproduce the review finding that a repository's
// .lfsconfig decided where "git lfs pull" connects (G1). They need git-lfs.

func requireLFS(t *testing.T) {
	t.Helper()
	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		t.Skip("git-lfs is not installed; skipping Git LFS integration test")
	}
}

// serveLFSBatch implements the download half of the Git LFS batch API:
// every known object gets a basic-transfer download action on this server.
func (g *gitServer) serveLFSBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Operation string `json:"operation"`
		Objects   []struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"objects"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Operation != "download" {
		http.Error(w, "bad batch request", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.lfsBatches = append(g.lfsBatches, r.URL.Path)
	objects := g.lfsObjects
	g.mu.Unlock()
	type action struct {
		Href string `json:"href"`
	}
	type object struct {
		OID     string            `json:"oid"`
		Size    int64             `json:"size"`
		Actions map[string]action `json:"actions,omitempty"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	resp := struct {
		Transfer string   `json:"transfer"`
		Objects  []object `json:"objects"`
	}{Transfer: "basic"}
	for _, o := range req.Objects {
		obj := object{OID: o.OID, Size: o.Size}
		if _, ok := objects[o.OID]; ok {
			obj.Actions = map[string]action{"download": {Href: g.srv.URL + "/lfs-obj/" + o.OID}}
		} else {
			obj.Error = &struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}{404, "not found"}
		}
		resp.Objects = append(resp.Objects, obj)
	}
	w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (g *gitServer) serveLFSObject(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	data, ok := g.lfsObjects[strings.TrimPrefix(r.URL.Path, "/lfs-obj/")]
	g.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(data)
}

// commitLFS commits name as an LFS pointer to random content of size bytes
// (registered with the LFS server) and returns the content.
func (g *gitServer) commitLFS(name string, size int) []byte {
	data := make([]byte, size)
	_, _ = rand.Read(data)
	sum := sha256.Sum256(data)
	oid := hex.EncodeToString(sum[:])
	g.mu.Lock()
	if g.lfsObjects == nil {
		g.lfsObjects = map[string][]byte{}
	}
	g.lfsObjects[oid] = data
	g.mu.Unlock()
	if _, err := os.Stat(filepath.Join(g.work, ".gitattributes")); err != nil {
		g.commit(".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	}
	g.commit(name, fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, size))
	return data
}

func (g *gitServer) batches() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.lfsBatches...)
}

// trap is a plain-HTTP server and a raw TCP listener (standing in for an
// SSH server) that count every request or connection they receive.
type trap struct {
	http     *httptest.Server
	httpHits atomic.Int32
	tcp      net.Listener
	tcpConns atomic.Int32
	wg       sync.WaitGroup
}

func newTrap(t *testing.T) *trap {
	tr := &trap{}
	tr.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.httpHits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
		_, _ = w.Write([]byte(`{"objects":[]}`))
	}))
	t.Cleanup(tr.http.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tr.tcp = ln
	tr.wg.Add(1)
	go func() {
		defer tr.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			tr.tcpConns.Add(1)
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close(); tr.wg.Wait() })
	return tr
}

func (tr *trap) sshURL() string { return "ssh://git@" + tr.tcp.Addr().String() + "/x.git" }

func (tr *trap) assertUntouched(t *testing.T) {
	t.Helper()
	if n := tr.httpHits.Load(); n != 0 {
		t.Errorf("plain-HTTP LFS server named in config received %d request(s)", n)
	}
	if n := tr.tcpConns.Load(); n != 0 {
		t.Errorf("SSH endpoint named in config received %d connection(s)", n)
	}
}

// TestG1_LFSWorksAtDerivedEndpoint: LFS content is fetched from the
// endpoint derived from the project's URL, with and without ".git".
func TestG1_LFSWorksAtDerivedEndpoint(t *testing.T) {
	requireLFS(t)
	g := newGitServer(t)
	content := g.commitLFS("asset.bin", 4096)
	g.push()
	for i, repo := range []string{"repo.git", "repo"} {
		r, base := g.runner(t)
		dir := filepath.Join(base, "repositories", "1")
		var log bytes.Buffer
		co, err := r.Clone(context.Background(), Remote{URL: g.url(repo), Auth: httpsAuth(srvPass)}, dir, &log)
		if err != nil {
			t.Fatalf("clone %s: %v\n%s", repo, err, log.String())
		}
		if !co.LFS {
			t.Fatal("LFS not reported")
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "asset.bin")); !bytes.Equal(got, content) {
			t.Fatalf("asset.bin is not the LFS content (%d bytes)", len(got))
		}
		b := g.batches()
		if len(b) != i+1 || b[i] != "/git/repo.git/info/lfs/objects/batch" {
			t.Fatalf("batch requests %q", b)
		}
		if strings.Contains(log.String(), srvPass) {
			t.Fatal("password in log")
		}
		assertNoLeftovers(t, filepath.Join(base, "tmp"))
	}
}

// TestG1_LFSConfigEndpointRefused reproduces the reviewer's experiments end
// to end: an .lfsconfig naming a plain-HTTP LFS server, or an SSH endpoint
// in an HTTPS project, fails the job with a clear message and neither
// endpoint ever receives a request.
func TestG1_LFSConfigEndpointRefused(t *testing.T) {
	requireLFS(t)
	tr := newTrap(t)
	for name, cfg := range map[string]string{
		"plain http lfs.url":      "[lfs]\n\turl = " + tr.http.URL + "/x/info/lfs\n",
		"ssh remote lfsurl":       "[remote \"origin\"]\n\tlfsurl = " + tr.sshURL() + "\n",
		"insteadOf to plain http": "[url \"" + tr.http.URL + "/\"]\n\tinsteadOf = https://127.0.0.1\n",
	} {
		t.Run(name, func(t *testing.T) {
			g := newGitServer(t)
			g.commitLFS("asset.bin", 64)
			g.commit(".lfsconfig", cfg)
			g.push()
			r, base := g.runner(t)
			dir := filepath.Join(base, "repositories", "1")
			var log bytes.Buffer
			_, err := r.Clone(context.Background(), Remote{URL: g.url("repo.git"), Auth: httpsAuth(srvPass)}, dir, &log)
			var ge *Error
			if !errors.As(err, &ge) || ge.Reason != ReasonLFSConfig || !ge.Unchanged {
				t.Fatalf("clone: %v\n%s", err, log.String())
			}
			if !strings.Contains(ge.Message(), ".lfsconfig") || !strings.Contains(log.String(), ".lfsconfig sets") {
				t.Fatalf("unclear failure: %s\n%s", ge.Message(), log.String())
			}
			if len(g.batches()) != 0 {
				t.Fatal("git lfs pull ran")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("checkout left behind")
			}
			assertNoLeftovers(t, filepath.Join(base, "tmp"))
		})
	}
	tr.assertUntouched(t)
}

// TestG1_LFSPinOutranksRepositoryConfig: with the refusal disabled, the
// command-line pin alone keeps git-lfs on the derived endpoint, whatever
// .lfsconfig (lfs.url to plain HTTP, remote.origin.lfsurl to SSH) and
// .git/config (lfs.url) say.
func TestG1_LFSPinOutranksRepositoryConfig(t *testing.T) {
	requireLFS(t)
	tr := newTrap(t)
	g := newGitServer(t)
	content := g.commitLFS("asset.bin", 2048)
	g.commit(".lfsconfig", "[lfs]\n\turl = "+tr.http.URL+"/x/info/lfs\n\tpushurl = "+tr.http.URL+"/y\n"+
		"[remote \"origin\"]\n\tlfsurl = "+tr.sshURL()+"\n")
	g.push()
	r, base := g.runner(t, func(o *Options) { o.skipLFSConfigCheck = true })
	dir := filepath.Join(base, "repositories", "1")
	var log bytes.Buffer
	ctx := context.Background()
	if _, err := r.Clone(ctx, Remote{URL: g.url("repo.git"), Auth: httpsAuth(srvPass)}, dir, &log); err != nil {
		t.Fatalf("clone: %v\n%s", err, log.String())
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "asset.bin")); !bytes.Equal(got, content) {
		t.Fatal("LFS content not fetched from the derived endpoint")
	}

	// The repository's own .git/config (not attacker-controlled in
	// practice, but it must not win either) and an SSH .lfsconfig.
	fixtureGit(t, dir, "config", "lfs.url", tr.http.URL+"/from-git-config")
	content2 := g.commitLFS("second.bin", 1024)
	g.commit(".lfsconfig", "[lfs]\n\turl = "+tr.sshURL()+"\n")
	g.push()
	if _, err := r.Pull(ctx, Remote{URL: g.url("repo.git"), Branch: "main", Auth: httpsAuth(srvPass)}, dir, &log); err != nil {
		t.Fatalf("pull: %v\n%s", err, log.String())
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "second.bin")); !bytes.Equal(got, content2) {
		t.Fatal("LFS content not fetched on pull")
	}
	for _, b := range g.batches() {
		if b != "/git/repo.git/info/lfs/objects/batch" {
			t.Fatalf("batch at %q", b)
		}
	}
	tr.assertUntouched(t)
}

// TestG1_NegativeControlWithoutPin shows that the trap above detects the
// vulnerability: with the pin and the refusal both disabled, git-lfs
// follows .lfsconfig to the plain-HTTP server. (Bounded by a short
// network timeout: git-lfs may retry against the trap.)
func TestG1_NegativeControlWithoutPin(t *testing.T) {
	requireLFS(t)
	tr := newTrap(t)
	g := newGitServer(t)
	g.commitLFS("asset.bin", 64)
	g.commit(".lfsconfig", "[lfs]\n\turl = "+tr.http.URL+"/x/info/lfs\n")
	g.push()
	r, base := g.runner(t, func(o *Options) {
		o.skipLFSConfigCheck = true
		o.noLFSPin = true
		o.NetworkTimeout = 15 * time.Second
	})
	var log bytes.Buffer
	_, _ = r.Clone(context.Background(), Remote{URL: g.url("repo.git"), Auth: httpsAuth(srvPass)}, filepath.Join(base, "repositories", "1"), &log)
	if tr.httpHits.Load() == 0 {
		t.Fatalf("without the pin, the .lfsconfig endpoint was not contacted; the trap tests prove nothing\n%s", log.String())
	}
}
