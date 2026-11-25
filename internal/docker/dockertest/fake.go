// Package dockertest provides an in-memory fake of the docker CLI that
// implements docker.Runner. It understands exactly the argv shapes the
// docker package emits, keeps containers, images, volumes and networks in
// memory, applies label filters like Docker does, reads "--env-file" like
// the real CLI (including "/dev/fd/N" pipes passed as extra files) and
// records every call (argv, child environment and env-file content) so tests
// can assert on them.
package dockertest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
)

// InstanceID is a valid instance id for tests.
const InstanceID = "0123456789abcdef0123456789abcdef"

// IntegrationInstanceID is the instance id of tests against a real Docker
// daemon (GOLEM_DEPLOY_DOCKER_TESTS=1). It is shared by every package so
// they all accept the golem-deploy network one of them created.
const IntegrationInstanceID = "9d2c1e0f5a7b4c3d8e6f0a1b2c3d4e5f"

// ExitError is returned for a failing command, like *exec.ExitError.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return "exit status " + strconv.Itoa(e.Code) }

// ExitCode implements the interface docker uses to read exit codes.
func (e *ExitError) ExitCode() int { return e.Code }

// Call is one recorded invocation.
type Call struct {
	Args []string
	Env  []string
	// EnvFile is the content of the --env-file the call passed (read the
	// way the CLI reads it), empty when none.
	EnvFile string
}

// Container is a fake container.
type Container struct {
	ID            string
	Name          string
	Image         string // image id
	Labels        map[string]string
	State         string
	ExitCode      int
	RestartCount  int
	RestartPolicy string
	Created       time.Time
	StartedAt     time.Time
	Ports         []string // --publish values
	Mounts        []string // volume names
	// MountTargets maps each volume mount's target to its volume name;
	// ReadOnlyTargets lists the targets mounted read-only.
	MountTargets    map[string]string
	ReadOnlyTargets []string
	Entrypoint      string
	Binds           []string // --mount values of type=bind
	Tmpfs           []string // --tmpfs values
	GroupAdd        []string // --group-add values
	// Assigned are the host ports chosen for --publish values without a
	// host port ("127.0.0.1::7778/tcp"), by container port ("7778/tcp").
	Assigned map[string]int
	Network  string
	Env      map[string]string
	Logs     []string
}

// Image is a fake image.
type Image struct {
	ID     string
	Tags   []string
	Labels map[string]string
	Size   int64
	// Volumes are the image's VOLUME declarations.
	Volumes []string
	// ExitOnRun makes containers from this image exit immediately with the
	// given code instead of running.
	ExitOnRun *int
	// Logs are the lines a container from this image prints.
	Logs []string
	// AssignPort is the host port reported for a --publish without a host
	// port (Docker's dynamic assignment), e.g. the port of a test admin
	// server; zero assigns an unused port from 49152 up.
	AssignPort int
	// Content is what the image holds below directories (by absolute
	// path). Like Docker, a create copies it into an empty named volume
	// mounted at that path unless the mount says volume-nocopy.
	Content map[string][]File
}

// Volume is a fake volume. Files is its content (see File).
type Volume struct {
	Name   string
	Labels map[string]string
	Files  []File
}

// Network is a fake network. Subnet and Gateway are its IPAM
// configuration ("172.30.0.0/16" and "172.30.0.1" unless set).
type Network struct {
	Labels  map[string]string
	Options map[string]string
	Subnet  string
	Gateway string
}

// Fake is the fake docker CLI. The zero value is not usable; call New.
type Fake struct {
	mu         sync.Mutex
	calls      []Call
	containers map[string]*Container
	images     map[string]*Image
	volumes    map[string]*Volume
	networks   map[string]*Network
	seq        int
	clock      time.Time
	down       bool
	// ignoreFilters makes "ps", "image ls" and "volume ls" return every
	// object, simulating a broken or bypassed filter so tests prove the
	// client re-checks labels itself.
	ignoreFilters bool
	// buildHook decides the outcome of "docker build" (see SetBuildHook).
	buildHook func(BuildCall) BuildOutcome
	// copyHook can fail or delay "docker cp" (see SetCopyHook).
	copyHook func(CopyCall) error
	// copiedIn records the tar headers of every "docker cp -" into a
	// container.
	copiedIn [][]File
	// registry holds the images "docker pull" can download, by reference;
	// pullHook, when set, can fail or block a pull.
	registry map[string]Image
	pullHook func(ref string) error
}

// AddRegistryImage makes "docker pull ref" add img (tagged ref) to the
// host, replacing the tag on any older image.
func (f *Fake) AddRegistryImage(ref string, img Image) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if img.Labels == nil {
		img.Labels = map[string]string{}
	}
	f.registry[ref] = img
}

// SetPullHook installs fn, called with the reference of every pull before
// it runs; a returned error fails the pull (printed as the CLI would).
func (f *Fake) SetPullHook(fn func(ref string) error) {
	f.mu.Lock()
	f.pullHook = fn
	f.mu.Unlock()
}

func (f *Fake) pull(a []string, stdout io.Writer) *failure {
	ref := firstOr(positional(a))
	f.mu.Lock()
	hook := f.pullHook
	f.mu.Unlock()
	if hook != nil {
		if err := hook(ref); err != nil {
			return fail(1, "Error response from daemon: %v", err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	img, ok := f.registry[ref]
	if !ok {
		return fail(1, "Error response from daemon: manifest for %s not found: manifest unknown", ref)
	}
	for _, other := range f.images {
		other.Tags = slices.DeleteFunc(other.Tags, func(t string) bool { return t == ref })
	}
	img.Tags = append(slices.Clone(img.Tags), ref)
	if existing := f.images[img.ID]; existing != nil {
		existing.Tags = append(existing.Tags, ref)
	} else {
		f.images[img.ID] = &img
	}
	fmt.Fprintln(stdout, img.ID)
	return nil
}

// BuildCall is a "docker build" the fake received.
type BuildCall struct {
	Args       []string
	Dockerfile []byte
	// Dockerignore is the content of "<Dockerfile>.dockerignore" (nil when
	// absent).
	Dockerignore []byte
	ContextDir   string
	StaticDir    string
	Tag          string
	Labels       map[string]string
}

// BuildOutcome is what the fake's "docker build" does.
type BuildOutcome struct {
	// Output is printed (on stderr, like BuildKit's plain progress).
	Output string
	// Fail makes the build exit 1 after printing Output.
	Fail bool
	// Block makes the build wait until its context is cancelled.
	Block bool
	// Release, when Block is set and Release is not nil, ends the wait
	// when closed (the build then succeeds or fails per Fail).
	Release <-chan struct{}
	// ExtraLabels are added to the image as if set by Dockerfile LABEL
	// instructions (command-line labels still win).
	ExtraLabels map[string]string
	// NoMetadata leaves the metadata file unwritten; ContainerdStore
	// writes an iidfile that does not resolve (the config digest) and puts
	// the real id only in the metadata file, and deletes an image whose
	// last tag the build takes over, like the containerd image store.
	NoMetadata      bool
	ContainerdStore bool
	// AssignPort is the image's Image.AssignPort (for example the port of
	// a fake game admin endpoint for containers run from it).
	AssignPort int
}

// SetBuildHook installs fn as the decision of every "docker build"; nil
// builds succeed silently.
func (f *Fake) SetBuildHook(fn func(BuildCall) BuildOutcome) {
	f.mu.Lock()
	f.buildHook = fn
	f.mu.Unlock()
}

// New returns an empty fake.
func New() *Fake {
	return &Fake{
		containers: map[string]*Container{},
		images:     map[string]*Image{},
		volumes:    map[string]*Volume{},
		networks: map[string]*Network{"bridge": {Labels: map[string]string{}, Options: map[string]string{}},
			"none": {Labels: map[string]string{}, Options: map[string]string{}}},
		clock:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		registry: map[string]Image{},
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

// BuildLabels returns the labels an image built by the tested installation
// for ref carries (exactly what docker build sets from the command line),
// plus extra key/value pairs.
func BuildLabels(ref docker.BuildRef, kv ...string) map[string]string {
	m := ref.Labels(InstanceID)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// OurLabels returns the managed label, the instance label (InstanceID) and
// extra key/value pairs: what objects created by the tested installation
// carry.
func OurLabels(kv ...string) map[string]string {
	return ManagedLabels(append([]string{docker.LabelInstance, InstanceID}, kv...)...)
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

// AddNetwork adds a network with labels and driver options.
func (f *Fake) AddNetwork(name string, labels, options map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if labels == nil {
		labels = map[string]string{}
	}
	if options == nil {
		options = map[string]string{}
	}
	f.networks[name] = &Network{Labels: labels, Options: options}
}

// Network returns a copy of a network.
func (f *Fake) Network(name string) (Network, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.networks[name]
	if !ok {
		return Network{}, false
	}
	return *n, true
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

// DeleteImage removes an image (as if someone deleted it behind the
// daemon's back).
func (f *Fake) DeleteImage(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.images, id)
}

// LatestEnv returns variable key of the most recently created container
// that is not a helper ("" when there is none), e.g. the admin token a test
// admin server expects.
func (f *Fake) LatestEnv(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.sortedContainers()
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Labels[docker.LabelHelper] == "" {
			return list[i].Env[key]
		}
	}
	return ""
}

// SetContainerState changes a container's state (e.g. to simulate a crash).
func (f *Fake) SetContainerState(ref, state string, exitCode int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.findContainer(ref); c != nil {
		c.State, c.ExitCode = state, exitCode
	}
}

// RemoveContainer deletes a container (as if removed outside the daemon).
// SetPublishedPort changes the host port a container reports for a
// container port published without a host port (key like "7778/tcp").
func (f *Fake) SetPublishedPort(ref, key string, port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.findContainer(ref); c != nil {
		c.Assigned[key] = port
	}
}

func (f *Fake) RemoveContainer(ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.findContainer(ref); c != nil {
		delete(f.containers, c.ID)
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

// readEnvFile reads the --env-file argument like the CLI: "/dev/fd/N" from
// the inherited extra file N-3, anything else from the file system.
func readEnvFile(path string, extra []*os.File) ([]byte, error) {
	if n, ok := strings.CutPrefix(path, "/dev/fd/"); ok {
		fd, err := strconv.Atoi(n)
		if err != nil || fd < 3 || fd-3 >= len(extra) {
			return nil, fmt.Errorf("open %s: no such file or directory", path)
		}
		return io.ReadAll(extra[fd-3])
	}
	return os.ReadFile(path)
}

// Run implements docker.Runner.
func (f *Fake) Run(ctx context.Context, _ string, cmd docker.Cmd) error {
	defer func() {
		for _, x := range cmd.ExtraFiles {
			_ = x.Close()
		}
	}()
	call := Call{Args: slices.Clone(cmd.Args), Env: slices.Clone(cmd.Env)}
	var envFileErr error
	if p := flagValues(cmd.Args, "--env-file"); len(p) > 0 {
		b, err := readEnvFile(p[0], cmd.ExtraFiles)
		call.EnvFile, envFileErr = string(b), err
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
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
	if envFileErr != nil {
		fmt.Fprintln(stderr, "docker: "+envFileErr.Error())
		return &ExitError{Code: 125}
	}
	a := cmd.Args
	var ferr *failure
	switch {
	case has(a, "build"):
		return f.build(ctx, a, stdout, stderr)
	case has(a, "version"):
		fmt.Fprintln(stdout, `{"Client":{"Version":"fake"},"Server":{"Version":"fake"}}`)
	case has(a, "ps"):
		ferr = f.ps(a, stdout)
	case has(a, "container", "inspect"):
		ferr = f.inspectContainers(positional(a), stdout)
	case has(a, "create"):
		ferr = f.create(a, call.EnvFile, cmd.Env, stdout)
	case has(a, "start"):
		ferr = f.start(a)
	case has(a, "update"):
		ferr = f.update(a)
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
		ferr = f.imageRm(a)
	case has(a, "cp"):
		return f.cp(ctx, a, cmd.Stdin, stdout, stderr)
	case has(a, "volume", "create"):
		ferr = f.volumeCreate(a, stdout)
	case has(a, "volume", "ls"):
		ferr = f.volumeLs(a, stdout)
	case has(a, "volume", "inspect"):
		ferr = f.volumeInspect(positional(a), stdout)
	case has(a, "volume", "rm"):
		ferr = f.volumeRm(positional(a))
	case has(a, "pull"):
		ferr = f.pull(a, stdout)
	case has(a, "network", "inspect"):
		ferr = f.networkInspect(positional(a), stdout)
	case has(a, "network", "create"):
		ferr = f.networkCreate(a)
	case has(a, "network", "rm"):
		ferr = f.networkRm(positional(a))
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
			"Config":          map[string]any{"Labels": c.Labels},
			"NetworkSettings": map[string]any{"Ports": c.portMap()},
		})
	}
	writeArray(w, out)
	if len(missing) > 0 {
		return fail(1, "Error: No such container: %s", missing[0])
	}
	return nil
}

// portMap renders the container's published ports like Docker's inspect
// (only while it runs, like Docker).
func (c *Container) portMap() map[string]any {
	out := map[string]any{}
	if c.State != "running" {
		return out
	}
	for _, p := range c.Ports {
		spec, proto, _ := strings.Cut(p, "/")
		parts := strings.Split(spec, ":")
		ip, host, cport := "0.0.0.0", "", parts[len(parts)-1]
		if len(parts) >= 2 {
			host = parts[len(parts)-2]
		}
		if len(parts) >= 3 {
			ip = strings.Join(parts[:len(parts)-2], ":")
		}
		key := cport + "/" + proto
		if host == "" {
			host = strconv.Itoa(c.Assigned[key])
		}
		list, _ := out[key].([]map[string]string)
		out[key] = append(list, map[string]string{"HostIp": ip, "HostPort": host})
	}
	return out
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

func (f *Fake) create(a []string, envFile string, cliEnv []string, w io.Writer) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	pos := positional(a)
	if len(pos) == 0 {
		return fail(125, "\"docker run\" requires at least 1 argument")
	}
	if len(flagValues(a, "--env")) > 0 {
		return fail(125, "fake docker: --env is never used by golem-deploy")
	}
	img := f.findImage(pos[0])
	if img == nil {
		if v := flagValues(a, "--pull"); len(v) > 0 && v[0] == "never" {
			return fail(125, "docker: Error response from daemon: No such image: %s", pos[0])
		}
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
				if hostPort(p) == hostPort(q) && !strings.HasPrefix(hostPort(q), "/") {
					return fail(125, "docker: Error response from daemon: Bind for %s failed: port is already allocated.", q)
				}
			}
		}
	}
	// Image labels are inherited; --label values override them.
	labels := map[string]string{}
	for k, v := range img.Labels {
		labels[k] = v
	}
	for _, l := range flagValues(a, "--label") {
		k, v, _ := strings.Cut(l, "=")
		labels[k] = v
	}
	envVals := map[string]string{}
	if envFile != "" {
		lookup := func(k string) (string, bool) {
			for _, kv := range cliEnv {
				if ek, ev, _ := strings.Cut(kv, "="); ek == k {
					return ev, true
				}
			}
			return "", false
		}
		lines, err := ParseEnvFile([]byte(envFile), lookup)
		if err != nil {
			return fail(125, "docker: %v", err)
		}
		for _, l := range lines {
			k, v, _ := strings.Cut(l, "=")
			envVals[k] = v
		}
	}
	var mounts, binds, readOnly []string
	targets := map[string]string{}
	for _, m := range flagValues(a, "--mount") {
		if strings.HasPrefix(m, "type=bind,") {
			binds = append(binds, m)
		}
		if !strings.Contains(m, "type=volume") {
			continue
		}
		source, target, ro, nocopy := "", "", false, false
		for _, part := range strings.Split(m, ",") {
			switch {
			case strings.HasPrefix(part, "source="):
				source = strings.TrimPrefix(part, "source=")
			case strings.HasPrefix(part, "target="):
				target = strings.TrimPrefix(part, "target=")
			case part == "readonly":
				ro = true
			case part == "volume-nocopy":
				nocopy = true
			}
		}
		mounts = append(mounts, source)
		targets[target] = source
		if ro {
			readOnly = append(readOnly, target)
		}
		if f.volumes[source] == nil {
			// Like Docker, a missing named volume is created (unlabelled).
			f.volumes[source] = &Volume{Name: source, Labels: map[string]string{}}
		}
		if v := f.volumes[source]; len(v.Files) == 0 && !nocopy && len(img.Content[target]) > 0 {
			v.Files = slices.Clone(img.Content[target])
		}
	}
	assigned := map[string]int{}
	for _, p := range ports {
		spec, proto, _ := strings.Cut(p, "/")
		parts := strings.Split(spec, ":")
		if len(parts) >= 2 && parts[len(parts)-2] == "" {
			port := img.AssignPort
			if port == 0 {
				f.seq++
				port = 49152 + f.seq
			}
			assigned[parts[len(parts)-1]+"/"+proto] = port
		}
	}
	network := ""
	if v := flagValues(a, "--network"); len(v) > 0 {
		network = v[0]
		if _, ok := f.networks[network]; !ok {
			return fail(125, "docker: Error response from daemon: network %s not found", network)
		}
	}
	restart := "no"
	if v := flagValues(a, "--restart"); len(v) > 0 {
		restart = v[0]
	}
	c := &Container{
		ID: f.nextID(), Name: name, Image: img.ID, Labels: labels, State: "created", RestartPolicy: restart,
		Created: f.tick(), Ports: ports, Mounts: mounts, Binds: binds, Tmpfs: flagValues(a, "--tmpfs"), Network: network,
		Env: envVals, Logs: slices.Clone(img.Logs), GroupAdd: flagValues(a, "--group-add"), Assigned: assigned,
		MountTargets: targets, ReadOnlyTargets: readOnly, Entrypoint: firstOr(flagValues(a, "--entrypoint")),
	}
	f.containers[c.ID] = c
	fmt.Fprintln(w, c.ID)
	return nil
}

func (f *Fake) start(a []string) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := positional(a)
	c := f.containers[ids[0]]
	if c == nil {
		return &failure{1, "Error: No such container: " + ids[0]}
	}
	c.State, c.StartedAt = "running", f.tick()
	if img := f.images[c.Image]; img != nil && img.ExitOnRun != nil {
		c.State, c.ExitCode = "exited", *img.ExitOnRun
	}
	return nil
}

func (f *Fake) update(a []string) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.findContainer(firstOr(positional(a)))
	if c == nil {
		return fail(1, "Error response from daemon: No such container: %s", firstOr(positional(a)))
	}
	if v := flagValues(a, "--restart"); len(v) > 0 {
		c.RestartPolicy = v[0]
	}
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
		var vols map[string]any
		if len(img.Volumes) > 0 {
			vols = map[string]any{}
			for _, v := range img.Volumes {
				vols[v] = map[string]any{}
			}
		}
		out = append(out, map[string]any{
			"Id": img.ID, "RepoTags": img.Tags, "Created": "2026-01-01T00:00:00Z", "Size": img.Size,
			"Config": map[string]any{"Labels": img.Labels, "Volumes": vols},
		})
	}
	writeArray(w, out)
	if len(missing) > 0 {
		return fail(1, "Error: No such image: %s", missing[0])
	}
	return nil
}

func (f *Fake) imageRm(a []string) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	end := slices.Index(a, "--")
	if end < 0 {
		end = len(a)
	}
	force := slices.Contains(a[:end], "--force")
	for _, ref := range positional(a) {
		img := f.findImage(ref)
		if img == nil {
			return fail(1, "Error response from daemon: No such image: %s", ref)
		}
		for _, c := range f.containers {
			if c.Image == img.ID && (!force || c.State == "running") {
				return fail(1, "Error response from daemon: conflict: unable to delete %s - image is being used by container", ref)
			}
		}
		if slices.Contains(img.Tags, ref) {
			img.Tags = slices.DeleteFunc(img.Tags, func(t string) bool { return t == ref })
			if len(img.Tags) > 0 {
				continue
			}
		} else if len(img.Tags) > 1 && !force {
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

func (f *Fake) attached(network string) map[string]any {
	out := map[string]any{}
	for _, c := range f.containers {
		if c.Network == network && c.State == "running" {
			out[c.ID] = map[string]any{"Name": c.Name}
		}
	}
	return out
}

func (f *Fake) networkInspect(names []string, w io.Writer) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := firstOr(names)
	n, ok := f.networks[name]
	if !ok {
		fmt.Fprintln(w, "[]")
		return fail(1, "Error response from daemon: network %s not found", name)
	}
	subnet, gateway := n.Subnet, n.Gateway
	if subnet == "" {
		subnet, gateway = "172.30.0.0/16", "172.30.0.1"
	}
	writeArray(w, []map[string]any{{"Name": name, "Labels": n.Labels, "Options": n.Options, "Containers": f.attached(name),
		"IPAM": map[string]any{"Config": []map[string]string{{"Subnet": subnet, "Gateway": gateway}}}}})
	return nil
}

func (f *Fake) networkCreate(a []string) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := firstOr(positional(a))
	if _, ok := f.networks[name]; ok {
		return fail(1, "Error response from daemon: network with name %s already exists", name)
	}
	n := &Network{Labels: map[string]string{}, Options: map[string]string{}}
	for _, l := range flagValues(a, "--label") {
		k, v, _ := strings.Cut(l, "=")
		n.Labels[k] = v
	}
	for _, o := range flagValues(a, "--opt") {
		k, v, _ := strings.Cut(o, "=")
		n.Options[k] = v
	}
	f.networks[name] = n
	return nil
}

func (f *Fake) networkRm(names []string) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, name := range names {
		if _, ok := f.networks[name]; !ok {
			return fail(1, "Error response from daemon: network %s not found", name)
		}
		if len(f.attached(name)) > 0 {
			return fail(1, "Error response from daemon: error while removing network: network %s has active endpoints", name)
		}
		delete(f.networks, name)
	}
	return nil
}

func (f *Fake) build(ctx context.Context, a []string, stdout, stderr io.Writer) error {
	call := BuildCall{Args: slices.Clone(a), Labels: map[string]string{}}
	if v := flagValues(a, "--file"); len(v) > 0 {
		call.Dockerfile, _ = os.ReadFile(v[0])
		if b, err := os.ReadFile(v[0] + ".dockerignore"); err == nil {
			call.Dockerignore = b
		}
	}
	if v := flagValues(a, "--tag"); len(v) > 0 {
		call.Tag = v[0]
	}
	for _, l := range flagValues(a, "--label") {
		k, v, _ := strings.Cut(l, "=")
		call.Labels[k] = v
	}
	for _, c := range flagValues(a, "--build-context") {
		if v, ok := strings.CutPrefix(c, "static="); ok {
			call.StaticDir = v
		}
	}
	call.ContextDir = firstOr(positional(a))
	f.mu.Lock()
	hook := f.buildHook
	f.mu.Unlock()
	var out BuildOutcome
	if hook != nil {
		out = hook(call)
	}
	if out.Output != "" {
		fmt.Fprint(stderr, out.Output)
	}
	if out.Block {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-out.Release:
		}
	}
	if out.Fail {
		fmt.Fprintln(stderr, "ERROR: failed to build: failed to solve: process did not complete successfully: exit code: 1")
		return &ExitError{Code: 1}
	}
	labels := map[string]string{}
	for k, v := range out.ExtraLabels {
		labels[k] = v
	}
	for k, v := range call.Labels {
		labels[k] = v
	}
	f.mu.Lock()
	f.seq++
	id := fmt.Sprintf("sha256:%064x", 0xb0000000+f.seq)
	config := fmt.Sprintf("sha256:%064x", 0xcf000000+f.seq)
	if old := f.findImage(call.Tag); old != nil {
		old.Tags = slices.DeleteFunc(old.Tags, func(t string) bool { return t == call.Tag })
		if out.ContainerdStore && len(old.Tags) == 0 {
			// The containerd image store deletes an image whose last name
			// moves to another image, even while containers use it.
			delete(f.images, old.ID)
		}
	}
	f.images[id] = &Image{ID: id, Tags: []string{call.Tag}, Labels: labels, Size: 1234, AssignPort: out.AssignPort}
	f.mu.Unlock()
	iid := id
	if out.ContainerdStore {
		iid = config
	}
	if v := flagValues(a, "--iidfile"); len(v) > 0 {
		_ = os.WriteFile(v[0], []byte(iid), 0o600)
	}
	if v := flagValues(a, "--metadata-file"); len(v) > 0 && !out.NoMetadata {
		md, _ := json.Marshal(map[string]string{"containerimage.digest": id, "containerimage.config.digest": config})
		_ = os.WriteFile(v[0], md, 0o600)
	}
	fmt.Fprintln(stdout, "#1 exporting to image")
	return nil
}
