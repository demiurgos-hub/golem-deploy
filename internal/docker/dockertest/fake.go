// Package dockertest provides an in-memory fake of the docker CLI that
// implements docker.Runner. It understands exactly the argv shapes the
// docker package emits, keeps containers, images, volumes and networks in
// memory, applies label filters like Docker does and records every call
// (argv and child environment) so tests can assert on them.
package dockertest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
)

// ExitError is returned for a failing command, like *exec.ExitError.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return "exit status " + strconv.Itoa(e.Code) }

// ExitCode implements the interface docker uses to read exit codes.
func (e *ExitError) ExitCode() int { return e.Code }

// Call is one recorded invocation.
type Call struct {
	Args []string
	Env  []string
}

// Container is a fake container.
type Container struct {
	ID           string
	Name         string
	Image        string // image id
	Labels       map[string]string
	State        string
	ExitCode     int
	RestartCount int
	Created      time.Time
	StartedAt    time.Time
	Ports        []string // --publish values
	Mounts       []string // volume names
	Env          map[string]string
	Logs         []string
}

// Image is a fake image.
type Image struct {
	ID     string
	Tags   []string
	Labels map[string]string
	Size   int64
	// ExitOnRun makes containers from this image exit immediately with the
	// given code instead of running.
	ExitOnRun *int
	// Logs are the lines a container from this image prints.
	Logs []string
}

// Volume is a fake volume.
type Volume struct {
	Name   string
	Labels map[string]string
}

// Fake is the fake docker CLI. The zero value is not usable; call New.
type Fake struct {
	mu         sync.Mutex
	calls      []Call
	containers map[string]*Container
	images     map[string]*Image
	volumes    map[string]*Volume
	networks   map[string]map[string]string
	seq        int
	clock      time.Time
	down       bool
	// ignoreFilters makes "ps", "image ls" and "volume ls" return every
	// object, simulating a broken or bypassed filter so tests prove the
	// client re-checks labels itself.
	ignoreFilters bool
}

// New returns an empty fake.
func New() *Fake {
	return &Fake{
		containers: map[string]*Container{},
		images:     map[string]*Image{},
		volumes:    map[string]*Volume{},
		networks:   map[string]map[string]string{"bridge": {}},
		clock:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// ImageID returns a deterministic full image id for n.
func ImageID(n int) string { return fmt.Sprintf("sha256:%064x", n) }

// ManagedLabels returns the managed label set plus extra key/value pairs.
func ManagedLabels(kv ...string) map[string]string {
	m := map[string]string{docker.LabelManaged: docker.ManagedValue}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// SetDown makes every command fail as if the daemon were unreachable.
func (f *Fake) SetDown(down bool) { f.mu.Lock(); f.down = down; f.mu.Unlock() }

// SetIgnoreFilters makes list commands ignore their filters.
func (f *Fake) SetIgnoreFilters(v bool) { f.mu.Lock(); f.ignoreFilters = v; f.mu.Unlock() }

// AddImage adds an image.
func (f *Fake) AddImage(img Image) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if img.Labels == nil {
		img.Labels = map[string]string{}
	}
	f.images[img.ID] = &img
}

// AddVolume adds a volume.
func (f *Fake) AddVolume(v Volume) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v.Labels == nil {
		v.Labels = map[string]string{}
	}
	f.volumes[v.Name] = &v
}

// AddNetwork adds a network with labels.
func (f *Fake) AddNetwork(name string, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if labels == nil {
		labels = map[string]string{}
	}
	f.networks[name] = labels
}

// AddContainer adds a container as if created outside golem-deploy (or by
// it, depending on the labels). Empty ID and State get defaults.
func (f *Fake) AddContainer(c Container) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.ID == "" {
		c.ID = f.nextID()
	}
	if c.State == "" {
		c.State = "running"
	}
	if c.Labels == nil {
		c.Labels = map[string]string{}
	}
	if c.Created.IsZero() {
		c.Created = f.tick()
	}
	f.containers[c.ID] = &c
	return c.ID
}

// Container returns a copy of a container by id or name.
func (f *Fake) Container(ref string) (Container, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.findContainer(ref)
	if c == nil {
		return Container{}, false
	}
	return *c, true
}

// Containers returns copies of all containers.
func (f *Fake) Containers() []Container {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Container, 0, len(f.containers))
	for _, c := range f.containers {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b Container) int { return a.Created.Compare(b.Created) })
	return out
}

// SetContainerState changes a container's state (e.g. to simulate a crash).
func (f *Fake) SetContainerState(ref, state string, exitCode int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.findContainer(ref); c != nil {
		c.State, c.ExitCode = state, exitCode
	}
}

// HasImage reports whether an image id exists.
func (f *Fake) HasImage(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.images[id] != nil
}

// HasVolume reports whether a volume exists.
func (f *Fake) HasVolume(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.volumes[name] != nil
}

// HasNetwork reports whether a network exists.
func (f *Fake) HasNetwork(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.networks[name]
	return ok
}

// Calls returns the recorded calls.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CallsTo returns the recorded calls whose argv starts with prefix.
func (f *Fake) CallsTo(prefix ...string) []Call {
	var out []Call
	for _, c := range f.Calls() {
		if len(c.Args) >= len(prefix) && slices.Equal(c.Args[:len(prefix)], prefix) {
			out = append(out, c)
		}
	}
	return out
}

// ResetCalls forgets the recorded calls.
func (f *Fake) ResetCalls() { f.mu.Lock(); f.calls = nil; f.mu.Unlock() }

func (f *Fake) nextID() string {
	f.seq++
	return fmt.Sprintf("%064x", 0xc0000000+f.seq)
}

func (f *Fake) tick() time.Time {
	f.clock = f.clock.Add(time.Second)
	return f.clock
}

func (f *Fake) findContainer(ref string) *Container {
	if ref == "" {
		return nil
	}
	if c := f.containers[ref]; c != nil {
		return c
	}
	for _, c := range f.containers {
		if c.Name == ref || strings.HasPrefix(c.ID, ref) && len(ref) >= 12 {
			return c
		}
	}
	return nil
}

func (f *Fake) findImage(ref string) *Image {
	if img := f.images[ref]; img != nil {
		return img
	}
	for _, img := range f.images {
		short := strings.TrimPrefix(img.ID, "sha256:")
		r := strings.TrimPrefix(ref, "sha256:")
		if len(r) >= 12 && strings.HasPrefix(short, r) {
			return img
		}
		if slices.Contains(img.Tags, ref) || slices.Contains(img.Tags, ref+":latest") {
			return img
		}
	}
	return nil
}

type failure struct {
	code int
	msg  string
}

func fail(code int, format string, a ...any) *failure {
	return &failure{code: code, msg: fmt.Sprintf(format, a...)}
}

// Run implements docker.Runner.
func (f *Fake) Run(ctx context.Context, _ string, cmd docker.Cmd) error {
	f.mu.Lock()
	f.calls = append(f.calls, Call{Args: slices.Clone(cmd.Args), Env: slices.Clone(cmd.Env)})
	down := f.down
	f.mu.Unlock()
	stdout, stderr := cmd.Stdout, cmd.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if down {
		fmt.Fprintln(stderr, "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
		return &ExitError{Code: 1}
	}
	a := cmd.Args
	var ferr *failure
	switch {
	case has(a, "version"):
		fmt.Fprintln(stdout, `{"Client":{"Version":"fake"},"Server":{"Version":"fake"}}`)
	case has(a, "ps"):
		ferr = f.ps(a, stdout)
	case has(a, "container", "inspect"):
		ferr = f.inspectContainers(positional(a), stdout)
	case has(a, "run"):
		ferr = f.run(a, cmd.Env, stdout)
	case has(a, "stop"):
		ferr = f.stop(positional(a))
	case has(a, "rm"):
		ferr = f.rm(positional(a))
	case has(a, "logs"):
		return f.logs(ctx, a, stdout, stderr)
	case has(a, "stats"):
		ferr = f.stats(positional(a), stdout)
	case has(a, "image", "ls"):
		ferr = f.imageLs(a, stdout)
	case has(a, "image", "inspect"):
		ferr = f.imageInspect(positional(a), stdout)
	case has(a, "image", "rm"):
		ferr = f.imageRm(positional(a))
	case has(a, "volume", "ls"):
		ferr = f.volumeLs(a, stdout)
	case has(a, "volume", "inspect"):
		ferr = f.volumeInspect(positional(a), stdout)
	case has(a, "volume", "rm"):
		ferr = f.volumeRm(positional(a))
	case has(a, "network", "inspect"):
		ferr = f.networkInspect(positional(a), stdout)
	case has(a, "network", "create"):
		ferr = f.networkCreate(a)
	default:
		ferr = fail(125, "fake docker: unsupported command %q", a)
	}
	if ferr != nil {
		fmt.Fprintln(stderr, ferr.msg)
		return &ExitError{Code: ferr.code}
	}
	return nil
}

func has(args []string, prefix ...string) bool {
	return len(args) >= len(prefix) && slices.Equal(args[:len(prefix)], prefix)
}

// positional returns the arguments after "--".
func positional(args []string) []string {
	if i := slices.Index(args, "--"); i >= 0 {
		return args[i+1:]
	}
	return nil
}

// flagValues returns every value of a repeated flag (before "--").
func flagValues(args []string, name string) []string {
	var out []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--" {
			break
		}
		if args[i] == name {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}

func labelMatch(labels map[string]string, filter string) bool {
	k, v, hasV := strings.Cut(filter, "=")
	got, ok := labels[k]
	return ok && (!hasV || got == v)
}

func (f *Fake) containerMatches(c *Container, filters []string) bool {
	if f.ignoreFilters {
		return true
	}
	for _, flt := range filters {
		kind, val, _ := strings.Cut(flt, "=")
		switch kind {
		case "label":
			if !labelMatch(c.Labels, val) {
				return false
			}
		case "ancestor":
			img := f.findImage(val)
			if img == nil || img.ID != c.Image {
				return false
			}
		case "volume":
			if !slices.Contains(c.Mounts, val) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (f *Fake) sortedContainers() []*Container {
	list := make([]*Container, 0, len(f.containers))
	for _, c := range f.containers {
		list = append(list, c)
	}
	slices.SortFunc(list, func(a, b *Container) int { return a.Created.Compare(b.Created) })
	return list
}

func (f *Fake) ps(a []string, w io.Writer) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	filters := flagValues(a, "--filter")
	for _, c := range f.sortedContainers() {
		if !f.containerMatches(c, filters) {
			continue
		}
		row, _ := json.Marshal(map[string]any{"ID": c.ID, "Names": c.Name, "State": c.State, "Mounts": strings.Join(c.Mounts, ",")})
		fmt.Fprintln(w, string(row))
	}
	return nil
}

func (f *Fake) inspectContainers(refs []string, w io.Writer) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	var missing []string
	for _, ref := range refs {
		c := f.findContainer(ref)
		if c == nil {
			missing = append(missing, ref)
			continue
		}
		started := "0001-01-01T00:00:00Z"
		if !c.StartedAt.IsZero() {
			started = c.StartedAt.Format(time.RFC3339Nano)
		}
		out = append(out, map[string]any{
			"Id":           c.ID,
			"Name":         "/" + c.Name,
			"Image":        c.Image,
			"Created":      c.Created.Format(time.RFC3339Nano),
			"RestartCount": c.RestartCount,
			"State": map[string]any{
				"Status": c.State, "Running": c.State == "running", "ExitCode": c.ExitCode,
				"StartedAt": started, "FinishedAt": "0001-01-01T00:00:00Z",
			},
			"Config": map[string]any{"Labels": c.Labels},
		})
	}
	writeArray(w, out)
	if len(missing) > 0 {
		return fail(1, "Error: No such container: %s", missing[0])
	}
	return nil
}

func writeArray(w io.Writer, v []map[string]any) {
	if v == nil {
		v = []map[string]any{}
	}
	b, _ := json.Marshal(v)
	fmt.Fprintln(w, string(b))
}

func hostPort(publish string) string {
	// [ip:]host:container/proto -> host/proto
	spec, proto, _ := strings.Cut(publish, "/")
	parts := strings.Split(spec, ":")
	if len(parts) < 2 {
		return publish
	}
	return parts[len(parts)-2] + "/" + proto
}

func (f *Fake) run(a []string, env []string, w io.Writer) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	pos := positional(a)
	if len(pos) == 0 {
		return fail(125, "\"docker run\" requires at least 1 argument")
	}
	img := f.findImage(pos[0])
	if img == nil {
		return fail(125, "Unable to find image '%s' locally", pos[0])
	}
	name := ""
	if v := flagValues(a, "--name"); len(v) > 0 {
		name = v[0]
	}
	if name != "" && f.findContainer(name) != nil {
		return fail(125, "docker: Error response from daemon: Conflict. The container name \"/%s\" is already in use by container.", name)
	}
	ports := flagValues(a, "--publish")
	for _, c := range f.containers {
		if c.State != "running" {
			continue
		}
		for _, p := range c.Ports {
			for _, q := range ports {
				if hostPort(p) == hostPort(q) {
					return fail(125, "docker: Error response from daemon: Bind for %s failed: port is already allocated.", q)
				}
			}
		}
	}
	labels := map[string]string{}
	for _, l := range flagValues(a, "--label") {
		k, v, _ := strings.Cut(l, "=")
		labels[k] = v
	}
	envVals := map[string]string{}
	for _, k := range flagValues(a, "--env") {
		if strings.Contains(k, "=") {
			return fail(125, "fake docker: env value in argv")
		}
		for _, kv := range env {
			if ek, ev, _ := strings.Cut(kv, "="); ek == k {
				envVals[k] = ev
			}
		}
	}
	var mounts []string
	for _, m := range flagValues(a, "--mount") {
		for _, part := range strings.Split(m, ",") {
			if strings.HasPrefix(part, "source=") && strings.Contains(m, "type=volume") {
				mounts = append(mounts, strings.TrimPrefix(part, "source="))
			}
		}
	}
	c := &Container{
		ID: f.nextID(), Name: name, Image: img.ID, Labels: labels, State: "running",
		Created: f.tick(), Ports: ports, Mounts: mounts, Env: envVals, Logs: slices.Clone(img.Logs),
	}
	c.StartedAt = c.Created
	if img.ExitOnRun != nil {
		c.State, c.ExitCode = "exited", *img.ExitOnRun
	}
	f.containers[c.ID] = c
	fmt.Fprintln(w, c.ID)
	return nil
}

func (f *Fake) stop(refs []string) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ref := range refs {
		c := f.findContainer(ref)
		if c == nil {
			return fail(1, "Error response from daemon: No such container: %s", ref)
		}
		if c.State == "running" || c.State == "restarting" || c.State == "paused" {
			c.State, c.ExitCode = "exited", 0
		}
	}
	return nil
}

func (f *Fake) rm(refs []string) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ref := range refs {
		c := f.findContainer(ref)
		if c == nil {
			return fail(1, "Error response from daemon: No such container: %s", ref)
		}
		delete(f.containers, c.ID)
	}
	return nil
}

func (f *Fake) logs(ctx context.Context, a []string, stdout, stderr io.Writer) error {
	refs := positional(a)
	f.mu.Lock()
	c := f.findContainer(firstOr(refs))
	if c == nil {
		f.mu.Unlock()
		fmt.Fprintln(stderr, "Error response from daemon: No such container")
		return &ExitError{Code: 1}
	}
	id := c.ID
	lines := slices.Clone(c.Logs)
	f.mu.Unlock()
	tail := len(lines)
	if v := flagValues(a, "--tail"); len(v) > 0 {
		if n, err := strconv.Atoi(v[0]); err == nil && n < tail {
			tail = n
		}
	}
	for _, l := range lines[len(lines)-tail:] {
		fmt.Fprintln(stdout, "2026-01-01T00:00:00.000000000Z "+l)
	}
	if !slices.Contains(a, "--follow") {
		return nil
	}
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			f.mu.Lock()
			c := f.containers[id]
			running := c != nil && c.State == "running"
			f.mu.Unlock()
			if !running {
				return nil
			}
		}
	}
}

func firstOr(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func (f *Fake) stats(refs []string, w io.Writer) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.findContainer(firstOr(refs))
	if c == nil {
		return fail(1, "Error response from daemon: No such container")
	}
	row, _ := json.Marshal(map[string]string{
		"ID": c.ID, "Name": c.Name, "CPUPerc": "1.50%", "MemUsage": "12MiB / 1GiB",
		"NetIO": "1.5kB / 2MB", "PIDs": "3", "BlockIO": "0B / 0B",
	})
	fmt.Fprintln(w, string(row))
	return nil
}

func (f *Fake) imageLs(a []string, w io.Writer) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	filters := flagValues(a, "--filter")
	ids := make([]string, 0, len(f.images))
	for id := range f.images {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		img := f.images[id]
		ok := true
		for _, flt := range filters {
			kind, val, _ := strings.Cut(flt, "=")
			if !f.ignoreFilters && (kind != "label" || !labelMatch(img.Labels, val)) {
				ok = false
			}
		}
		if ok {
			row, _ := json.Marshal(map[string]string{"ID": img.ID})
			fmt.Fprintln(w, string(row))
		}
	}
	return nil
}

func (f *Fake) imageInspect(refs []string, w io.Writer) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	var missing []string
	for _, ref := range refs {
		img := f.findImage(ref)
		if img == nil {
			missing = append(missing, ref)
			continue
		}
		out = append(out, map[string]any{
			"Id": img.ID, "RepoTags": img.Tags, "Created": "2026-01-01T00:00:00Z", "Size": img.Size,
			"Config": map[string]any{"Labels": img.Labels},
		})
	}
	writeArray(w, out)
	if len(missing) > 0 {
		return fail(1, "Error: No such image: %s", missing[0])
	}
	return nil
}

func (f *Fake) imageRm(refs []string) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ref := range refs {
		img := f.findImage(ref)
		if img == nil {
			return fail(1, "Error response from daemon: No such image: %s", ref)
		}
		for _, c := range f.containers {
			if c.Image == img.ID {
				return fail(1, "Error response from daemon: conflict: unable to delete %s - image is being used by container", ref)
			}
		}
		if slices.Contains(img.Tags, ref) {
			img.Tags = slices.DeleteFunc(img.Tags, func(t string) bool { return t == ref })
			if len(img.Tags) > 0 {
				continue
			}
		} else if len(img.Tags) > 1 {
			return fail(1, "Error response from daemon: conflict: unable to delete %s (must be forced) - image is referenced in multiple repositories", ref)
		}
		delete(f.images, img.ID)
	}
	return nil
}

func (f *Fake) volumeLs(a []string, w io.Writer) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	filters := flagValues(a, "--filter")
	names := make([]string, 0, len(f.volumes))
	for n := range f.volumes {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		v := f.volumes[n]
		ok := true
		for _, flt := range filters {
			kind, val, _ := strings.Cut(flt, "=")
			if !f.ignoreFilters && (kind != "label" || !labelMatch(v.Labels, val)) {
				ok = false
			}
		}
		if ok {
			row, _ := json.Marshal(map[string]string{"Name": v.Name})
			fmt.Fprintln(w, string(row))
		}
	}
	return nil
}

func (f *Fake) volumeInspect(names []string, w io.Writer) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	var missing []string
	for _, n := range names {
		v := f.volumes[n]
		if v == nil {
			missing = append(missing, n)
			continue
		}
		out = append(out, map[string]any{"Name": v.Name, "CreatedAt": "2026-01-01T00:00:00Z", "Labels": v.Labels})
	}
	writeArray(w, out)
	if len(missing) > 0 {
		return fail(1, "Error response from daemon: get %s: no such volume", missing[0])
	}
	return nil
}

func (f *Fake) volumeRm(names []string) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range names {
		if f.volumes[n] == nil {
			return fail(1, "Error response from daemon: get %s: no such volume", n)
		}
		for _, c := range f.containers {
			if slices.Contains(c.Mounts, n) {
				return fail(1, "Error response from daemon: remove %s: volume is in use", n)
			}
		}
		delete(f.volumes, n)
	}
	return nil
}

func (f *Fake) networkInspect(names []string, w io.Writer) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	labels, ok := f.networks[firstOr(names)]
	if !ok {
		fmt.Fprintln(w, "[]")
		return fail(1, "Error response from daemon: network %s not found", firstOr(names))
	}
	writeArray(w, []map[string]any{{"Name": firstOr(names), "Labels": labels}})
	return nil
}

func (f *Fake) networkCreate(a []string) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := firstOr(positional(a))
	if _, ok := f.networks[name]; ok {
		return fail(1, "Error response from daemon: network with name %s already exists", name)
	}
	labels := map[string]string{}
	for _, l := range flagValues(a, "--label") {
		k, v, _ := strings.Cut(l, "=")
		labels[k] = v
	}
	f.networks[name] = labels
	return nil
}
