package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Default timeouts and limits.
const (
	DefaultNetworkTimeout = 15 * time.Minute
	DefaultLocalTimeout   = 60 * time.Second
	// DefaultMinFreeSpace is the free space a clone, pull or branch switch
	// requires on the data filesystem before it starts.
	DefaultMinFreeSpace = 512 << 20
	// waitDelay bounds how long Wait waits for output pipes held open by
	// grandchildren after git itself exited or was killed.
	waitDelay = 5 * time.Second
	// sizeCheckInterval is how often the size watchdog samples a checkout.
	sizeCheckInterval = 2 * time.Second
)

// Options configures a Runner.
type Options struct {
	// GitPath is the git executable. Default: "git" looked up on PATH.
	GitPath string
	// AskpassPath is the executable git runs as GIT_ASKPASS; it must call
	// Askpass at startup. Default: os.Executable() (the golem-deploy binary).
	AskpassPath string
	// TempDir holds per-operation scratch directories (private HOME, SSH
	// key and config, clone staging). It must be on the same filesystem as
	// the checkouts so a finished clone can be renamed into place. Required;
	// its absolute path must pass CheckSSHConfigPath.
	TempDir string
	// KnownHostsFile is the SSH known_hosts file (StrictHostKeyChecking is
	// accept-new: unknown hosts are recorded on first use, changed keys are
	// rejected). Required; its absolute path must pass CheckSSHConfigPath.
	KnownHostsFile string
	// NetworkTimeout bounds each command that talks to a remote (clone,
	// fetch, ls-remote, lfs pull). Default DefaultNetworkTimeout.
	NetworkTimeout time.Duration
	// LocalTimeout bounds each local command. Default DefaultLocalTimeout.
	LocalTimeout time.Duration
	// Environ is the environment the allowlisted variables are taken from.
	// Default os.Environ().
	Environ []string
	// MaxRepoSize bounds, in bytes, the size of a checkout (working tree
	// and .git, LFS content included) while clone, fetch, checkout and
	// "git lfs pull" write it: a watchdog samples the directory every few
	// seconds and after each of these commands, and fails the operation
	// with ReasonSizeLimit. 0 means unlimited.
	MaxRepoSize int64
	// MinFreeSpace is the free space, in bytes, the TempDir filesystem must
	// have before a clone, pull or branch switch starts (ReasonDiskSpace
	// otherwise). 0 means DefaultMinFreeSpace; negative disables the check.
	MinFreeSpace int64

	sizeInterval       time.Duration // tests only; 0 means sizeCheckInterval
	skipLFSConfigCheck bool          // tests only: exercise the LFS endpoint pin alone
	noLFSPin           bool          // tests only: negative control for the pin
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
	maxSize        int64
	minFree        int64
	sizeInterval   time.Duration
	skipLFSCheck   bool
	noLFSPin       bool

	lfsOnce sync.Once
	lfsOK   bool
}

// New validates o and returns a Runner.
func New(o Options) (*Runner, error) {
	if o.TempDir == "" || o.KnownHostsFile == "" {
		return nil, errors.New("git: TempDir and KnownHostsFile are required")
	}
	if o.MaxRepoSize < 0 {
		return nil, errors.New("git: MaxRepoSize must not be negative")
	}
	tmp, err := filepath.Abs(o.TempDir)
	if err != nil {
		return nil, fmt.Errorf("git: temp dir: %w", err)
	}
	kh, err := filepath.Abs(o.KnownHostsFile)
	if err != nil {
		return nil, fmt.Errorf("git: known_hosts: %w", err)
	}
	for _, p := range []string{tmp, kh} {
		if err := CheckSSHConfigPath(p); err != nil {
			return nil, fmt.Errorf("git: %s cannot be used in the SSH configuration: %w", p, err)
		}
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
	if o.MinFreeSpace == 0 {
		o.MinFreeSpace = DefaultMinFreeSpace
	}
	if o.sizeInterval <= 0 {
		o.sizeInterval = sizeCheckInterval
	}
	return &Runner{
		git:            o.GitPath,
		askpass:        o.AskpassPath,
		tmp:            tmp,
		knownHosts:     kh,
		networkTimeout: o.NetworkTimeout,
		localTimeout:   o.LocalTimeout,
		baseEnv:        filterEnv(o.Environ, runtime.GOOS == "windows"),
		maxSize:        o.MaxRepoSize,
		minFree:        o.MinFreeSpace,
		sizeInterval:   o.sizeInterval,
		skipLFSCheck:   o.skipLFSConfigCheck,
		noLFSPin:       o.noLFSPin,
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

// lfsPin pins the Git LFS endpoint. Command-line config outranks both the
// repository's .lfsconfig and .git/config, and git-lfs consults lfs.url
// before remote.<name>.lfsurl; both are set so that neither key from any
// other source can redirect LFS traffic.
func lfsPin(endpoint string) []string {
	return []string{
		"-c", "lfs.url=" + endpoint,
		"-c", "remote.origin.lfsurl=" + endpoint,
	}
}

// argv returns the arguments after the git executable for args. A non-empty
// lfsEndpoint is pinned (see lfsPin) when Git LFS is available, so every
// git-lfs process git starts (filter-process, lfs pull) uses it.
func (r *Runner) argv(lfsEndpoint string, args []string) []string {
	out := hardening()
	if r.lfsAvailable() {
		out = append(out, lfsFilter()...)
		if lfsEndpoint != "" && !r.noLFSPin {
			out = append(out, lfsPin(lfsEndpoint)...)
		}
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

// sshPath converts a path for ssh (forward slashes work for both native and
// MSYS ssh on Windows).
func sshPath(p string) string { return filepath.ToSlash(p) }

// sshCommand returns GIT_SSH_COMMAND: ssh with the per-operation config
// file as its only configuration (-F also skips ~/.ssh/config and the
// system-wide file). The path is a single shell word; every option lives
// in the file, because ssh splits -o values on whitespace and expands
// tokens in them.
func sshCommand(configFile string) string {
	return "ssh -F " + shQuote(sshPath(configFile))
}

// sshConfigQuote renders path p as an ssh_config(5) argument: double
// quoted (spaces) with '%' doubled (IdentityFile and UserKnownHostsFile
// expand % tokens). p must pass CheckSSHConfigPath.
func sshConfigQuote(p string) string {
	return `"` + strings.ReplaceAll(sshPath(p), "%", "%%") + `"`
}

// sshConfig returns the per-operation ssh_config. keyFile may be empty:
// then no public key is offered at all, never the daemon user's own.
func sshConfig(knownHosts, keyFile string) string {
	identity, pubkey := "none", "no"
	if keyFile != "" {
		identity, pubkey = sshConfigQuote(keyFile), "yes"
	}
	opts := [][2]string{
		{"BatchMode", "yes"},
		{"IdentitiesOnly", "yes"},
		// An explicit IdentityFile also keeps ssh from adding the default
		// ~/.ssh/id_* identities (found through the passwd entry).
		{"IdentityFile", identity},
		{"IdentityAgent", "none"},
		{"PubkeyAuthentication", pubkey},
		{"PasswordAuthentication", "no"},
		{"KbdInteractiveAuthentication", "no"},
		{"HostbasedAuthentication", "no"},
		{"StrictHostKeyChecking", "accept-new"},
		{"UserKnownHostsFile", sshConfigQuote(knownHosts)},
		{"GlobalKnownHostsFile", "none"},
		{"UpdateHostKeys", "no"},
		{"CheckHostIP", "no"},
		{"HashKnownHosts", "no"},
		{"ConnectTimeout", "30"},
		{"ForwardAgent", "no"},
		{"ForwardX11", "no"},
		{"ClearAllForwardings", "yes"},
		{"ProxyCommand", "none"},
		{"PermitLocalCommand", "no"},
		{"ControlMaster", "no"},
		{"ControlPath", "none"},
		// Clear SendEnv, then allow only the variable git uses to request
		// protocol v2 (git passes -o SendEnv=GIT_PROTOCOL itself).
		{"SendEnv", "-*"},
		{"SendEnv", "GIT_PROTOCOL"},
	}
	var b strings.Builder
	b.WriteString("# Written by golem-deploy for one git operation and removed when it ends.\n")
	for _, o := range opts {
		b.WriteString(o[0] + " " + o[1] + "\n")
	}
	return b.String()
}

// session is the per-operation state: a private scratch directory (HOME,
// SSH key and SSH config), the environment and the scrubbing log writer.
type session struct {
	r   *Runner
	dir string
	env []string
	log *scrubber
	// lfsEndpoint is the pinned Git LFS endpoint ("" without a remote).
	lfsEndpoint string
}

// begin creates the session for remote, writing output to log.
func (r *Runner) begin(remote Remote, log io.Writer) (*session, error) {
	if log == nil {
		log = io.Discard
	}
	if err := os.MkdirAll(r.tmp, 0o700); err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	dir, err := os.MkdirTemp(r.tmp, sessionPrefix)
	if err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	s := &session{r: r, dir: dir, log: newScrubber(log, remote.Auth.secrets())}
	if remote.URL != "" {
		s.lfsEndpoint = LFSEndpoint(remote.URL)
	}
	keyFile := ""
	if remote.Auth.Method == AuthSSH && len(remote.Auth.PrivateKey) > 0 {
		keyFile = filepath.Join(dir, "id_key")
		if err := os.WriteFile(keyFile, remote.Auth.PrivateKey, 0o600); err != nil {
			s.close()
			return nil, fmt.Errorf("write ssh key: %w", err)
		}
	}
	sshConfigFile := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(sshConfigFile, []byte(sshConfig(r.knownHosts, keyFile)), 0o600); err != nil {
		s.close()
		return nil, fmt.Errorf("write ssh config: %w", err)
	}
	env := append([]string{}, r.baseEnv...)
	env = append(env,
		"HOME="+dir,
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_SSH_COMMAND="+sshCommand(sshConfigFile),
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
	// watch is the directory the command writes the checkout into; with
	// Options.MaxRepoSize set, its size is bounded while the command runs.
	watch string
}

// errSizeLimit is the cancellation cause set by the size watchdog.
var errSizeLimit = errors.New("checkout size limit exceeded")

// run executes git args in dir. Failures are *Error with a Reason derived
// from the context and the command's (scrubbed) output.
func (s *session) run(ctx context.Context, op, dir string, o cmdOpts, args ...string) error {
	timeout := s.r.localTimeout
	if o.network {
		timeout = s.r.networkTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// wctx is additionally cancelled by the size watchdog (errSizeLimit).
	wctx, stop := context.WithCancelCause(cctx)
	defer stop(nil)
	watch := o.watch != "" && s.r.maxSize > 0

	if !o.quiet {
		s.log.println("$ git " + strings.Join(args, " "))
	}
	s.log.resetTail()

	cmd := exec.CommandContext(wctx, s.r.git, s.r.argv(s.lfsEndpoint, args)...)
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
	stopWatch := func() {}
	var watchDone chan struct{}
	if watch {
		var watchCtx context.Context
		watchCtx, stopWatch = context.WithCancel(wctx)
		watchDone = make(chan struct{})
		go func() {
			defer close(watchDone)
			s.r.watchSize(watchCtx, o.watch, func() { stop(errSizeLimit) })
		}()
	}
	err := cmd.Wait()
	tree.close()
	stopWatch()
	if watchDone != nil {
		<-watchDone
	}
	s.log.Flush()
	if err == nil && watch && ctx.Err() == nil && dirExceeds(ctx, o.watch, s.r.maxSize) {
		// The command finished between two samples.
		stop(errSizeLimit)
		err = errSizeLimit
	}
	if err == nil {
		return nil
	}
	reason := classify(s.log.Tail())
	switch {
	case ctx.Err() != nil:
		reason = ReasonCancelled
	case errors.Is(context.Cause(wctx), errSizeLimit):
		reason = ReasonSizeLimit
		s.log.println(fmt.Sprintf("error: the checkout exceeds the size limit of %d MiB set on the host", s.r.maxSize>>20))
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		reason = ReasonTimeout
	}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		err = fmt.Errorf("git %s exited with status %d", firstNonOption(args), ee.ExitCode())
	case errors.Is(err, errSizeLimit):
	default:
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

// watchSize samples the size of dir until ctx is done and calls exceeded
// (once) when it grows beyond the runner's MaxRepoSize. The interval grows
// with the cost of a walk, so sampling a huge tree never dominates.
func (r *Runner) watchSize(ctx context.Context, dir string, exceeded func()) {
	t := time.NewTimer(r.sizeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		start := time.Now()
		if dirExceeds(ctx, dir, r.maxSize) {
			exceeded()
			return
		}
		t.Reset(max(r.sizeInterval, 4*time.Since(start)))
	}
}

// dirExceeds reports whether the files below dir (symlinks not followed)
// total more than limit bytes. The walk stops as soon as the limit is
// passed or ctx is done; entries that vanish meanwhile are skipped.
func dirExceeds(ctx context.Context, dir string, limit int64) bool {
	var total int64
	over := false
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return fs.SkipAll
		}
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		if total > limit {
			over = true
			return fs.SkipAll
		}
		return nil
	})
	return over
}

// checkFreeSpace refuses to start an operation that writes a checkout when
// the data filesystem has less than MinFreeSpace available. When free space
// cannot be determined the operation proceeds (the size limit still
// applies).
func (r *Runner) checkFreeSpace(op string) error {
	if r.minFree < 0 {
		return nil
	}
	if err := os.MkdirAll(r.tmp, 0o700); err != nil {
		return &Error{Op: op, Reason: ReasonLocal, Unchanged: true, Err: fmt.Errorf("create temp dir: %w", err)}
	}
	free, err := freeSpace(r.tmp)
	if err != nil {
		return nil
	}
	if free < uint64(r.minFree) {
		return &Error{Op: op, Reason: ReasonDiskSpace, Unchanged: true,
			Err: fmt.Errorf("%d MiB free on the data filesystem, at least %d MiB required", free>>20, r.minFree>>20)}
	}
	return nil
}
