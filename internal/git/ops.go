package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/demiurgos-hub/golem-deploy/internal/logsafe"
)

// Checkout describes a working tree after a successful operation.
type Checkout struct {
	Branch string
	// Commit is the full hash of HEAD.
	Commit string
	// LFS reports that Git LFS content was fetched.
	LFS bool
}

func invalid(op string, err error) error {
	return &Error{Op: op, Reason: ReasonInvalidInput, Unchanged: true, Err: err}
}

// Clone clones remote into dir, which must not exist. It clones into a
// staging directory under the runner's TempDir and renames the result into
// place only when everything (including LFS content) succeeded, so dir
// never holds a partial checkout. An empty remote.Branch selects the
// remote's default branch. Full history and all branches are fetched
// (builds may need "git describe", and switching branches needs no
// re-clone).
func (r *Runner) Clone(ctx context.Context, remote Remote, dir string, log io.Writer) (Checkout, error) {
	const op = "clone"
	if _, err := remote.Validate(); err != nil {
		return Checkout{}, invalid(op, err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		return Checkout{}, &Error{Op: op, Reason: ReasonLocal, Unchanged: true, Err: errors.New("checkout directory already exists")}
	}
	if err := r.checkFreeSpace(op); err != nil {
		return Checkout{}, err
	}
	s, err := r.begin(remote, log)
	if err != nil {
		return Checkout{}, &Error{Op: op, Reason: ReasonLocal, Unchanged: true, Err: err}
	}
	defer s.close()

	staging, err := os.MkdirTemp(r.tmp, clonePrefix)
	if err != nil {
		return Checkout{}, &Error{Op: op, Reason: ReasonLocal, Unchanged: true, Err: err}
	}
	defer os.RemoveAll(staging)
	// Clone into a relative directory from inside the staging dir so git's
	// messages ("Cloning into 'repo'...") do not reveal data-dir paths.
	target := filepath.Join(staging, "repo")
	if err := s.run(ctx, op, staging, cmdOpts{network: true, watch: staging}, cloneArgs(remote.URL, remote.Branch, "repo")...); err != nil {
		return Checkout{}, markUnchanged(err)
	}
	co, err := s.finish(ctx, op, target, "")
	if err != nil {
		return Checkout{}, markUnchanged(err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return Checkout{}, &Error{Op: op, Reason: ReasonLocal, Unchanged: true, Err: err}
	}
	if err := os.Rename(target, dir); err != nil {
		return Checkout{}, &Error{Op: op, Reason: ReasonLocal, Unchanged: true, Err: errors.New("could not move the clone into place")}
	}
	s.log.println("Checked out " + co.Branch + " at " + co.Commit)
	return co, nil
}

// markUnchanged flags a clone failure as leaving no checkout behind.
func markUnchanged(err error) error {
	var ge *Error
	if errors.As(err, &ge) {
		ge.Unchanged = true
	}
	return err
}

// Pull brings the checkout in dir to the remote's current remote.Branch:
//
//  1. git remote set-url origin <url> (the URL stored for the project)
//  2. git fetch --prune --force --tags origin +refs/heads/<b>:refs/remotes/origin/<b>
//  3. git checkout --force --no-track -B <b> refs/remotes/origin/<b>
//  4. git clean -ffdx
//  5. git lfs pull, when the tree uses Git LFS
//
// There is no merge: local changes are discarded and the tree always equals
// the fetched commit. A failure in steps 1-2 leaves the checkout untouched
// (Error.Unchanged).
func (r *Runner) Pull(ctx context.Context, remote Remote, dir string, log io.Writer) (Checkout, error) {
	return r.sync(ctx, "pull", remote, dir, log)
}

// SwitchBranch checks out remote.Branch in dir with the same steps as Pull:
// the branch is fetched explicitly, checked out as a local branch at the
// remote commit and the tree cleaned. A branch missing on the remote fails
// with ReasonBranchNotFound and leaves the checkout untouched.
func (r *Runner) SwitchBranch(ctx context.Context, remote Remote, dir string, log io.Writer) (Checkout, error) {
	return r.sync(ctx, "branch", remote, dir, log)
}

func (r *Runner) sync(ctx context.Context, op string, remote Remote, dir string, log io.Writer) (Checkout, error) {
	if remote.Branch == "" {
		return Checkout{}, invalid(op, errors.New("branch is required"))
	}
	if _, err := remote.Validate(); err != nil {
		return Checkout{}, invalid(op, err)
	}
	if !HasCheckout(dir) {
		return Checkout{}, &Error{Op: op, Reason: ReasonLocal, Unchanged: true, Err: errors.New("no checkout")}
	}
	if err := r.checkFreeSpace(op); err != nil {
		return Checkout{}, err
	}
	s, err := r.begin(remote, log)
	if err != nil {
		return Checkout{}, &Error{Op: op, Reason: ReasonLocal, Unchanged: true, Err: err}
	}
	defer s.close()

	if err := s.run(ctx, op, dir, cmdOpts{}, setURLArgs(remote.URL)...); err != nil {
		return Checkout{}, markUnchanged(err)
	}
	if err := s.run(ctx, op, dir, cmdOpts{network: true, watch: dir}, fetchArgs(remote.Branch)...); err != nil {
		removePartialDownloads(dir)
		return Checkout{}, markUnchanged(err)
	}
	co, err := s.finish(ctx, op, dir, remote.Branch)
	if err != nil {
		return Checkout{}, err
	}
	s.log.println("Checked out " + co.Branch + " at " + co.Commit)
	return co, nil
}

// finish completes a checkout in dir: for sync operations (branch set) it
// resets the tree to the fetched branch and cleans it; then it fetches LFS
// content if needed and reads the branch and commit.
func (s *session) finish(ctx context.Context, op, dir, branch string) (Checkout, error) {
	if branch != "" {
		if err := s.run(ctx, op, dir, cmdOpts{watch: dir}, checkoutArgs(branch)...); err != nil {
			return Checkout{}, localErr(err)
		}
		if err := s.run(ctx, op, dir, cmdOpts{}, cleanArgs()...); err != nil {
			return Checkout{}, localErr(err)
		}
	}
	var out bytes.Buffer
	if err := s.run(ctx, op, dir, cmdOpts{quiet: true, stdout: &out}, currentBranchArgs()...); err != nil {
		return Checkout{}, &Error{Op: op, Reason: ReasonUnknown, Err: errors.New("the remote's HEAD is not a branch; set a branch explicitly")}
	}
	co := Checkout{Branch: strings.TrimSpace(out.String())}
	if co.Branch == "" || ValidateBranch(co.Branch) != nil {
		return Checkout{}, &Error{Op: op, Reason: ReasonUnknown, Err: errors.New("unsupported branch name")}
	}
	lfs, err := s.usesLFS(ctx, op, dir)
	if err != nil {
		return Checkout{}, err
	}
	if lfs {
		if !s.r.lfsAvailable() {
			s.log.println("error: this repository uses Git LFS, but git-lfs is not installed on the host")
			return Checkout{}, &Error{Op: op, Reason: ReasonLFSMissing}
		}
		if err := s.checkLFSConfig(ctx, op, dir); err != nil {
			return Checkout{}, err
		}
		include, err := s.lfsIncludes(op, dir)
		if err != nil {
			return Checkout{}, err
		}
		if err := s.run(ctx, op, dir, cmdOpts{network: true, lfsSmudge: true, watch: dir}, lfsPullArgs(include)...); err != nil {
			removePartialDownloads(dir)
			return Checkout{}, err
		}
		co.LFS = true
	}
	out.Reset()
	if err := s.run(ctx, op, dir, cmdOpts{quiet: true, stdout: &out}, headArgs()...); err != nil {
		if branch == "" {
			// A fresh clone without a commit.
			return Checkout{}, &Error{Op: op, Reason: ReasonEmptyRepository, Err: err}
		}
		return Checkout{}, err
	}
	co.Commit = strings.TrimSpace(out.String())
	if !isHash(co.Commit) {
		return Checkout{}, &Error{Op: op, Reason: ReasonUnknown, Err: errors.New("unexpected rev-parse output")}
	}
	return co, nil
}

func localErr(err error) error {
	var ge *Error
	if errors.As(err, &ge) && (ge.Reason == ReasonUnknown) {
		ge.Reason = ReasonLocal
	}
	return err
}

// maxAttributes bounds how much of a .gitattributes file is read.
const maxAttributes = 1 << 20

// usesLFS reports whether any tracked .gitattributes file selects the LFS
// filter.
func (s *session) usesLFS(ctx context.Context, op, dir string) (bool, error) {
	var out bytes.Buffer
	if err := s.run(ctx, op, dir, cmdOpts{quiet: true, stdout: &out}, attributesArgs()...); err != nil {
		return false, err
	}
	for _, rel := range strings.Split(out.String(), "\x00") {
		if rel == "" || filepath.IsAbs(rel) || strings.Contains(rel, "..") {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(rel))
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(f, maxAttributes))
		f.Close()
		if bytes.Contains(data, []byte("filter=lfs")) {
			return true, nil
		}
	}
	return false, nil
}

// maxLsRemote bounds the ls-remote output read.
const maxLsRemote = 8 << 20

// ListRemoteBranches returns the remote's branch names, sorted. Names that
// ValidateBranch rejects are omitted (they could not be checked out).
func (r *Runner) ListRemoteBranches(ctx context.Context, remote Remote) ([]string, error) {
	const op = "ls-remote"
	remote.Branch = ""
	if _, err := remote.Validate(); err != nil {
		return nil, invalid(op, err)
	}
	s, err := r.begin(remote, nil)
	if err != nil {
		return nil, &Error{Op: op, Reason: ReasonLocal, Err: err}
	}
	defer s.close()
	out := &limitedBuffer{max: maxLsRemote}
	if err := s.run(ctx, op, s.dir, cmdOpts{network: true, quiet: true, stdout: out}, lsRemoteArgs(remote.URL)...); err != nil {
		return nil, err
	}
	if out.overflow {
		return nil, &Error{Op: op, Reason: ReasonUnknown, Err: errors.New("too many remote branches")}
	}
	return parseLsRemote(out.Bytes()), nil
}

func parseLsRemote(b []byte) []string {
	seen := map[string]bool{}
	names := []string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		hash, ref, ok := strings.Cut(sc.Text(), "\t")
		if !ok || !isHash(hash) {
			continue
		}
		name, ok := strings.CutPrefix(ref, "refs/heads/")
		if !ok || seen[name] || ValidateBranch(name) != nil {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// HeadCommit returns the full hash of HEAD in dir.
func (r *Runner) HeadCommit(ctx context.Context, dir string) (string, error) {
	const op = "rev-parse"
	s, err := r.begin(Remote{}, nil)
	if err != nil {
		return "", &Error{Op: op, Reason: ReasonLocal, Err: err}
	}
	defer s.close()
	var out bytes.Buffer
	if err := s.run(ctx, op, dir, cmdOpts{quiet: true, stdout: &out}, headArgs()...); err != nil {
		return "", err
	}
	h := strings.TrimSpace(out.String())
	if !isHash(h) {
		return "", &Error{Op: op, Reason: ReasonUnknown, Err: errors.New("unexpected rev-parse output")}
	}
	return h, nil
}

// HasCheckout reports whether dir contains a git working tree.
func HasCheckout(dir string) bool {
	fi, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil && fi.IsDir()
}

func isHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// limitedBuffer stores up to max bytes and records overflow.
type limitedBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.Len(); len(p) > room {
		l.overflow = true
		if room > 0 {
			l.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return l.Buffer.Write(p)
}

// Argument lists of each git command (after the hardening options). Every
// positional argument follows "--" or "--end-of-options", and option values
// use the stuck "--opt=value" form.

func cloneArgs(url, branch, dir string) []string {
	args := []string{"clone", "--origin=origin", "--no-recurse-submodules"}
	if branch != "" {
		args = append(args, "--branch="+branch)
	}
	return append(args, "--", url, dir)
}

func setURLArgs(url string) []string {
	return []string{"remote", "set-url", "--end-of-options", "origin", url}
}

func fetchArgs(branch string) []string {
	return []string{"fetch", "--prune", "--force", "--tags", "--no-recurse-submodules", "--end-of-options",
		"origin", fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branch, branch)}
}

func checkoutArgs(branch string) []string {
	return []string{"checkout", "--force", "--no-track", "--no-recurse-submodules", "-B", branch, "refs/remotes/origin/" + branch, "--"}
}

func cleanArgs() []string { return []string{"clean", "-ffdx", "--"} }

func currentBranchArgs() []string { return []string{"symbolic-ref", "--quiet", "--short", "HEAD"} }

func headArgs() []string {
	return []string{"rev-parse", "--verify", "--end-of-options", "HEAD^{commit}"}
}

func attributesArgs() []string {
	return []string{"ls-files", "-z", "--", ".gitattributes", ":(glob)**/.gitattributes"}
}

// lfsPullArgs adds the include patterns as exactly one stuck
// "--include=<p1>,<p2>" argument (patterns passed checkLFSPatterns: no
// comma, no white space, none starts with '-').
func lfsPullArgs(include []string) []string {
	args := []string{"lfs", "pull"}
	if len(include) > 0 {
		args = append(args, "--include="+strings.Join(include, ","))
	}
	return args
}

func lfsConfigListArgs() []string {
	return []string{"config", "--file=.lfsconfig", "--no-includes", "--list", "-z"}
}

func lsRemoteArgs(url string) []string {
	return []string{"ls-remote", "--heads", "--end-of-options", url}
}

// removePartialDownloads deletes what an interrupted fetch or "git lfs pull"
// leaves in a checkout (temporary pack files, incomplete LFS objects), so a
// download stopped by the size limit does not keep its bytes.
func removePartialDownloads(dir string) {
	for _, pattern := range []string{
		filepath.Join(dir, ".git", "objects", "pack", "tmp_*"),
		filepath.Join(dir, ".git", "objects", "incoming-*"),
		filepath.Join(dir, ".git", "lfs", "incomplete", "*"),
		filepath.Join(dir, ".git", "lfs", "tmp", "*"),
	} {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			_ = os.RemoveAll(m)
		}
	}
}

// maxLFSConfig bounds the "git config --list" output read for .lfsconfig.
const maxLFSConfig = 1 << 20

// checkLFSConfig refuses a checkout whose .lfsconfig sets endpoint or
// transfer settings. The endpoint is pinned on the command line anyway
// (see lfsPin); the refusal makes the situation visible instead of
// silently ignoring the repository's settings. The file is parsed by git
// itself (git config --file, includes disabled), never by hand. Only a
// regular file is read: a symlink could point anywhere on the host.
func (s *session) checkLFSConfig(ctx context.Context, op, dir string) error {
	if s.r.skipLFSCheck {
		return nil
	}
	fi, err := os.Lstat(filepath.Join(dir, ".lfsconfig"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return &Error{Op: op, Reason: ReasonLocal, Err: errors.New("could not read .lfsconfig")}
	}
	if !fi.Mode().IsRegular() {
		s.log.println("error: .lfsconfig is not a regular file")
		return &Error{Op: op, Reason: ReasonLFSConfig, Err: errors.New(".lfsconfig is not a regular file")}
	}
	out := &limitedBuffer{max: maxLFSConfig}
	if err := s.run(ctx, op, dir, cmdOpts{quiet: true, stdout: out}, lfsConfigListArgs()...); err != nil {
		var ge *Error
		if errors.As(err, &ge) && (ge.Reason == ReasonCancelled || ge.Reason == ReasonTimeout) {
			return err
		}
		s.log.println("error: .lfsconfig could not be parsed")
		return &Error{Op: op, Reason: ReasonLFSConfig, Err: errors.New(".lfsconfig could not be parsed")}
	}
	if out.overflow {
		s.log.println("error: .lfsconfig is too large")
		return &Error{Op: op, Reason: ReasonLFSConfig, Err: errors.New(".lfsconfig is too large")}
	}
	bad := disallowedLFSKeys(out.Bytes(), s.lfsEndpoint)
	if len(bad) == 0 {
		return nil
	}
	for _, k := range bad {
		if len(k) > 200 {
			k = k[:200] + "..."
		}
		s.log.println("error: .lfsconfig sets " + strconv.Quote(k) + ", which Golem Deploy does not allow")
	}
	s.log.println("The Git LFS server is always derived from the project's repository URL (" + s.lfsEndpoint + "). " +
		"Remove these settings from .lfsconfig, or point the project at the repository that hosts the LFS objects.")
	return &Error{Op: op, Reason: ReasonLFSConfig, Err: fmt.Errorf(".lfsconfig sets %d disallowed key(s)", len(bad))}
}

// disallowedLFSKeys returns the keys of "git config --list -z" output that
// change where or how git-lfs transfers objects: lfs.url, lfs.pushurl,
// remote.*.lfsurl, remote.*.lfspushurl, lfs.<url>.access for any URL but
// endpoint, lfs.customtransfer.*, lfs.standalonetransferagent,
// lfs.extension.* and url.*.insteadof / url.*.pushinsteadof.
func disallowedLFSKeys(list []byte, endpoint string) []string {
	var bad []string
	for _, entry := range strings.Split(string(list), "\x00") {
		key, _, _ := strings.Cut(entry, "\n")
		if key == "" {
			continue
		}
		// git lower-cases section and variable names; subsections keep
		// their case, which does not matter for these patterns.
		k := strings.ToLower(key)
		hasSuffix := func(sfx ...string) bool {
			for _, x := range sfx {
				if strings.HasSuffix(k, x) {
					return true
				}
			}
			return false
		}
		switch {
		case k == "lfs.url", k == "lfs.pushurl", k == "lfs.standalonetransferagent",
			strings.HasPrefix(k, "lfs.customtransfer."), strings.HasPrefix(k, "lfs.extension."),
			strings.HasPrefix(k, "remote.") && hasSuffix(".lfsurl", ".lfspushurl"),
			strings.HasPrefix(k, "url.") && hasSuffix(".insteadof", ".pushinsteadof"):
			bad = append(bad, key)
		case strings.HasPrefix(k, "lfs.") && strings.HasSuffix(k, ".access") && len(k) > len("lfs..access"):
			if key[len("lfs."):len(key)-len(".access")] != endpoint {
				bad = append(bad, key)
			}
		}
	}
	return bad
}

// deployConfigFile is the repository's optional deploy configuration
// (build.ConfigFile), named in messages about lfs.include.
const deployConfigFile = "golem-deploy.yaml"

// Bounds re-checked on the patterns Options.LFSInclude returns.
const (
	maxLFSPatterns     = 32
	maxLFSIncludeBytes = 4096
)

// lfsPatternRE is the character set of an lfs.include pattern; the first
// character is neither '-' nor '/'.
var lfsPatternRE = regexp.MustCompile(`^[A-Za-z0-9_.*][A-Za-z0-9_.*/-]*$`)

// checkLFSPatterns re-checks the patterns before they become an argument
// of git lfs pull (defence in depth: the deploy config parser validates
// them fully).
func checkLFSPatterns(patterns []string) error {
	if len(patterns) > maxLFSPatterns {
		return errors.New("too many lfs.include patterns")
	}
	total := 0
	for _, p := range patterns {
		if !lfsPatternRE.MatchString(p) || strings.Contains(p, "/-") {
			return errors.New("an lfs.include pattern contains characters that are not allowed")
		}
		total += len(p) + 1
	}
	if total > maxLFSIncludeBytes+1 {
		return errors.New("the lfs.include patterns are too long")
	}
	return nil
}

// lfsIncludes returns the patterns that limit "git lfs pull" (nil: all)
// from Options.LFSInclude, logging one line when the download is limited.
// A golem-deploy.yaml that cannot be read or is not valid fails the
// operation: silently downloading everything could be tens of gigabytes.
func (s *session) lfsIncludes(op, dir string) ([]string, error) {
	if s.r.lfsInclude == nil {
		return nil, nil
	}
	fail := func(cause error, msg string) error {
		msg = safeDetail(msg)
		s.log.println("error: " + msg)
		return &Error{Op: op, Reason: ReasonDeployConfig, Err: cause, detail: msg}
	}
	patterns, err := s.r.lfsInclude(dir)
	if err != nil {
		return nil, fail(err, err.Error())
	}
	if len(patterns) == 0 {
		return nil, nil
	}
	if err := checkLFSPatterns(patterns); err != nil {
		return nil, fail(err, deployConfigFile+": "+err.Error())
	}
	noun := "patterns"
	if len(patterns) == 1 {
		noun = "pattern"
	}
	s.log.println(fmt.Sprintf("Git LFS: downloading only the paths listed under lfs.include in %s (%d %s)", deployConfigFile, len(patterns), noun))
	return patterns, nil
}

// maxDetail bounds a user-facing failure detail.
const maxDetail = 300

// safeDetail renders msg as a single line with control and bidirectional
// characters escaped, clipped to maxDetail bytes.
func safeDetail(msg string) string {
	msg = logsafe.String(strings.Join(strings.Fields(msg), " "))
	if len(msg) <= maxDetail {
		return msg
	}
	cut := maxDetail
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + "..."
}
