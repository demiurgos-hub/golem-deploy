package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Default timeouts.
const (
	DefaultNetworkTimeout = 15 * time.Minute
	DefaultLocalTimeout   = 60 * time.Second
	// waitDelay bounds how long Wait waits for output pipes held open by
	// grandchildren after git itself exited or was killed.
	waitDelay = 5 * time.Second
)

// Options configures a Runner.
type Options struct {
	// GitPath is the git executable. Default: "git" looked up on PATH.
	GitPath string
	// AskpassPath is the executable git runs as GIT_ASKPASS; it must call
	// Askpass at startup. Default: os.Executable() (the golem-deploy binary).
	AskpassPath string
	// TempDir holds per-operation scratch directories (private HOME, SSH
	// key, clone staging). It must be on the same filesystem as the
	// checkouts so a finished clone can be renamed into place. Required.
	TempDir string
	// KnownHostsFile is the SSH known_hosts file (StrictHostKeyChecking is
	// accept-new: unknown hosts are recorded on first use, changed keys are
	// rejected). Required.
	KnownHostsFile string
	// NetworkTimeout bounds each command that talks to a remote (clone,
	// fetch, ls-remote, lfs pull). Default DefaultNetworkTimeout.
	NetworkTimeout time.Duration
	// LocalTimeout bounds each local command. Default DefaultLocalTimeout.
	LocalTimeout time.Duration
	// Environ is the environment the allowlisted variables are taken from.
	// Default os.Environ().
	Environ []string
}

// Runner runs hardened, non-interactive git commands. It is safe for
// concurrent use; callers serialise operations on the same checkout.
type Runner struct {
	git            string
	askpass        string
	tmp            string
	knownHosts     string
	networkTimeout time.Duration
	localTimeout   time.Duration
	baseEnv        []string

	lfsOnce sync.Once
	lfsOK   bool
}

// New validates o and returns a Runner.
func New(o Options) (*Runner, error) {
	if o.TempDir == "" || o.KnownHostsFile == "" {
		return nil, errors.New("git: TempDir and KnownHostsFile are required")
	}
	if o.GitPath == "" {
		p, err := exec.LookPath("git")
		if err != nil {
			return nil, &Error{Op: "init", Reason: ReasonGitMissing, Err: err}
		}
		o.GitPath = p
	}
	if o.AskpassPath == "" {
		p, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("git: locate askpass executable: %w", err)
		}
		o.AskpassPath = p
	}
	if o.NetworkTimeout <= 0 {
		o.NetworkTimeout = DefaultNetworkTimeout
	}
	if o.LocalTimeout <= 0 {
		o.LocalTimeout = DefaultLocalTimeout
	}
	if o.Environ == nil {
		o.Environ = os.Environ()
	}
	tmp, err := filepath.Abs(o.TempDir)
	if err != nil {
		return nil, fmt.Errorf("git: temp dir: %w", err)
	}
	kh, err := filepath.Abs(o.KnownHostsFile)
	if err != nil {
		return nil, fmt.Errorf("git: known_hosts: %w", err)
	}
	return &Runner{
		git:            o.GitPath,
		askpass:        o.AskpassPath,
		tmp:            tmp,
		knownHosts:     kh,
		networkTimeout: o.NetworkTimeout,
		localTimeout:   o.LocalTimeout,
		baseEnv:        filterEnv(o.Environ, runtime.GOOS == "windows"),
	}, nil
}

// hardening are the -c options given to every git command.
func hardening() []string {
	return []string{
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
}

// lfsFilter configures the Git LFS filter per command, because the global
// and system config files (where "git lfs install" puts it) are ignored.
func lfsFilter() []string {
	return []string{
		"-c", "filter.lfs.process=git-lfs filter-process",
		"-c", "filter.lfs.smudge=git-lfs smudge -- %f",
		"-c", "filter.lfs.clean=git-lfs clean -- %f",
		"-c", "filter.lfs.required=true",
	}
}

// argv returns the arguments after the git executable for args.
func (r *Runner) argv(args []string) []string {
	out := hardening()
	if r.lfsAvailable() {
		out = append(out, lfsFilter()...)
	}
	return append(out, args...)
}

// lfsAvailable reports whether "git lfs" works; probed once per Runner.
func (r *Runner) lfsAvailable() bool {
	r.lfsOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), r.localTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, r.git, "lfs", "version")
		cmd.Env = append(append([]string{}, r.baseEnv...), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		cmd.Dir = r.tmp
		r.lfsOK = cmd.Run() == nil
	})
	return r.lfsOK
}

// allowedEnv reports whether variable key (upper case on Windows) is
// passed from the daemon's environment to git: PATH, proxy settings, TLS
// trust settings and, on Windows, the system variables Git for Windows and
// its MSYS runtime need.
func allowedEnv(key string, windows bool) bool {
	switch key {
	case "PATH", "SSL_CERT_FILE", "SSL_CERT_DIR", "GIT_SSL_CAINFO", "GIT_SSL_CAPATH":
		return true
	}
	switch strings.ToUpper(key) {
	case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY":
		return true
	}
	if !windows {
		return false
	}
	switch key {
	case "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT", "TEMP", "TMP",
		"PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMW6432", "COMMONPROGRAMFILES",
		"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE", "OS":
		return true
	}
	return false
}

func filterEnv(src []string, windows bool) []string {
	out := []string{}
	for _, kv := range src {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if windows {
			k = strings.ToUpper(k)
		}
		if allowedEnv(k, windows) {
			out = append(out, kv)
		}
	}
	return out
}

// shQuote quotes s for the POSIX shell git uses to run GIT_SSH_COMMAND.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// sshPath converts a path for the ssh command line (forward slashes work
// for both native and MSYS ssh on Windows).
func sshPath(p string) string { return filepath.ToSlash(p) }

// sshCommand returns GIT_SSH_COMMAND. keyFile may be empty.
func sshCommand(knownHosts, keyFile string) string {
	parts := []string{
		"ssh", "-F", "none",
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", shQuote("UserKnownHostsFile=" + sshPath(knownHosts)),
		"-o", "ConnectTimeout=30",
	}
	if keyFile != "" {
		parts = append(parts, "-i", shQuote(sshPath(keyFile)))
	}
	return strings.Join(parts, " ")
}

// session is the per-operation state: a private scratch directory (HOME
// and SSH key), the environment and the scrubbing log writer.
type session struct {
	r   *Runner
	dir string
	env []string
	log *scrubber
}

// begin creates the session for remote, writing output to log.
func (r *Runner) begin(remote Remote, log io.Writer) (*session, error) {
	if log == nil {
		log = io.Discard
	}
	if err := os.MkdirAll(r.tmp, 0o700); err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	dir, err := os.MkdirTemp(r.tmp, "git-")
	if err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	s := &session{r: r, dir: dir, log: newScrubber(log, remote.Auth.secrets())}
	keyFile := ""
	if remote.Auth.Method == AuthSSH && len(remote.Auth.PrivateKey) > 0 {
		keyFile = filepath.Join(dir, "id_key")
		if err := os.WriteFile(keyFile, remote.Auth.PrivateKey, 0o600); err != nil {
			s.close()
			return nil, fmt.Errorf("write ssh key: %w", err)
		}
	}
	env := append([]string{}, r.baseEnv...)
	env = append(env,
		"HOME="+dir,
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_SSH_COMMAND="+sshCommand(r.knownHosts, keyFile),
		"GIT_SSH_VARIANT=ssh",
	)
	if runtime.GOOS == "windows" {
		env = append(env, "USERPROFILE="+dir)
	} else {
		env = append(env, "TMPDIR="+dir)
	}
	if remote.Auth.Method == AuthHTTPS {
		env = append(env,
			"GIT_ASKPASS="+r.askpass,
			AskpassEnv+"=1",
			askpassUsernameEnv+"="+remote.Auth.Username,
			askpassPasswordEnv+"="+remote.Auth.Password,
			askpassHostEnv+"="+URLHost(remote.URL),
		)
	}
	s.env = env
	return s, nil
}

// close removes the session directory (and with it the SSH key).
func (s *session) close() {
	s.log.Flush()
	_ = os.RemoveAll(s.dir)
}

// cmdOpts tunes one command.
type cmdOpts struct {
	network bool
	// stdout captures standard output instead of logging it.
	stdout io.Writer
	// quiet suppresses the "$ git ..." log line.
	quiet bool
	// lfsSmudge lets the LFS filter download content (only "git lfs pull").
	lfsSmudge bool
}

// run executes git args in dir. Failures are *Error with a Reason derived
// from the context and the command's (scrubbed) output.
func (s *session) run(ctx context.Context, op, dir string, o cmdOpts, args ...string) error {
	timeout := s.r.localTimeout
	if o.network {
		timeout = s.r.networkTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if !o.quiet {
		s.log.println("$ git " + strings.Join(args, " "))
	}
	s.log.resetTail()

	cmd := exec.CommandContext(cctx, s.r.git, s.r.argv(args)...)
	cmd.Dir = dir
	env := s.env
	if !o.lfsSmudge {
		env = append(append([]string{}, env...), "GIT_LFS_SKIP_SMUDGE=1")
	}
	cmd.Env = env
	cmd.Stdout = s.log
	if o.stdout != nil {
		cmd.Stdout = o.stdout
	}
	cmd.Stderr = s.log
	cmd.WaitDelay = waitDelay
	tree := newProcTree(cmd)
	if err := cmd.Start(); err != nil {
		tree.close()
		reason := ReasonUnknown
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			reason = ReasonGitMissing
		}
		return &Error{Op: op, Reason: reason, Err: fmt.Errorf("start git: %w", err)}
	}
	tree.started(cmd)
	err := cmd.Wait()
	tree.close()
	s.log.Flush()
	if err == nil {
		return nil
	}
	reason := classify(s.log.Tail())
	switch {
	case ctx.Err() != nil:
		reason = ReasonCancelled
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		reason = ReasonTimeout
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		err = fmt.Errorf("git %s exited with status %d", firstNonOption(args), ee.ExitCode())
	} else {
		err = errors.New(s.log.scrubString(err.Error()))
	}
	return &Error{Op: op, Reason: reason, Err: err}
}

func firstNonOption(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}
