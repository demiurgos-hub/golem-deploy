package docker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The Inspector hub container (internal/inspector): one per installation,
// created from a third-party image the daemon resolved to an immutable id,
// found again only through its labels and its fixed name.
const (
	// InspectorHubName is the hub container's name. It contains "_", which
	// server names cannot, so it never collides with a server container.
	InspectorHubName = "golem-deploy_inspector"
	// InspectorHubPort is the hub's container port; it is published on
	// 127.0.0.1 only, on a port Docker assigns.
	InspectorHubPort = 8787
	// inspectorHubPids bounds the hub's processes.
	inspectorHubPids = 256
	// inspectorHubCPUs bounds the hub's CPU time (one CPU).
	inspectorHubCPUs = "1"
	// inspectorStopTimeout is the hub's graceful stop timeout.
	inspectorStopTimeout = 15 * time.Second
	// pullTimeout bounds one image download.
	pullTimeout = 15 * time.Minute
)

// HubSpec describes the Inspector hub container. The hardening matches
// RunSpec: no registry pull at create, read-only root filesystem, all
// capabilities dropped, no-new-privileges, an init process, no image health
// check, a pids, a memory and a CPU limit (one CPU), the non-root user 65532, size-limited
// noexec tmpfs mounts on /tmp and on every volume the image declares, a
// bounded json-file log, the managed network, no mounts, the hub port
// published on 127.0.0.1 only, and every dev.golemengine.deploy.* label set
// explicitly. The settings travel in an env file.
type HubSpec struct {
	// Image is the immutable id ("sha256:<64 hex>") the configured
	// reference resolved to (Client.ResolveImage).
	Image string
	// ImageVolumes are the paths the image declares as volumes.
	ImageVolumes []string
	// Env is the hub's whole environment (GOLEM_INSPECTOR_*).
	Env map[string]string
	// MemoryMB is the memory limit (at least 8; swap is not added).
	MemoryMB int
	// ConfigHash is recorded as a label so the daemon can tell whether the
	// container matches its settings.
	ConfigHash string
}

var hubEnvKeyRE = regexp.MustCompile(`^GOLEM_INSPECTOR_[A-Z0-9_]{1,64}$`)

// envFile validates the variables and returns the env-file content.
func (s HubSpec) envFile() ([]byte, error) {
	if len(s.Env) == 0 || len(s.Env) > MaxEnvVars {
		return nil, invalid("env", "the hub needs between 1 and %d variables", MaxEnvVars)
	}
	for k, v := range s.Env {
		if !hubEnvKeyRE.MatchString(k) {
			return nil, invalid("env", "invalid hub variable name")
		}
		if err := ValidateEnvValue(k, v); err != nil {
			return nil, err
		}
	}
	return encodeEnvFile(s.Env), nil
}

// Args validates the spec and returns the "docker create" arguments.
func (s HubSpec) Args(instance, envFilePath string) ([]string, error) {
	if !ValidInstanceID(instance) {
		return nil, invalid("instance", "invalid instance id")
	}
	if !imageIDRE.MatchString(s.Image) {
		return nil, invalid("image", "image must be a resolved image id")
	}
	if _, err := s.envFile(); err != nil {
		return nil, err
	}
	if envFilePath == "" || strings.ContainsAny(envFilePath, "\x00\n\r") {
		return nil, invalid("env", "an env file is required")
	}
	if s.MemoryMB < minMemoryMB {
		return nil, invalid("memory_mb", "the hub's memory limit must be at least %d MiB", minMemoryMB)
	}
	if strings.ContainsAny(s.ConfigHash, "\x00\n\r") {
		return nil, invalid("labels", "invalid label value")
	}
	tmpfs, err := RunSpec{ImageVolumes: s.ImageVolumes}.tmpfsTargets()
	if err != nil {
		return nil, err
	}
	mem := strconv.Itoa(s.MemoryMB) + "m"
	args := []string{
		"create", "--pull", "never",
		"--name", InspectorHubName,
		"--label", LabelManaged + "=" + ManagedValue,
		"--label", LabelInstance + "=" + instance,
		"--label", LabelServerID + "=",
		"--label", LabelServer + "=",
		"--label", LabelProjectID + "=",
		"--label", LabelCommit + "=",
		"--label", LabelBuildID + "=",
		"--label", LabelEngineVersion + "=",
		"--label", LabelConfigHash + "=" + s.ConfigHash,
		"--label", LabelLaunchID + "=",
		"--label", LabelHelper + "=",
		"--label", LabelInspector + "=",
		"--label", LabelInspectorHub + "=" + ManagedValue,
		// The hub is stateless; keep it serving across Docker restarts,
		// like the daemon keeps it across its own restarts.
		"--restart", "unless-stopped",
		"--init",
		"--no-healthcheck",
		"--stop-signal", "SIGTERM",
		"--stop-timeout", strconv.Itoa(int(inspectorStopTimeout / time.Second)),
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(inspectorHubPids),
		"--memory", mem, "--memory-swap", mem,
		"--cpus", inspectorHubCPUs,
		"--log-driver", "json-file",
		"--log-opt", "max-size=10m",
		"--log-opt", "max-file=3",
		"--tmpfs", "/tmp:" + ImageVolumeTmpfs,
	}
	for _, p := range tmpfs {
		args = append(args, "--tmpfs", p+":"+ImageVolumeTmpfs)
	}
	args = append(args,
		"--user", strconv.Itoa(DefaultUID)+":"+strconv.Itoa(DefaultGID),
		"--network", NetworkName,
		"--publish", "127.0.0.1::"+strconv.Itoa(InspectorHubPort)+"/tcp",
		"--env-file", envFilePath,
		"--", s.Image)
	return args, nil
}

// IsInspectorHub reports whether ct is this installation's Inspector hub:
// managed and instance labels, the hub label and the hub's name. Server and
// helper containers set the hub label explicitly to empty, and a build
// image carrying it is never run, so only the daemon's own create gives a
// container all of these.
func (c *Client) IsInspectorHub(ct Container) bool {
	return ValidContainerID(ct.ID) && c.ours(ct.Labels) && ct.Labels[LabelInspectorHub] == ManagedValue &&
		ct.Name == InspectorHubName && ct.ServerID == 0
}

// InspectorHubs returns every container that is this installation's
// Inspector hub (normally at most one), oldest first.
func (c *Client) InspectorHubs(ctx context.Context) ([]Container, error) {
	if err := c.needInstance(); err != nil {
		return nil, err
	}
	ids, err := c.psIDs(ctx, managedFilter(), c.instanceFilter(), "label="+LabelInspectorHub+"="+ManagedValue)
	if err != nil {
		return nil, err
	}
	all, err := c.inspectContainers(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, ct := range all {
		if c.IsInspectorHub(ct) {
			out = append(out, ct)
		}
	}
	return out, nil
}

// inspectorHub inspects id and verifies it is the hub.
func (c *Client) inspectorHub(ctx context.Context, id string) (Container, error) {
	if err := c.needInstance(); err != nil {
		return Container{}, err
	}
	if !ValidContainerID(id) {
		return Container{}, ErrNotOurs
	}
	list, err := c.inspectContainers(ctx, []string{id})
	if err != nil {
		return Container{}, err
	}
	if len(list) == 0 {
		return Container{}, ErrNotFound
	}
	if !c.IsInspectorHub(list[0]) {
		return Container{}, ErrNotOurs
	}
	return list[0], nil
}

// InspectorHub inspects the hub container id (ErrNotFound when it is gone,
// ErrNotOurs when the id names another container).
func (c *Client) InspectorHub(ctx context.Context, id string) (Container, error) {
	return c.inspectorHub(ctx, id)
}

// CreateInspectorHub creates the stopped hub container and returns its id.
// The variables reach the CLI only through an env file.
func (c *Client) CreateInspectorHub(ctx context.Context, spec HubSpec) (string, error) {
	if err := c.needInstance(); err != nil {
		return "", err
	}
	envFile, err := spec.envFile()
	if err != nil {
		return "", err
	}
	if _, err := spec.Args(c.instance, "/dev/null"); err != nil {
		return "", err
	}
	c.logger.Info("docker create", "container", InspectorHubName, "image", spec.Image, "env_keys", strings.Join(EnvKeys(spec.Env), ","))
	out := &capBuffer{max: 4 << 10}
	errBuf := &capBuffer{max: maxStderr}
	err = c.envFile(envFile, func(path string, extra []*os.File) error {
		args, err := spec.Args(c.instance, path)
		if err != nil {
			closeFiles(extra)
			return err
		}
		err = c.exec(ctx, runTimeout, args, extra, out, errBuf)
		var ce *CmdError
		if errors.As(err, &ce) {
			ce.Detail = RedactValues(strings.TrimSpace(errBuf.buf.String()), spec.Env)
			reclassify(ctx, args, ce)
		}
		return err
	})
	id := strings.TrimSpace(out.buf.String())
	if err != nil {
		if errors.Is(err, errEnvFileNotRead) && ValidContainerID(id) {
			_ = c.RemoveInspectorHub(context.WithoutCancel(ctx), id)
		}
		return "", err
	}
	if !ValidContainerID(id) {
		return "", &CmdError{Op: "create", Detail: "unexpected container id output"}
	}
	return id, nil
}

// StartInspectorHub starts the verified hub container id.
func (c *Client) StartInspectorHub(ctx context.Context, id string) error {
	ct, err := c.inspectorHub(ctx, id)
	if err != nil {
		return err
	}
	return c.exec(ctx, runTimeout, []string{"start", "--", ct.ID}, nil, nil, nil)
}

// RemoveInspectorHub stops and removes the verified hub container id. A
// missing container is not an error; one that is not the hub is ErrNotOurs
// and is not touched.
func (c *Client) RemoveInspectorHub(ctx context.Context, id string) error {
	ct, err := c.inspectorHub(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if ct.Running || ct.State == "restarting" || ct.State == "paused" {
		args := []string{"stop", "--time", strconv.Itoa(int(inspectorStopTimeout / time.Second)), "--", ct.ID}
		if err := c.exec(ctx, inspectorStopTimeout+stopMargin, args, nil, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	c.logger.Info("docker rm", "container", InspectorHubName, "id", ct.ID[:12])
	if err := c.exec(ctx, quickTimeout, []string{"rm", "--force", "--volumes", "--", ct.ID}, nil, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// InspectorHubLogTail returns the last n lines of the hub's output (raw;
// callers sanitise it).
func (c *Client) InspectorHubLogTail(ctx context.Context, id string, n int) ([]byte, error) {
	if n < 1 || n > MaxLogTail {
		return nil, invalid("tail", "tail must be between 1 and %d", MaxLogTail)
	}
	ct, err := c.inspectorHub(ctx, id)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w := &capWriter{w: &buf, max: 1 << 20}
	err = c.exec(ctx, quickTimeout, []string{"logs", "--timestamps", "--tail", strconv.Itoa(n), "--", ct.ID}, nil, w, w)
	return buf.Bytes(), err
}

// ValidateHubImageRef checks the configured Inspector image reference
// (--inspector-image): a name with a tag or a digest, as the docker CLI
// accepts it, never starting with "-".
func ValidateHubImageRef(ref string) error {
	if err := ValidateImageRef(ref); err != nil || strings.HasPrefix(ref, "-") || imageIDRE.MatchString(ref) || shortIDRE.MatchString(ref) {
		return invalid("image", "the Inspector image must be a reference such as ghcr.io/owner/name:tag or name@sha256:<digest>")
	}
	name, digest, hasDigest := strings.Cut(ref, "@")
	if hasDigest && !imageIDRE.MatchString(digest) {
		return invalid("image", "an image digest must be sha256:<64 hex characters>")
	}
	last := name[strings.LastIndex(name, "/")+1:]
	if strings.Count(last, ":") > 1 || strings.HasSuffix(name, ":") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return invalid("image", "the Inspector image reference is malformed")
	}
	return nil
}

// ResolveImage returns the local image ref names (ErrNotFound when the
// host does not have it). ref is a reference checked by
// ValidateHubImageRef; the result's ID is the immutable id to create
// containers from.
func (c *Client) ResolveImage(ctx context.Context, ref string) (Image, error) {
	if err := ValidateHubImageRef(ref); err != nil {
		return Image{}, err
	}
	list, err := c.inspectImages(ctx, []string{ref})
	if err != nil {
		return Image{}, err
	}
	if len(list) != 1 || !imageIDRE.MatchString(list[0].ID) {
		return Image{}, ErrNotFound
	}
	return list[0], nil
}

// PullImage downloads ref with the daemon's build client configuration
// (the one builds use), never with the operator's own client
// configuration. That configuration holds no registry credentials unless
// an operator added some there; the pull then uses them, which is fine
// because the reference is the operator's own setting. Proxies in it are
// refused.
func (c *Client) PullImage(ctx context.Context, ref string) error {
	if err := ValidateHubImageRef(ref); err != nil {
		return err
	}
	if c.buildConfigDir == "" {
		return ErrBuildConfig
	}
	env, err := c.isolatedClientEnv(ctx, c.buildConfigDir, nil)
	if err != nil {
		return err
	}
	c.logger.Info("docker pull", "image", ref)
	return c.execEnv(ctx, pullTimeout, []string{"pull", "--quiet", "--", ref}, env, nil, nil, nil, nil, false)
}
