// Package docker is a typed, label-scoped wrapper around the docker CLI.
//
// Every object the daemon creates carries the managed label
// (LabelManaged=true) and this installation's instance label
// (LabelInstance=<instance id>), and every operation on an existing object
// first proves the object is ours: a server's container is reached only
// through the container id recorded when it was created (ServerRef), never
// through a client-supplied name or id and never through labels alone (an
// image can carry any label and containers inherit image labels), and its
// labels are verified on inspect as a second check; images, volumes and
// networks are inspected and their labels checked before they are changed.
// Commands run without a shell, with a timeout, with "--" before positional
// arguments and with JSON output. Container environment values are never
// placed in argv or in the CLI's own environment: they travel in an env file
// (see envfile.go).
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// Cmd is one docker CLI invocation.
type Cmd struct {
	// Args follow the binary (argv[1:]).
	Args []string
	// Env is the complete child environment (never inherited). It is always
	// the fixed allowlist taken from the daemon's own environment (BaseEnv);
	// nothing derived from a request is ever added.
	Env []string
	// ExtraFiles are inherited by the child as fds 3, 4, ... (the env-file
	// pipe). The runner owns them: it closes them once the child has
	// started, or when starting it fails.
	ExtraFiles []*os.File
	// Stdout and Stderr receive the output. They may be the same writer.
	Stdout io.Writer
	Stderr io.Writer
	// ProcessTree keeps CLI plugins in the command's lifetime.
	ProcessTree bool
}

// Runner executes docker CLI invocations. ExecRunner is the real one; tests
// inject a fake (see internal/docker/dockertest).
type Runner interface {
	Run(ctx context.Context, path string, cmd Cmd) error
}

// ExecRunner runs commands with os/exec and optionally scopes cancellation
// to the whole process tree.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, path string, c Cmd) error {
	cmd := exec.CommandContext(ctx, path, c.Args...)
	cmd.Env = c.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.ExtraFiles = c.ExtraFiles
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	cmd.WaitDelay = 5 * time.Second
	if c.ProcessTree {
		defer closeFiles(c.ExtraFiles)
		return runProcessTree(ctx, cmd)
	}
	err := cmd.Start()
	// The child holds its own copies now. Closing ours means a pipe writer
	// gets EPIPE instead of blocking if the child exits without reading.
	closeFiles(c.ExtraFiles)
	if err != nil {
		return err
	}
	return cmd.Wait()
}

func closeFiles(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// Options configures New.
type Options struct {
	// Binary is the docker CLI; a bare name is resolved on PATH once.
	// Default "docker".
	Binary string
	// Runner defaults to ExecRunner.
	Runner Runner
	Logger *slog.Logger
	// LookupEnv reads the daemon's environment for the child's base
	// environment (default os.LookupEnv).
	LookupEnv func(string) (string, bool)
	// InstanceID is this installation's id (32 lower-case hex characters,
	// store.InstanceID). Every created object is labelled with it, and only
	// objects carrying it are listed or acted on. Without a valid id every
	// call that lists, creates or changes containers, networks or volumes
	// fails.
	InstanceID string
	// TempDir holds env files on platforms without the pipe mechanism
	// (the data directory's tmp/; default os.TempDir()).
	TempDir string
	// BuildConfigDir is the dedicated Docker client configuration for builds.
	BuildConfigDir string
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Client talks to Docker through the CLI. It is safe for concurrent use.
type Client struct {
	bin            string
	binErr         error
	runner         Runner
	logger         *slog.Logger
	baseEnv        []string
	now            func() time.Time
	instance       string
	envFile        envFileFunc
	buildConfigDir string

	probeMu sync.Mutex
	probeAt time.Time
	probeOK bool
}

// Timeouts for CLI calls.
const (
	quickTimeout = 20 * time.Second
	runTimeout   = 2 * time.Minute
	probeTimeout = 5 * time.Second
	probeTTL     = 15 * time.Second
	// stopMargin is added to a container's stop timeout for the CLI call.
	stopMargin = 30 * time.Second
)

// Output limits for captured CLI output.
const (
	maxStdout = 32 << 20
	maxStderr = 8 << 10
)

// New returns a Client. A missing docker binary is not an error here; every
// call then fails with ErrUnavailable.
func New(o Options) *Client {
	if o.Binary == "" {
		o.Binary = "docker"
	}
	if o.Runner == nil {
		o.Runner = ExecRunner{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	c := &Client{runner: o.Runner, logger: o.Logger, now: o.Now, bin: o.Binary, instance: o.InstanceID,
		envFile: newEnvFileFunc(runtime.GOOS, o.TempDir), buildConfigDir: o.BuildConfigDir}
	if _, real := o.Runner.(ExecRunner); real && !strings.ContainsAny(o.Binary, `/\`) {
		// Resolve once so later PATH changes cannot swap the binary.
		// exec.LookPath never returns a path relative to the working
		// directory (exec.ErrDot).
		p, err := exec.LookPath(o.Binary)
		if err != nil {
			c.binErr = err
		} else {
			c.bin = p
		}
	}
	c.baseEnv = BaseEnv(o.LookupEnv, runtime.GOOS)
	return c
}

// errNoInstance is returned by calls that need the instance id when the
// client was created without a valid one.
var errNoInstance = errors.New("docker: client has no valid instance id")

func (c *Client) needInstance() error {
	if !ValidInstanceID(c.instance) {
		return errNoInstance
	}
	return nil
}

// ours reports whether labels mark an object as created by this
// installation (managed label and our instance id).
func (c *Client) ours(labels map[string]string) bool {
	return IsManaged(labels) && c.instance != "" && labels[LabelInstance] == c.instance
}

// childEnvNames are the daemon environment variables passed to the docker
// CLI. Everything else (including the daemon's own secrets) is withheld.
func childEnvNames(goos string) []string {
	names := []string{
		"PATH", "HOME", "XDG_RUNTIME_DIR",
		"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG",
		"DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY", "DOCKER_API_VERSION",
	}
	if goos == "windows" {
		names = append(names,
			"SystemRoot", "SystemDrive", "windir", "ComSpec", "PATHEXT",
			"TEMP", "TMP", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
			"ProgramData", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432",
			"HOMEDRIVE", "HOMEPATH", "USERNAME", "USERDOMAIN", "COMPUTERNAME",
			"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE", "OS",
		)
	}
	return names
}

// BaseEnv returns the minimal environment for the docker CLI child process:
// a fixed allowlist read from the daemon's own environment.
func BaseEnv(lookup func(string) (string, bool), goos string) []string {
	var env []string
	for _, k := range childEnvNames(goos) {
		if v, ok := lookup(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// capBuffer is a bytes.Buffer that stops growing at max bytes.
type capBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.max - b.buf.Len(); room < len(p) {
		b.truncated = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return n, nil
	}
	b.buf.Write(p)
	return n, nil
}

// exec runs one CLI call with the fixed base environment. extra files are
// inherited by the child (the runner closes them). stdout may be nil to
// discard. When stderr is nil, it is captured for error classification.
func (c *Client) exec(ctx context.Context, timeout time.Duration, args []string, extra []*os.File, stdout, stderr io.Writer) error {
	return c.execEnv(ctx, timeout, args, c.baseEnv, extra, stdout, stderr, false)
}

// execEnv is exec with a caller-selected client environment. Its callers
// use the base allowlist or the dedicated build environment.
func (c *Client) execEnv(ctx context.Context, timeout time.Duration, args, env []string, extra []*os.File, stdout, stderr io.Writer, tree bool) error {
	if c.binErr != nil {
		closeFiles(extra)
		return &CmdError{Op: opName(args), Kind: ErrUnavailable, Detail: "docker CLI not found"}
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var errBuf *capBuffer
	if stderr == nil {
		errBuf = &capBuffer{max: maxStderr}
		stderr = errBuf
	}
	if stdout == nil {
		stdout = io.Discard
	}
	c.logger.Debug("docker", "op", opName(args))
	err := c.runner.Run(ctx, c.bin, Cmd{Args: args, Env: slices.Clone(env), ExtraFiles: extra, Stdout: stdout, Stderr: stderr, ProcessTree: tree})
	if err == nil {
		return nil
	}
	msg := ""
	if errBuf != nil {
		msg = errBuf.buf.String()
	}
	cerr := classify(ctx, args, err, msg)
	if errors.Is(cerr, ErrUnavailable) {
		c.markUnreachable()
	}
	return cerr
}

// output runs a call and returns its stdout (capped). On failure the
// partial stdout is returned too.
func (c *Client) output(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	out := &capBuffer{max: maxStdout}
	err := c.exec(ctx, timeout, args, nil, out, nil)
	if err == nil && out.truncated {
		err = &CmdError{Op: opName(args), Kind: errOutputTooLarge}
	}
	return out.buf.Bytes(), err
}

var errOutputTooLarge = errors.New("docker output too large")

func opName(args []string) string {
	var parts []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || len(parts) == 2 {
			break
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// classify maps a failed command to a sentinel kind.
func classify(ctx context.Context, args []string, err error, stderr string) error {
	ce := &CmdError{Op: opName(args), Detail: strings.TrimSpace(stderr), cause: err}
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		ce.ExitCode = exitErr.ExitCode()
	}
	low := strings.ToLower(stderr)
	switch {
	case ctx.Err() != nil:
		ce.Kind = ctx.Err()
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		ce.Kind = ErrUnavailable
	case containsAny(low, "cannot connect to the docker daemon", "error during connect",
		"is the docker daemon running", "docker daemon is not running",
		"permission denied while trying to connect", "failed to connect to the docker api"):
		ce.Kind = ErrUnavailable
	case len(args) > 0 && args[0] == "create" && containsAny(low, "no such image", "unable to find image"):
		ce.Kind = ErrImageMissing
	case containsAny(low, "no such container", "no such image", "no such volume",
		"no such network", "no such object", "not found"):
		ce.Kind = ErrNotFound
	case containsAny(low, "port is already allocated", "address already in use"):
		ce.Kind = ErrPortBusy
	case containsAny(low, "already in use", "already exists", "conflict"):
		ce.Kind = ErrConflict
	}
	return ce
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Ping asks the Docker daemon for its version.
func (c *Client) Ping(ctx context.Context) error {
	out, err := c.output(ctx, probeTimeout, "version", "--format", "json")
	if err != nil {
		return err
	}
	var v struct {
		Server *struct {
			Version string `json:"Version"`
		} `json:"Server"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.Server == nil {
		return &CmdError{Op: "version", Kind: ErrUnavailable, Detail: "no server version reported"}
	}
	return nil
}

// Reachable reports whether Docker answered recently. The answer is cached
// for a few seconds and invalidated when a call finds Docker unreachable.
func (c *Client) Reachable(ctx context.Context) bool {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	if !c.probeAt.IsZero() && c.now().Sub(c.probeAt) < probeTTL {
		return c.probeOK
	}
	c.probeOK = c.Ping(ctx) == nil
	c.probeAt = c.now()
	return c.probeOK
}

func (c *Client) markUnreachable() {
	if !c.probeMu.TryLock() {
		return // a probe is in progress and will record the answer
	}
	defer c.probeMu.Unlock()
	c.probeOK = false
	c.probeAt = c.now()
}

// decodeLines decodes newline-separated JSON objects ("--format json").
func decodeLines[T any](out []byte) ([]T, error) {
	var items []T
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(line, &v); err != nil {
			return nil, fmt.Errorf("decode docker output: %w", err)
		}
		items = append(items, v)
	}
	return items, nil
}

// parseTime parses Docker's RFC 3339 timestamps; the zero time and
// unparsable values yield the zero time.Time.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return time.Time{}
	}
	return t.UTC()
}
