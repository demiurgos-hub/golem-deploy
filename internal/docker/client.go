// Package docker is a typed, label-scoped wrapper around the docker CLI.
//
// Every object the daemon creates carries the managed label
// (LabelManaged=true) and every operation on an existing object first proves
// the object is managed: containers are resolved by label from a server id,
// never from a client-supplied name or id, and images and volumes are
// inspected and their label checked before they are removed. Commands run
// without a shell, with a timeout, with "--" before positional arguments and
// with JSON output. Environment values for containers are never placed in
// argv: "docker run" gets "--env KEY" and the value travels in the CLI child
// process environment.
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
	"strings"
	"sync"
	"time"
)

// Cmd is one docker CLI invocation.
type Cmd struct {
	// Args follow the binary (argv[1:]).
	Args []string
	// Env is the complete child environment (never inherited).
	Env []string
	// Stdout and Stderr receive the output. They may be the same writer.
	Stdout io.Writer
	Stderr io.Writer
}

// Runner executes docker CLI invocations. ExecRunner is the real one; tests
// inject a fake (see internal/docker/dockertest).
type Runner interface {
	Run(ctx context.Context, path string, cmd Cmd) error
}

// ExecRunner runs commands with os/exec. Cancelling ctx kills the process.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, path string, c Cmd) error {
	cmd := exec.CommandContext(ctx, path, c.Args...)
	cmd.Env = c.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
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
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Client talks to Docker through the CLI. It is safe for concurrent use.
type Client struct {
	bin     string
	binErr  error
	runner  Runner
	logger  *slog.Logger
	baseEnv []string
	now     func() time.Time

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
	c := &Client{runner: o.Runner, logger: o.Logger, now: o.Now, bin: o.Binary}
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

// BaseEnv returns the minimal environment for the docker CLI child process.
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

// exec runs one CLI call. secretEnv entries (KEY=value) are appended to the
// child environment only; they are never logged. stdout may be nil to
// discard. When stderr is nil, it is captured for error classification.
func (c *Client) exec(ctx context.Context, timeout time.Duration, args []string, secretEnv []string, stdout, stderr io.Writer) error {
	if c.binErr != nil {
		return &CmdError{Op: opName(args), Kind: ErrUnavailable, Detail: "docker CLI not found"}
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	env := make([]string, 0, len(c.baseEnv)+len(secretEnv))
	env = append(env, c.baseEnv...)
	env = append(env, secretEnv...)
	var errBuf *capBuffer
	if stderr == nil {
		errBuf = &capBuffer{max: maxStderr}
		stderr = errBuf
	}
	if stdout == nil {
		stdout = io.Discard
	}
	c.logger.Debug("docker", "op", opName(args))
	err := c.runner.Run(ctx, c.bin, Cmd{Args: args, Env: env, Stdout: stdout, Stderr: stderr})
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
