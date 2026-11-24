package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Container is an inspected container.
type Container struct {
	ID       string
	Name     string
	ImageID  string
	Labels   map[string]string
	ServerID int64
	// State is Docker's status: created, running, paused, restarting,
	// removing, exited or dead.
	State        string
	Running      bool
	ExitCode     int
	RestartCount int
	StartedAt    time.Time
	FinishedAt   time.Time
	Created      time.Time
}

// ServerRef identifies a server's container: the server id and the
// container id the daemon recorded when it created the container. It is the
// only way to reach a server's container; an empty ContainerID means the
// server has no container.
type ServerRef struct {
	ServerID    int64
	ContainerID string
}

type containerJSON struct {
	ID           string `json:"Id"`
	Name         string `json:"Name"`
	Image        string `json:"Image"`
	Created      string `json:"Created"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func (j containerJSON) container() Container {
	labels := j.Config.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return Container{
		ID:           j.ID,
		Name:         strings.TrimPrefix(j.Name, "/"),
		ImageID:      j.Image,
		Labels:       labels,
		ServerID:     serverIDLabel(labels),
		State:        j.State.Status,
		Running:      j.State.Running,
		ExitCode:     j.State.ExitCode,
		RestartCount: j.RestartCount,
		StartedAt:    parseTime(j.State.StartedAt),
		FinishedAt:   parseTime(j.State.FinishedAt),
		Created:      parseTime(j.Created),
	}
}

func managedFilter() string { return "label=" + LabelManaged + "=" + ManagedValue }

func (c *Client) instanceFilter() string { return "label=" + LabelInstance + "=" + c.instance }

// psIDs lists container ids (all states) matching the filters.
func (c *Client) psIDs(ctx context.Context, filters ...string) ([]string, error) {
	args := []string{"ps", "--all", "--no-trunc"}
	for _, f := range filters {
		args = append(args, "--filter", f)
	}
	args = append(args, "--format", "{{json .}}")
	out, err := c.output(ctx, quickTimeout, args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeLines[struct {
		ID string `json:"ID"`
	}](out)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.ID != "" {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

// inspectContainers inspects ids in one call. Containers that vanished
// between listing and inspecting are skipped.
func (c *Client) inspectContainers(ctx context.Context, ids []string) ([]Container, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := append([]string{"container", "inspect", "--"}, ids...)
	out, err := c.output(ctx, quickTimeout, args...)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	var raw []containerJSON
	if jerr := json.Unmarshal(out, &raw); jerr != nil {
		if err != nil {
			return nil, nil // every container vanished
		}
		return nil, fmt.Errorf("decode container inspect: %w", jerr)
	}
	list := make([]Container, 0, len(raw))
	for _, r := range raw {
		list = append(list, r.container())
	}
	return list, nil
}

// ManagedContainers returns every container carrying the managed label and
// this installation's instance id. The label filter is applied by Docker
// and verified again on the inspected labels. A listed container is not
// necessarily a server's container: only the container id recorded for a
// server is (see ServerRef); labels can be inherited from an image.
func (c *Client) ManagedContainers(ctx context.Context) ([]Container, error) {
	if err := c.needInstance(); err != nil {
		return nil, err
	}
	ids, err := c.psIDs(ctx, managedFilter(), c.instanceFilter())
	if err != nil {
		return nil, err
	}
	all, err := c.inspectContainers(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, ct := range all {
		if c.ours(ct.Labels) {
			out = append(out, ct)
		}
	}
	return out, nil
}

// IsServerContainer reports whether ct is the container ref points to: the
// recorded id, with our managed and instance labels and the server's id.
func (c *Client) IsServerContainer(ct Container, ref ServerRef) bool {
	return ref.ContainerID != "" && ct.ID == ref.ContainerID && c.ours(ct.Labels) && ct.ServerID == ref.ServerID
}

// ServerContainer inspects the server's recorded container. It returns
// ErrNotFound when no container is recorded or it no longer exists, and
// ErrNotOurs when the recorded id now belongs to a container without our
// labels for this server (it is then never acted on).
func (c *Client) ServerContainer(ctx context.Context, ref ServerRef) (Container, error) {
	if err := c.needInstance(); err != nil {
		return Container{}, err
	}
	if ref.ServerID <= 0 {
		return Container{}, invalid("server_id", "server id must be positive")
	}
	if ref.ContainerID == "" {
		return Container{}, ErrNotFound
	}
	if !ValidContainerID(ref.ContainerID) {
		return Container{}, ErrNotOurs
	}
	list, err := c.inspectContainers(ctx, []string{ref.ContainerID})
	if err != nil {
		return Container{}, err
	}
	if len(list) == 0 {
		return Container{}, ErrNotFound
	}
	if !c.IsServerContainer(list[0], ref) {
		return Container{}, ErrNotOurs
	}
	return list[0], nil
}

// RunServer creates and starts a server container (without a restart
// policy; see ApplyRestartPolicy) and returns its id. The spec is
// validated; env values reach the CLI only through an env file.
func (c *Client) RunServer(ctx context.Context, spec RunSpec) (string, error) {
	if err := c.needInstance(); err != nil {
		return "", err
	}
	envFile, err := spec.EnvFile()
	if err != nil {
		return "", err
	}
	if _, err := spec.Args(c.instance, "/dev/null"); err != nil { // validate before any side effect
		return "", err
	}
	c.logger.Info("docker run", "server_id", spec.ServerID, "container", ContainerName(spec.ServerName), "image", spec.Image,
		"port", spec.HostPort, "env_keys", strings.Join(EnvKeys(spec.mergedEnv()), ","))
	out := &capBuffer{max: 4 << 10}
	errBuf := &capBuffer{max: maxStderr}
	run := func(path string, extra []*os.File) error {
		args, err := spec.Args(c.instance, path)
		if err != nil {
			closeFiles(extra)
			return err
		}
		err = c.exec(ctx, runTimeout, args, extra, out, errBuf)
		var ce *CmdError
		if errors.As(err, &ce) {
			// The CLI's stderr may end up in the daemon log; keep values out.
			ce.Detail = RedactValues(strings.TrimSpace(errBuf.buf.String()), spec.mergedEnv())
			reclassify(ctx, args, ce)
		}
		return err
	}
	if len(envFile) == 0 {
		err = run("", nil)
	} else {
		err = c.envFile(envFile, run)
	}
	id := strings.TrimSpace(out.buf.String())
	if err != nil {
		if errors.Is(err, errEnvFileNotRead) && ValidContainerID(id) {
			// Created without its environment: never leave it running.
			_ = c.exec(context.WithoutCancel(ctx), quickTimeout, []string{"rm", "--force", "--volumes", "--", id}, nil, nil, nil)
		}
		return "", err
	}
	if !ValidContainerID(id) {
		return "", &CmdError{Op: "run", Detail: "unexpected container id output"}
	}
	return id, nil
}

// reclassify sets ce.Kind from its (redacted) Detail when exec could not
// classify it because stderr was redirected.
func reclassify(ctx context.Context, args []string, ce *CmdError) {
	if ce.Kind != nil {
		return
	}
	if k := classify(ctx, args, ce.cause, ce.Detail).(*CmdError).Kind; k != nil {
		ce.Kind = k
	}
}

// RedactValues replaces every line (of at least 4 bytes) of every value in
// s. Best effort: it cannot catch transformed values.
func RedactValues(s string, env map[string]string) string {
	var pieces []string
	for _, v := range env {
		for _, line := range strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == '\r' }) {
			if len(line) >= 4 {
				pieces = append(pieces, line)
			}
		}
	}
	slices.SortFunc(pieces, func(a, b string) int { return len(b) - len(a) })
	for _, p := range pieces {
		s = strings.ReplaceAll(s, p, "[redacted]")
	}
	return s
}

// ApplyRestartPolicy sets RestartPolicy on the server's container once it
// has settled.
func (c *Client) ApplyRestartPolicy(ctx context.Context, ref ServerRef) error {
	ct, err := c.ServerContainer(ctx, ref)
	if err != nil {
		return err
	}
	return c.exec(ctx, quickTimeout, []string{"update", "--restart", RestartPolicy, "--", ct.ID}, nil, nil, nil)
}

// StopServer gracefully stops the server's recorded container. It reports
// whether a running container was stopped; a missing container is not an
// error, a container that is not ours is ErrNotOurs (and is not touched).
func (c *Client) StopServer(ctx context.Context, ref ServerRef, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = DefaultStopTimeout
	}
	ct, err := c.ServerContainer(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !ct.Running && ct.State != "restarting" && ct.State != "paused" {
		return false, nil
	}
	args := []string{"stop", "--time", strconv.Itoa(int(timeout / time.Second)), "--", ct.ID}
	if err := c.exec(ctx, timeout+stopMargin, args, nil, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	return true, nil
}

// RemoveServerContainer gracefully stops and then removes the server's
// recorded container and its anonymous volumes. It reports whether a
// container was removed; a missing container is not an error, a container
// that is not ours is ErrNotOurs (and is not touched).
func (c *Client) RemoveServerContainer(ctx context.Context, ref ServerRef, timeout time.Duration) (bool, error) {
	if _, err := c.StopServer(ctx, ref, timeout); err != nil {
		return false, err
	}
	ct, err := c.ServerContainer(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := c.exec(ctx, quickTimeout, []string{"rm", "--force", "--volumes", "--", ct.ID}, nil, nil, nil); err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Log limits.
const (
	DefaultLogTail = 200
	MaxLogTail     = 5000
)

// LogOptions selects server log output.
type LogOptions struct {
	// Tail is the number of trailing lines (DefaultLogTail when zero,
	// at most MaxLogTail).
	Tail int
	// Follow keeps streaming until ctx is cancelled or the container stops.
	Follow bool
}

// ServerLogs writes the server container's stdout and stderr, interleaved
// and timestamped, to w. With Follow it streams until ctx is cancelled or
// the container stops or is removed. The output is the raw container
// output; callers sanitise it.
func (c *Client) ServerLogs(ctx context.Context, ref ServerRef, o LogOptions, w io.Writer) error {
	if o.Tail == 0 {
		o.Tail = DefaultLogTail
	}
	if o.Tail < 0 || o.Tail > MaxLogTail {
		return invalid("tail", "tail must be between 1 and %d", MaxLogTail)
	}
	ct, err := c.ServerContainer(ctx, ref)
	if err != nil {
		return err
	}
	args := []string{"logs", "--timestamps", "--tail", strconv.Itoa(o.Tail)}
	timeout := quickTimeout
	if o.Follow {
		args = append(args, "--follow")
		timeout = 0 // bounded by ctx
	}
	args = append(args, "--", ct.ID)
	// One writer for both streams: os/exec uses a single pipe (and so
	// serialises writes) when Stdout and Stderr are the same value.
	err = c.exec(ctx, timeout, args, nil, w, w)
	if o.Follow && ctx.Err() != nil {
		return nil
	}
	return err
}

// ServerLogTail returns the last n lines of the server container's output
// (for job logs).
func (c *Client) ServerLogTail(ctx context.Context, ref ServerRef, n int) ([]byte, error) {
	var buf bytes.Buffer
	err := c.ServerLogs(ctx, ref, LogOptions{Tail: n}, &capWriter{w: &buf, max: 1 << 20})
	return buf.Bytes(), err
}

// capWriter drops output beyond max bytes.
type capWriter struct {
	w   io.Writer
	max int
	n   int
}

func (c *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.max - c.n; room < len(p) {
		p = p[:max(room, 0)]
	}
	c.n += len(p)
	if len(p) > 0 {
		if _, err := c.w.Write(p); err != nil {
			return 0, err
		}
	}
	return n, nil
}
