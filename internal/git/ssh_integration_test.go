package git

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// sshGitServer is an in-process SSH server (golang.org/x/crypto/ssh) that
// serves "git-upload-pack '<path>'" from a directory of bare repositories.
// It records the public keys clients offer and the session requests they
// make.
type sshGitServer struct {
	t       *testing.T
	ln      net.Listener
	root    string
	hostKey ssh.Signer
	allowed ssh.PublicKey

	mu       sync.Mutex
	offered  []string // marshalled public keys offered by clients
	requests []string // channel request types (and env names)
	wg       sync.WaitGroup
}

func newSSHGitServer(t *testing.T, root string, allowed ssh.PublicKey) *sshGitServer {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &sshGitServer{t: t, ln: ln, root: root, hostKey: hostKey, allowed: allowed}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			s.mu.Lock()
			s.offered = append(s.offered, string(key.Marshal()))
			s.mu.Unlock()
			if bytes.Equal(key.Marshal(), s.allowed.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("key not allowed")
		},
	}
	cfg.AddHostKey(hostKey)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serveConn(c, cfg)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	return s
}

func (s *sshGitServer) addr() string { return s.ln.Addr().String() }

func (s *sshGitServer) record(req string) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
}

func (s *sshGitServer) serveConn(c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go func() {
		for r := range reqs {
			s.record("global:" + r.Type)
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
	}()
	for nc := range chans {
		if nc.ChannelType() != "session" {
			s.record("channel:" + nc.ChannelType())
			_ = nc.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		s.serveSession(ch, creqs)
	}
}

func (s *sshGitServer) serveSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	var env []string
	for r := range reqs {
		switch r.Type {
		case "env":
			var kv struct{ Name, Value string }
			_ = ssh.Unmarshal(r.Payload, &kv)
			s.record("env:" + kv.Name)
			if kv.Name == "GIT_PROTOCOL" {
				env = append(env, "GIT_PROTOCOL="+kv.Value)
			}
			_ = r.Reply(true, nil)
		case "exec":
			var cmd struct{ Command string }
			_ = ssh.Unmarshal(r.Payload, &cmd)
			s.record("exec")
			_ = r.Reply(true, nil)
			code := s.exec(ch, cmd.Command, env)
			status := make([]byte, 4)
			binary.BigEndian.PutUint32(status, uint32(code))
			_, _ = ch.SendRequest("exit-status", false, status)
			return
		default:
			s.record(r.Type)
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
	}
}

// exec runs "git-upload-pack '<path>'" against the repository root.
func (s *sshGitServer) exec(ch ssh.Channel, command string, env []string) int {
	rest, ok := strings.CutPrefix(command, "git-upload-pack ")
	if !ok {
		_, _ = io.WriteString(ch.Stderr(), "unsupported command\n")
		return 1
	}
	repo := strings.Trim(rest, "'")
	if strings.Contains(repo, "..") {
		return 1
	}
	cmd := exec.Command("git", "upload-pack", "--strict", filepath.Join(s.root, filepath.FromSlash(strings.TrimPrefix(repo, "/"))))
	cmd.Env = append(append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull), env...)
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 1
	}
	if err := cmd.Start(); err != nil {
		return 1
	}
	go func() {
		_, _ = io.Copy(stdin, ch)
		stdin.Close()
	}()
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 1
	}
	return 0
}

func (s *sshGitServer) snapshot() (offered, requests []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.offered...), append([]string{}, s.requests...)
}

func requireSSH(t *testing.T) {
	t.Helper()
	requireGit(t)
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not on PATH; skipping")
	}
}

// TestG5_SSHConfigPathsWithSpaceAndPercent completes real SSH handshakes
// (clone, then pull) from a data directory whose path contains a space and
// a '%' token. The host key must land in <data>/known_hosts and nowhere
// else, and known_hosts files in the working directory (the checkout,
// which repository content controls) must never be read: they mark the
// server's key as revoked, which would fail the pull.
//
// It also checks G6: only the project key is offered (never the daemon
// user's ~/.ssh keys), and no agent forwarding or environment beyond
// GIT_PROTOCOL is requested.
func TestG5_SSHConfigPathsWithSpaceAndPercent(t *testing.T) {
	requireSSH(t)
	keyPEM := genKey(t, "")
	key, err := PrepareSSHKey(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	top := t.TempDir()
	srvRoot := filepath.Join(top, "srv")
	work := filepath.Join(top, "work")
	if err := os.MkdirAll(srvRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	srv := newSSHGitServer(t, srvRoot, signer.PublicKey())
	host, port, _ := net.SplitHostPort(srv.addr())
	hostPattern := "[" + host + "]:" + port
	revoked := "@revoked " + hostPattern + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(srv.hostKey.PublicKey()))) + "\n"

	fixtureGit(t, top, "init", "-q", "--bare", "-b", "main", filepath.Join(srvRoot, "repo.git"))
	fixtureGit(t, top, "init", "-q", "-b", "main", work)
	// Files a split or relative UserKnownHostsFile would resolve to, relative
	// to the working directory of a pull (the checkout).
	for _, name := range []string{"known_hosts", "dir/known_hosts", "dir %d/known_hosts", "%d/known_hosts"} {
		p := filepath.Join(work, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(revoked), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixtureGit(t, work, "add", "-A")
	fixtureGit(t, work, "commit", "-q", "-m", "planted known_hosts")
	fixtureGit(t, work, "push", "-q", filepath.Join(srvRoot, "repo.git"), "main")

	data := filepath.Join(top, "data dir %d")
	r, err := New(Options{TempDir: filepath.Join(data, "tmp"), KnownHostsFile: filepath.Join(data, "known_hosts")})
	if err != nil {
		t.Fatal(err)
	}
	remote := Remote{URL: "ssh://git@" + srv.addr() + "/repo.git", Auth: Auth{Method: AuthSSH, PrivateKey: key}}
	dir := filepath.Join(data, "repositories", "1")
	var log bytes.Buffer
	ctx := context.Background()
	if _, err := r.Clone(ctx, remote, dir, &log); err != nil {
		t.Fatalf("clone over ssh: %v\n%s", err, log.String())
	}
	kh, err := os.ReadFile(filepath.Join(data, "known_hosts"))
	if err != nil || !strings.Contains(string(kh), hostPattern+" ssh-ed25519 ") {
		t.Fatalf("host key not recorded in <data>/known_hosts: %v\n%s", err, kh)
	}

	head := g2commit(t, work, srvRoot)
	remote.Branch = "main"
	co, err := r.Pull(ctx, remote, dir, &log)
	if err != nil {
		t.Fatalf("pull over ssh (cwd = checkout with planted known_hosts): %v\n%s", err, log.String())
	}
	if co.Commit != head {
		t.Fatalf("pulled %s, want %s", co.Commit, head)
	}
	branches, err := r.ListRemoteBranches(ctx, Remote{URL: remote.URL, Auth: remote.Auth})
	if err != nil || strings.Join(branches, ",") != "main" {
		t.Fatalf("ls-remote: %q %v", branches, err)
	}

	// Exactly one known_hosts was written: <data>/known_hosts. The planted
	// ones are unchanged and nothing else appeared anywhere below top.
	err = filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "known_hosts" {
			return nil
		}
		if p == filepath.Join(data, "known_hosts") {
			return nil
		}
		b, _ := os.ReadFile(p)
		if string(b) != revoked {
			t.Errorf("unexpected or modified known_hosts at %s:\n%s", p, b)
		}
		rel, _ := filepath.Rel(top, p)
		if !strings.HasPrefix(rel, "work") && !strings.HasPrefix(rel, filepath.Join("data dir %d", "repositories")) && !strings.HasPrefix(rel, "srv") {
			t.Errorf("known_hosts written outside the data dir: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(kh), "\n") != 1 {
		t.Errorf("known_hosts has %d lines:\n%s", strings.Count(string(kh), "\n"), kh)
	}

	offered, requests := srv.snapshot()
	for _, k := range offered {
		if k != string(signer.PublicKey().Marshal()) {
			t.Errorf("a key other than the project key was offered")
		}
	}
	if len(offered) == 0 {
		t.Error("no key offered")
	}
	for _, req := range requests {
		switch req {
		case "exec", "env:GIT_PROTOCOL":
		default:
			t.Errorf("unexpected client request %q", req)
		}
	}
	if strings.Contains(log.String(), "data dir") {
		t.Errorf("log reveals data-dir paths:\n%s", log.String())
	}
	assertNoLeftovers(t, filepath.Join(data, "tmp"))
}

// g2commit adds a commit in work, pushes it to srvRoot/repo.git and returns
// its hash.
func g2commit(t *testing.T, work, srvRoot string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, "next.txt"), []byte("next\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, work, "add", "-A")
	fixtureGit(t, work, "commit", "-q", "-m", "next")
	fixtureGit(t, work, "push", "-q", filepath.Join(srvRoot, "repo.git"), "main")
	return fixtureGit(t, work, "rev-parse", "HEAD")
}
