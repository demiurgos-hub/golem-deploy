package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Container is an inspected managed container.
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

// ManagedContainers returns every managed container. The label filter is
// applied by Docker and verified again on the inspected labels.
func (c *Client) ManagedContainers(ctx context.Context) ([]Container, error) {
	ids, err := c.psIDs(ctx, managedFilter())
	if err != nil {
		return nil, err
	}
	all, err := c.inspectContainers(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, ct := range all {
		if IsManaged(ct.Labels) {
			out = append(out, ct)
		}
	}
	return out, nil
}

// ServerContainers returns the managed containers labelled with serverID,
// newest first. This label lookup is the only way to reach a server's
// container.
func (c *Client) ServerContainers(ctx context.Context, serverID int64) ([]Container, error) {
	if serverID <= 0 {
		return nil, invalid("server_id", "server id must be positive")
	}
	ids, err := c.psIDs(ctx, managedFilter(), "label="+LabelServerID+"="+strconv.FormatInt(serverID, 10))
	if err != nil {
		return nil, err
	}
	all, err := c.inspectContainers(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, ct := range all {
		if IsManaged(ct.Labels) && ct.ServerID == serverID {
			out = append(out, ct)
		}
	}
	slices.SortFunc(out, func(a, b Container) int { return b.Created.Compare(a.Created) })
	return out, nil
}

// ServerContainer returns the server's current container (a running one if
// any, else the newest), or ErrNotFound.
func (c *Client) ServerContainer(ctx context.Context, serverID int64) (Container, error) {
	list, err := c.ServerContainers(ctx, serverID)
	if err != nil {
		return Container{}, err
	}
	if len(list) == 0 {
		return Container{}, ErrNotFound
	}
	for _, ct := range list {
		if ct.Running {
			return ct, nil
		}
	}
	return list[0], nil
}

// RunServer creates and starts a server container and returns its id. The
// spec is validated; env values reach the CLI only through its environment.
func (c *Client) RunServer(ctx context.Context, spec RunSpec) (string, error) {
	args, secretEnv, err := spec.Args()
	if err != nil {
		return "", err
	}
	c.logger.Info("docker run", "server_id", spec.ServerID, "container", ContainerName(spec.ServerName), "image", spec.Image,
		"port", spec.HostPort, "env_keys", strings.Join(EnvKeys(spec.Env), ","))
	out := &capBuffer{max: 4 << 10}
	if err := c.exec(ctx, runTimeout, args, secretEnv, out, nil); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.buf.String()), nil
}

// StopServer gracefully stops the server's running containers.
func (c *Client) StopServer(ctx context.Context, serverID int64, timeout time.Duration) (stopped int, err error) {
	if timeout <= 0 {
		timeout = DefaultStopTimeout
	}
	list, err := c.ServerContainers(ctx, serverID)
	if err != nil {
		return 0, err
	}
	for _, ct := range list {
		if !ct.Running && ct.State != "restarting" && ct.State != "paused" {
			continue
		}
		args := []string{"stop", "--time", strconv.Itoa(int(timeout / time.Second)), "--", ct.ID}
		if err := c.exec(ctx, timeout+stopMargin, args, nil, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
			return stopped, err
		}
		stopped++
	}
	return stopped, nil
}

// RemoveServerContainers gracefully stops and then removes every container
// of the server. It never touches containers without the managed label.
func (c *Client) RemoveServerContainers(ctx context.Context, serverID int64, timeout time.Duration) (removed int, err error) {
	if _, err := c.StopServer(ctx, serverID, timeout); err != nil {
		return 0, err
	}
	list, err := c.ServerContainers(ctx, serverID)
	if err != nil {
		return 0, err
	}
	for _, ct := range list {
		if err := c.exec(ctx, quickTimeout, []string{"rm", "--force", "--", ct.ID}, nil, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
			return removed, err
		}
		removed++
	}
	return removed, nil
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
// the container stops or is removed.
func (c *Client) ServerLogs(ctx context.Context, serverID int64, o LogOptions, w io.Writer) error {
	if o.Tail == 0 {
		o.Tail = DefaultLogTail
	}
	if o.Tail < 0 || o.Tail > MaxLogTail {
		return invalid("tail", "tail must be between 1 and %d", MaxLogTail)
	}
	ct, err := c.ServerContainer(ctx, serverID)
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
	// One writer for both streams: exec serialises writes when Stdout and
	// Stderr are the same value.
	err = c.exec(ctx, timeout, args, nil, w, w)
	if o.Follow && ctx.Err() != nil {
		return nil
	}
	return err
}
