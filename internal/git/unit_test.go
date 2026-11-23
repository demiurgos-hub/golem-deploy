package git

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestMain lets the test binary act as the askpass helper, exactly like the
// golem-deploy binary does in production.
func TestMain(m *testing.M) {
	if code, ok := Askpass(os.Args, os.Getenv, os.Stdout); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

const plantedPassword = "Pl4nted-S3cret!value"

func TestAuthNeverFormatsSecrets(t *testing.T) {
	a := Auth{Method: AuthHTTPS, Username: "bob", Password: plantedPassword, PrivateKey: []byte("PRIVATE-KEY-BYTES")}
	r := Remote{URL: "https://h/x", Branch: "main", Auth: a}
	var logBuf bytes.Buffer
	slog.New(slog.NewTextHandler(&logBuf, nil)).Info("x", "auth", a, "remote", r)
	outputs := []string{
		fmt.Sprint(a), fmt.Sprintf("%v %+v %#v %s %q %x", a, a, a, a, a, a),
		fmt.Sprint(r), fmt.Sprintf("%v %+v %#v", r, r, r), a.String(), logBuf.String(),
	}
	for _, o := range outputs {
		if strings.Contains(o, plantedPassword) || strings.Contains(o, "PRIVATE-KEY") {
			t.Fatalf("secret leaked in %q", o)
		}
	}
	if !strings.Contains(fmt.Sprint(a), "bob") {
		t.Fatal("username missing from rendering")
	}
}

func genKey(t *testing.T, passphrase string) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var blk *pem.Block
	if passphrase == "" {
		blk, err = ssh.MarshalPrivateKey(priv, "test")
	} else {
		blk, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "test", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(blk))
}

func TestPrepareSSHKey(t *testing.T) {
	key := genKey(t, "")
	crlf := "\r\n  " + strings.ReplaceAll(strings.TrimSpace(key), "\n", "\r\n") + "  \r\n\r\n"
	got, err := PrepareSSHKey(crlf)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte("\r")) || !bytes.HasSuffix(got, []byte("-----\n")) || bytes.HasSuffix(got, []byte("\n\n")) {
		t.Fatalf("not normalised: %q", got)
	}
	if string(got) != strings.TrimSpace(key)+"\n" {
		t.Fatal("normalised key differs from the original")
	}
	if _, err := PrepareSSHKey(genKey(t, "hunter2")); !errors.Is(err, ErrKeyEncrypted) {
		t.Fatalf("encrypted key: %v", err)
	}
	for _, bad := range []string{"", "   ", "not a key", "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n", strings.Repeat("a", MaxKeyLength+1)} {
		if _, err := PrepareSSHKey(bad); err == nil {
			t.Fatalf("accepted %q", bad[:min(20, len(bad))])
		}
	}
}

func TestAskpass(t *testing.T) {
	env := map[string]string{
		AskpassEnv:         "1",
		askpassUsernameEnv: "alice",
		askpassPasswordEnv: plantedPassword,
		askpassHostEnv:     "git.example.com:8443",
	}
	getenv := func(k string) string { return env[k] }
	cases := []struct {
		prompt string
		want   string
		code   int
	}{
		{"Username for 'https://git.example.com:8443': ", "alice\n", 0},
		{"Password for 'https://alice@git.example.com:8443': ", plantedPassword + "\n", 0},
		{"Password for 'https://alice@GIT.example.com:8443': ", plantedPassword + "\n", 0},
		// Another host (e.g. after a redirect) gets nothing.
		{"Password for 'https://alice@evil.example': ", "", 1},
		{"Password for 'https://alice@git.example.com': ", "", 1},
		{"Password for 'http://alice@git.example.com:8443': ", "", 1},
		{"Are you sure you want to continue connecting (yes/no)? ", "", 1},
		{"Password for git.example.com:8443: ", "", 1},
	}
	for _, c := range cases {
		var out bytes.Buffer
		code, ok := Askpass([]string{"golem-deploy", c.prompt}, getenv, &out)
		if !ok || code != c.code || out.String() != c.want {
			t.Errorf("prompt %q: ok=%v code=%d out=%q", c.prompt, ok, code, out.String())
		}
	}
	if _, ok := Askpass([]string{"golem-deploy", "serve"}, func(string) string { return "" }, &bytes.Buffer{}); ok {
		t.Fatal("handled without the marker")
	}
	if code, ok := Askpass([]string{"golem-deploy"}, getenv, &bytes.Buffer{}); !ok || code != 1 {
		t.Fatal("missing prompt not rejected")
	}
	// No password configured: refuse rather than print an empty line.
	delete(env, askpassPasswordEnv)
	var out bytes.Buffer
	if code, _ := Askpass([]string{"x", "Password for 'https://alice@git.example.com:8443': "}, getenv, &out); code != 1 || out.Len() != 0 {
		t.Fatal("empty password answered")
	}
}

func TestScrubber(t *testing.T) {
	var out bytes.Buffer
	s := newScrubber(&out, []string{"p@ss word/x"})
	// Split the secret across writes and lines across \r and \n.
	chunks := []string{"fatal: unable to access 'https://bob:hunter", "2@host/x': p@ss ", "word/x and p%40ss+word%2Fx\r\n",
		"progress 10%\rprogress 100%\n", "ssh://git@host/x is fine\n", "tail without newline p@ss word/x"}
	for _, c := range chunks {
		if _, err := s.Write([]byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	s.Flush()
	got := out.String()
	for _, leak := range []string{"hunter2", "p@ss word/x", "p%40ss+word%2Fx"} {
		if strings.Contains(got, leak) {
			t.Fatalf("leak %q in %q", leak, got)
		}
	}
	for _, keep := range []string{"https://bob:***@host/x", "ssh://git@host/x is fine", "progress 100%", "tail without newline ***"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("missing %q in %q", keep, got)
		}
	}
	if !strings.Contains(s.Tail(), "tail without newline") {
		t.Fatal("tail not recorded")
	}

	// An overlong line is truncated without leaking a secret cut at the
	// truncation point.
	out.Reset()
	s = newScrubber(&out, []string{"SECRETSECRET"})
	long := strings.Repeat("a", maxLine-6) + "SECRETSECRET" + strings.Repeat("b", 100) + "\nnext\n"
	_, _ = s.Write([]byte(long))
	s.Flush()
	if strings.Contains(out.String(), "SECRET") || !strings.Contains(out.String(), "[line truncated]") || !strings.HasSuffix(out.String(), "next\n") {
		t.Fatalf("truncation wrong: ...%q", out.String()[max(0, out.Len()-80):])
	}
}

func TestClassify(t *testing.T) {
	for out, want := range map[string]Reason{
		"fatal: Authentication failed for 'https://h/x'":                              ReasonAuth,
		"fatal: could not read Username for 'https://h': terminal prompts disabled":   ReasonAuth,
		"git@h: Permission denied (publickey).\nfatal: Could not read from remote":    ReasonAuth,
		"remote: Repository not found.\nfatal: repository 'https://h/x/' not found":   ReasonNotFound,
		"fatal: Remote branch nope not found in upstream origin":                      ReasonBranchNotFound,
		"fatal: couldn't find remote ref refs/heads/nope":                             ReasonBranchNotFound,
		"Host key verification failed.\nfatal: Could not read from remote repository": ReasonHostKey,
		"fatal: unable to access 'https://h/': Could not resolve host: h":             ReasonNetwork,
		"fatal: unable to access 'https://h/': SSL certificate problem: self-signed":  ReasonTLS,
		"git: 'lfs' is not a git command. See 'git --help'.":                          ReasonLFSMissing,
		"something else": ReasonUnknown,
	} {
		if got := classify(out); got != want {
			t.Errorf("classify(%q) = %d, want %d", out, got, want)
		}
	}
	e := &Error{Op: "clone", Reason: ReasonAuth, Err: errors.New("git clone exited with status 128")}
	if SafeMessage(e) == "" || SafeMessage(errors.New("x")) == "" || !strings.Contains(e.Error(), "authentication") {
		t.Fatal("messages")
	}
}

// TestArgv pins the exact argument lists: hardening first, "--" or
// "--end-of-options" before every positional argument.
func TestArgv(t *testing.T) {
	r := &Runner{}
	r.lfsOnce.Do(func() {}) // LFS unavailable: no filter options
	wantPrefix := []string{
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "protocol.ssh.allow=always",
		"-c", "protocol.file.allow=never",
		"-c", "protocol.ext.allow=never",
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "credential.helper=",
		"-c", "submodule.recurse=false",
		"-c", "advice.detachedHead=false",
		"-c", "core.protectNTFS=true",
		"-c", "core.protectHFS=true",
		"-c", "gc.autoDetach=false",
		"-c", "maintenance.autoDetach=false",
	}
	got := r.argv([]string{"status"})
	if !reflect.DeepEqual(got, append(slices.Clone(wantPrefix), "status")) {
		t.Fatalf("argv:\n%q", got)
	}
	r2 := &Runner{}
	r2.lfsOnce.Do(func() { r2.lfsOK = true })
	if got := r2.argv(nil); !slices.Contains(got, "filter.lfs.process=git-lfs filter-process") || !slices.Contains(got, "filter.lfs.required=true") {
		t.Fatalf("lfs filter missing: %q", got)
	}

	golden := map[string][2][]string{
		"clone":         {cloneArgs("https://h/x", "main", "/d"), {"clone", "--origin=origin", "--no-recurse-submodules", "--branch=main", "--", "https://h/x", "/d"}},
		"clone-default": {cloneArgs("git@h:x", "", "/d"), {"clone", "--origin=origin", "--no-recurse-submodules", "--", "git@h:x", "/d"}},
		"set-url":       {setURLArgs("https://h/x"), {"remote", "set-url", "--end-of-options", "origin", "https://h/x"}},
		"fetch": {fetchArgs("feature/x"), {"fetch", "--prune", "--force", "--tags", "--no-recurse-submodules", "--end-of-options",
			"origin", "+refs/heads/feature/x:refs/remotes/origin/feature/x"}},
		"checkout":  {checkoutArgs("main"), {"checkout", "--force", "--no-track", "--no-recurse-submodules", "-B", "main", "refs/remotes/origin/main", "--"}},
		"clean":     {cleanArgs(), {"clean", "-ffdx", "--"}},
		"head":      {headArgs(), {"rev-parse", "--verify", "--end-of-options", "HEAD^{commit}"}},
		"branch":    {currentBranchArgs(), {"symbolic-ref", "--quiet", "--short", "HEAD"}},
		"ls-remote": {lsRemoteArgs("ssh://h/x"), {"ls-remote", "--heads", "--end-of-options", "ssh://h/x"}},
		"lfs":       {lfsPullArgs(), {"lfs", "pull"}},
		"attrs":     {attributesArgs(), {"ls-files", "-z", "--", ".gitattributes", ":(glob)**/.gitattributes"}},
	}
	for name, g := range golden {
		if !reflect.DeepEqual(g[0], g[1]) {
			t.Errorf("%s:\n got %q\nwant %q", name, g[0], g[1])
		}
	}
}

func TestSSHCommandQuoting(t *testing.T) {
	cmd := sshCommand(`/data dir/known_hosts`, `/tmp/it's here/id_key`)
	want := `ssh -F none -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o 'UserKnownHostsFile=/data dir/known_hosts' -o ConnectTimeout=30 -i '/tmp/it'\''s here/id_key'`
	if cmd != want {
		t.Fatalf("got  %s\nwant %s", cmd, want)
	}
	if strings.Contains(sshCommand("/k", ""), " -i ") {
		t.Fatal("-i without a key")
	}
	if runtime.GOOS == "windows" && sshPath(`C:\Program Files\x`) != "C:/Program Files/x" {
		t.Fatal("windows path not converted")
	}
}

func TestSessionEnvironment(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "with space")
	r, err := New(Options{
		GitPath: "git", AskpassPath: "/bin/askpass", TempDir: tmp, KnownHostsFile: filepath.Join(tmp, "known_hosts"),
		Environ: []string{"PATH=/usr/bin", "HOME=/home/daemon", "GOLEM_DEPLOY_SECRET=x", "GIT_DIR=/evil", "GIT_CONFIG_PARAMETERS='core.pager=evil'",
			"https_proxy=http://proxy:3128", "SSL_CERT_FILE=/etc/ca.pem", "SSH_AUTH_SOCK=/tmp/agent", "GIT_ASKPASS=/evil"},
	})
	if err != nil {
		t.Fatal(err)
	}
	envOf := func(s *session) map[string]string {
		m := map[string]string{}
		for _, kv := range s.env {
			k, v, _ := strings.Cut(kv, "=")
			if _, dup := m[k]; dup {
				t.Fatalf("duplicate %s", k)
			}
			m[k] = v
		}
		return m
	}

	s, err := r.begin(Remote{URL: "https://h.example:8443/x", Auth: Auth{Method: AuthHTTPS, Username: "u", Password: plantedPassword}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(s)
	for _, k := range []string{"GOLEM_DEPLOY_SECRET", "GIT_DIR", "GIT_CONFIG_PARAMETERS", "SSH_AUTH_SOCK"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s passed through", k)
		}
	}
	want := map[string]string{
		"PATH": "/usr/bin", "https_proxy": "http://proxy:3128", "SSL_CERT_FILE": "/etc/ca.pem",
		"HOME": s.dir, "GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull,
		"GIT_ASKPASS": "/bin/askpass", AskpassEnv: "1", askpassUsernameEnv: "u", askpassPasswordEnv: plantedPassword,
		askpassHostEnv: "h.example:8443", "LC_ALL": "C",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if !strings.Contains(env["GIT_SSH_COMMAND"], "StrictHostKeyChecking=accept-new") {
		t.Error("ssh command missing")
	}
	dir := s.dir
	s.close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("session dir not removed")
	}

	// SSH: key in a 0600 file inside the session dir, removed on close; no
	// askpass variables.
	key, err := PrepareSSHKey(genKey(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	s, err = r.begin(Remote{URL: "git@h:x", Auth: Auth{Method: AuthSSH, PrivateKey: key}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	env = envOf(s)
	keyFile := filepath.Join(s.dir, "id_key")
	data, err := os.ReadFile(keyFile)
	if err != nil || !bytes.Equal(data, key) {
		t.Fatalf("key file: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(keyFile)
		di, _ := os.Stat(s.dir)
		if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
			t.Fatalf("modes %v %v", fi.Mode().Perm(), di.Mode().Perm())
		}
	}
	if !strings.Contains(env["GIT_SSH_COMMAND"], shQuote(sshPath(keyFile))) {
		t.Errorf("ssh command lacks key: %s", env["GIT_SSH_COMMAND"])
	}
	if _, ok := env["GIT_ASKPASS"]; ok {
		t.Error("askpass set for ssh")
	}
	s.close()
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Fatal("key file not removed")
	}
}

func TestParseLsRemote(t *testing.T) {
	h := strings.Repeat("a", 40)
	in := h + "\trefs/heads/main\n" + h + "\trefs/heads/feature/x\n" + h + "\trefs/tags/v1\n" + h + "\trefs/heads/--evil\n" +
		"junk\n" + h + "\trefs/heads/main\n" + strings.Repeat("b", 64) + "\trefs/heads/sha256\n"
	got := parseLsRemote([]byte(in))
	if !reflect.DeepEqual(got, []string{"feature/x", "main", "sha256"}) {
		t.Fatalf("%q", got)
	}
}

func TestCheckoutsHousekeeping(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repositories")
	tmp := filepath.Join(base, "tmp")
	for _, d := range []string{"1", "2", "07", "notes", "3"} {
		if err := os.MkdirAll(filepath.Join(root, d, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A read-only file (git object files are read-only) must not block
	// removal.
	ro := filepath.Join(root, "2", ".git", "obj")
	if err := os.WriteFile(ro, []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if CheckoutDir(root, 2) != filepath.Join(root, "2") {
		t.Fatal("CheckoutDir")
	}
	if err := RemoveCheckout(root, 2, tmp); err != nil {
		t.Fatal(err)
	}
	if HasCheckout(filepath.Join(root, "2")) {
		t.Fatal("checkout still present")
	}
	if err := RemoveCheckout(root, 2, tmp); err != nil {
		t.Fatalf("missing checkout: %v", err)
	}
	if err := RemoveCheckout(root, 0, tmp); err == nil {
		t.Fatal("id 0 accepted")
	}
	if err := RemoveCheckout(root, -1, tmp); err == nil {
		t.Fatal("negative id accepted")
	}
	removed, err := SweepCheckouts(root, tmp, func(id int64) bool { return id == 1 })
	if err != nil || !reflect.DeepEqual(removed, []int64{3}) {
		t.Fatalf("sweep: %v %v", removed, err)
	}
	for _, keep := range []string{"1", "07", "notes"} {
		if _, err := os.Stat(filepath.Join(root, keep)); err != nil {
			t.Fatalf("%s removed", keep)
		}
	}
	for _, n := range []string{"git-abc", "clone-1", "remove-2", "unrelated"} {
		_ = os.MkdirAll(filepath.Join(tmp, n), 0o700)
	}
	if err := SweepTemp(tmp); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(tmp)
	if len(entries) != 1 || entries[0].Name() != "unrelated" {
		t.Fatalf("sweep temp left %v", entries)
	}
}
