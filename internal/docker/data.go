package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
)

// Data volumes and helper containers.
//
// A server's data directory lives on a named volume the daemon creates
// with the managed, instance, server-id and server labels; its name is
// recorded on the server row and is unique per volume (server id plus a
// random part). Reading or writing a volume goes through a helper
// container: created from the server's verified build image and never
// started, it mounts only that volume and exists only for the duration of
// one "docker cp". Its random launch id is recorded by the caller before
// the create and set as the LabelHelper label, so a helper is found again
// only through that recorded id, never through labels alone.

const (
	// LabelHelper marks a helper container; its value is the helper's
	// launch id ("" on server containers, where it is set explicitly so it
	// cannot be inherited from an image).
	LabelHelper = "dev.golemengine.deploy.helper"
	// helperNamePrefix names helpers. Server container names cannot
	// contain "_", so a helper name never collides with one.
	helperNamePrefix = "golem-deploy_helper_"
	// helperEntrypoint replaces the image's entrypoint: helpers are never
	// started, and if one were, it would fail instead of running the game.
	helperEntrypoint = "/golem-deploy-helper-is-never-started"
	// helperPidsLimit bounds a helper that is started against the rules.
	helperPidsLimit = 16
	// copyTimeout bounds one docker cp of a data directory.
	copyTimeout = 30 * time.Minute
	// copyDrainLimit is how much trailing output (tar padding) is read
	// after the consumer finished before the copy is cut off.
	copyDrainLimit = 1 << 20
)

// NewDataVolumeName returns a fresh, unique volume name for a server's
// data: golem-deploy-data-<server id>-<16 hex>.
func NewDataVolumeName(serverID int64) (string, error) {
	if serverID <= 0 {
		return "", invalid("server_id", "server id must be positive")
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "golem-deploy-data-" + strconv.FormatInt(serverID, 10) + "-" + hex.EncodeToString(b), nil
}

// NewLaunchID returns a random launch id (32 lower-case hex characters)
// for a container create intent.
func NewLaunchID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// HelperName is the container name of the helper with launch id launch.
func HelperName(launch string) string { return helperNamePrefix + launch }

// IsHelper reports whether a listed container claims to be a helper. Only
// a launch id the caller recorded proves it is one.
func IsHelper(ct Container) bool { return ct.Labels[LabelHelper] != "" }

// CreateDataVolume creates the data volume name for server serverID with
// the managed, instance, server-id and server labels, then verifies that
// the volume carries them (Docker returns an existing volume of the same
// name unchanged).
func (c *Client) CreateDataVolume(ctx context.Context, serverID int64, serverName, name string) error {
	if err := c.needInstance(); err != nil {
		return err
	}
	if serverID <= 0 {
		return invalid("server_id", "server id must be positive")
	}
	if err := ValidateServerName(serverName); err != nil {
		return err
	}
	if err := ValidateVolumeName(name); err != nil {
		return err
	}
	c.logger.Info("docker volume create", "server_id", serverID, "volume", name)
	args := []string{"volume", "create",
		"--label", LabelManaged + "=" + ManagedValue,
		"--label", LabelInstance + "=" + c.instance,
		"--label", LabelServerID + "=" + strconv.FormatInt(serverID, 10),
		"--label", LabelServer + "=" + serverName,
		"--", name}
	if err := c.exec(ctx, quickTimeout, args, nil, nil, nil); err != nil {
		return err
	}
	_, err := c.DataVolume(ctx, serverID, name)
	return err
}

// DataVolume inspects the data volume name of server serverID: ErrNotFound
// when it does not exist, ErrNotManaged without the managed label and
// ErrNotOurs when it belongs to another installation or another server.
func (c *Client) DataVolume(ctx context.Context, serverID int64, name string) (Volume, error) {
	if err := c.needInstance(); err != nil {
		return Volume{}, err
	}
	if err := ValidateVolumeName(name); err != nil {
		return Volume{}, err
	}
	list, err := c.inspectVolumes(ctx, []string{name})
	if err != nil {
		return Volume{}, err
	}
	if len(list) != 1 || list[0].Name != name {
		return Volume{}, ErrNotFound
	}
	v := list[0]
	switch {
	case !IsManaged(v.Labels):
		return Volume{}, ErrNotManaged
	case !c.ours(v.Labels) || v.ServerID != serverID:
		return Volume{}, ErrNotOurs
	}
	return v, nil
}

// RemoveDataVolume removes the data volume name of server serverID after
// verifying it like DataVolume; a missing volume is not an error. A volume
// a container mounts is ErrInUse.
func (c *Client) RemoveDataVolume(ctx context.Context, serverID int64, name string) error {
	if _, err := c.DataVolume(ctx, serverID, name); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	err := c.RemoveVolume(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// HelperSpec describes a helper container.
type HelperSpec struct {
	// LaunchID is the random id the caller recorded before the create.
	LaunchID string
	// ServerID, ServerName, ProjectID, BuildID, Commit and EngineVersion
	// fill the labels like a server container's.
	ServerID      int64
	ServerName    string
	ProjectID     *int64
	BuildID       int64
	Commit        string
	EngineVersion string
	// Image is the immutable id of the server's verified build image and
	// ImageVolumes the paths it declares (each gets a tmpfs, so Docker
	// creates no anonymous volume).
	Image        string
	ImageVolumes []string
	// Volume is mounted at Target, read-only when ReadOnly is set.
	Volume   string
	Target   string
	ReadOnly bool
}

// Args validates the spec and returns the "docker create" arguments.
func (s HelperSpec) Args(instance string) ([]string, error) {
	switch {
	case !ValidInstanceID(instance):
		return nil, invalid("instance", "invalid instance id")
	case !ValidInstanceID(s.LaunchID):
		return nil, invalid("launch", "invalid launch id")
	case s.ServerID <= 0:
		return nil, invalid("server_id", "server id must be positive")
	case !imageIDRE.MatchString(s.Image):
		return nil, invalid("image", "image must be a resolved image id")
	case s.BuildID < 0:
		return nil, invalid("build_id", "build id must not be negative")
	case !mountPathOK(s.Target, false) || s.Target == "/":
		return nil, invalid("mounts", "volume target must be an absolute container path")
	}
	if err := ValidateServerName(s.ServerName); err != nil {
		return nil, err
	}
	if err := ValidateVolumeName(s.Volume); err != nil {
		return nil, err
	}
	for _, v := range []string{s.Commit, s.EngineVersion} {
		if strings.ContainsAny(v, "\x00\n\r") {
			return nil, invalid("labels", "invalid label value")
		}
	}
	tmpfs, err := RunSpec{ImageVolumes: s.ImageVolumes, DataVolume: &VolumeMount{Name: s.Volume, Target: s.Target}}.tmpfsTargets()
	if err != nil {
		return nil, err
	}
	projectID, buildID := "", ""
	if s.ProjectID != nil {
		projectID = strconv.FormatInt(*s.ProjectID, 10)
	}
	if s.BuildID > 0 {
		buildID = strconv.FormatInt(s.BuildID, 10)
	}
	args := []string{
		"create", "--pull", "never",
		"--name", HelperName(s.LaunchID),
		"--label", LabelManaged + "=" + ManagedValue,
		"--label", LabelInstance + "=" + instance,
		"--label", LabelServerID + "=" + strconv.FormatInt(s.ServerID, 10),
		"--label", LabelServer + "=" + s.ServerName,
		"--label", LabelProjectID + "=" + projectID,
		"--label", LabelCommit + "=" + s.Commit,
		"--label", LabelBuildID + "=" + buildID,
		"--label", LabelEngineVersion + "=" + s.EngineVersion,
		"--label", LabelConfigHash + "=",
		"--label", LabelLaunchID + "=",
		"--label", LabelHelper + "=" + s.LaunchID,
		"--label", LabelInspector + "=",
		"--label", LabelInspectorHub + "=",
		"--entrypoint", helperEntrypoint,
		"--restart", "no",
		"--no-healthcheck",
		"--network", "none",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(helperPidsLimit),
		"--log-driver", "none",
		"--user", strconv.Itoa(DefaultUID) + ":" + strconv.Itoa(DefaultGID),
	}
	for _, p := range tmpfs {
		args = append(args, "--tmpfs", p+":"+ImageVolumeTmpfs)
	}
	mount := "type=volume,source=" + s.Volume + ",target=" + s.Target
	if s.ReadOnly {
		mount += ",readonly"
	}
	args = append(args, "--mount", mount, "--", s.Image)
	return args, nil
}

// CreateHelper creates (and never starts) a helper container and returns
// its id.
func (c *Client) CreateHelper(ctx context.Context, spec HelperSpec) (string, error) {
	if err := c.needInstance(); err != nil {
		return "", err
	}
	args, err := spec.Args(c.instance)
	if err != nil {
		return "", err
	}
	c.logger.Info("docker create helper", "server_id", spec.ServerID, "volume", spec.Volume, "read_only", spec.ReadOnly, "image", spec.Image)
	out := &capBuffer{max: 4 << 10}
	if err := c.exec(ctx, runTimeout, args, nil, out, nil); err != nil {
		return "", err
	}
	id := strings.TrimSpace(out.buf.String())
	if !ValidContainerID(id) {
		return "", &CmdError{Op: "create", Detail: "unexpected container id output"}
	}
	return id, nil
}

// isHelper verifies that ct is the helper with launch id launch.
func (c *Client) isHelper(ct Container, launch string) bool {
	return ValidInstanceID(launch) && ValidContainerID(ct.ID) && c.ours(ct.Labels) &&
		ct.Labels[LabelHelper] == launch && ct.Name == HelperName(launch)
}

// Helper finds the helper with launch id launch: by its recorded container
// id when known, otherwise by the launch label (a create whose reply was
// lost). ErrNotFound when there is none.
func (c *Client) Helper(ctx context.Context, launch, containerID string) (Container, error) {
	if err := c.needInstance(); err != nil {
		return Container{}, err
	}
	if !ValidInstanceID(launch) {
		return Container{}, ErrNotOurs
	}
	var ids []string
	if containerID != "" {
		if !ValidContainerID(containerID) {
			return Container{}, ErrNotOurs
		}
		ids = []string{containerID}
	} else {
		var err error
		if ids, err = c.psIDs(ctx, managedFilter(), c.instanceFilter(), "label="+LabelHelper+"="+launch); err != nil {
			return Container{}, err
		}
	}
	all, err := c.inspectContainers(ctx, ids)
	if err != nil {
		return Container{}, err
	}
	for _, ct := range all {
		if c.isHelper(ct, launch) {
			return ct, nil
		}
	}
	if containerID != "" && len(all) > 0 {
		return Container{}, ErrNotOurs
	}
	return Container{}, ErrNotFound
}

// RemoveHelper removes the helper with launch id launch (see Helper) and
// reports whether there was one; a missing helper is not an error.
// Anonymous volumes go with it; the data volume, a named volume, stays.
func (c *Client) RemoveHelper(ctx context.Context, launch, containerID string) (found bool, err error) {
	ct, err := c.Helper(ctx, launch, containerID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := c.exec(ctx, quickTimeout, []string{"rm", "--force", "--volumes", "--", ct.ID}, nil, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
		return true, err
	}
	return true, nil
}

// helperPath validates a path inside a helper for docker cp.
func helperPath(p string) error {
	if !mountPathOK(p, false) || p == "/" || strings.ContainsRune(p, ':') || path.Clean(p) != p {
		return invalid("path", "invalid container path")
	}
	return nil
}

// CopyFromHelper streams "docker cp <helper>:<dir> -" (a tar whose entries
// start with the directory's base name) into consume. When consume returns
// an error the copy is cut off and that error is returned; consume must
// treat the stream as untrusted.
func (c *Client) CopyFromHelper(ctx context.Context, launch, containerID, dir string, consume func(io.Reader) error) error {
	if err := helperPath(dir); err != nil {
		return err
	}
	ct, err := c.Helper(ctx, launch, containerID)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := c.exec(cctx, copyTimeout, []string{"cp", "--", ct.ID + ":" + dir, "-"}, nil, pw, nil)
		if err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
		done <- err
	}()
	cerr := consume(pr)
	if cerr == nil {
		// Read what is left (tar padding); a stream that keeps going is
		// cut off.
		n, _ := io.Copy(io.Discard, io.LimitReader(pr, copyDrainLimit+1))
		if n > copyDrainLimit {
			cerr = fmt.Errorf("docker cp: unexpected trailing data")
		}
	}
	if cerr != nil {
		cancel()
		pr.CloseWithError(cerr)
		<-done
		return cerr
	}
	return <-done
}

// CopyToHelper extracts the tar stream src into dir inside the helper with
// "docker cp - <helper>:<dir>". The tar's uid and gid are kept. When
// produce returns an error the copy is cut off and that error is returned.
func (c *Client) CopyToHelper(ctx context.Context, launch, containerID, dir string, produce func(io.Writer) error) error {
	if err := helperPath(dir); err != nil {
		return err
	}
	ct, err := c.Helper(ctx, launch, containerID)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	perr := make(chan error, 1)
	go func() {
		err := produce(pw)
		if err != nil {
			cancel()
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
		perr <- err
	}()
	err = c.execEnv(cctx, copyTimeout, []string{"cp", "--", "-", ct.ID + ":" + dir}, c.baseEnv, nil, pr, nil, nil, false)
	pr.CloseWithError(errCopyEnded)
	if p := <-perr; p != nil {
		return p
	}
	return err
}

var errCopyEnded = errors.New("docker cp ended")
