package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// BuildSpec describes one "docker build" of a project checkout. Every field
// is validated by Args; nothing from the repository reaches the argv except
// through these validated values.
type BuildSpec struct {
	// ContextDir is the build context (the checkout), Dockerfile the
	// Dockerfile (outside the context; its "<Dockerfile>.dockerignore"
	// sits next to it) and StaticDir the "static" named context. All are
	// absolute host paths.
	ContextDir string
	Dockerfile string
	StaticDir  string
	// IIDFile and MetadataFile receive the CLI's image id and build
	// metadata (absolute host paths in the build's private directory).
	IIDFile      string
	MetadataFile string
	// ProjectName and Commit form the tag golem-deploy/<name>:<commit[:12]>.
	ProjectName   string
	ProjectID     int64
	Commit        string
	BuildID       int64
	EngineVersion string
}

// BuildEnv are fixed variables added to the docker CLI's environment for
// builds: BuildKit is required (no fallback to the legacy builder), the
// CLI never runs git in the context to collect VCS information and adds no
// attestations, and the metadata file carries no provenance.
var BuildEnv = []string{
	"DOCKER_BUILDKIT=1",
	"BUILDX_GIT_INFO=false",
	"BUILDX_GIT_LABELS=false",
	"BUILDX_NO_DEFAULT_ATTESTATIONS=1",
	"BUILDX_METADATA_PROVENANCE=disabled",
	"BUILDX_METADATA_WARNINGS=false",
}

// LabelPrefix is the namespace of the daemon's labels.
const LabelPrefix = "dev.golemengine.deploy."

var (
	buildTagNameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	commitRE        = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	engineVersionRE = regexp.MustCompile(`^v[0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}(-[0-9A-Za-z.-]{1,100})?$`)
)

// Tag returns the image tag of the build.
func (s BuildSpec) Tag() string {
	c := s.Commit
	if len(c) > 12 {
		c = c[:12]
	}
	return "golem-deploy/" + s.ProjectName + ":" + c
}

// Labels returns the labels the build sets from the command line, which
// are exactly the dev.golemengine.deploy.* labels the built image must
// carry.
func (s BuildSpec) Labels(instance string) map[string]string {
	return buildLabels(instance, s.ProjectID, s.Commit, s.BuildID, s.EngineVersion)
}

// BuildRef is what a successful build recorded about its image (the builds
// row): the image a server may run and the values its labels must have.
type BuildRef struct {
	// ImageID is the full immutable image id recorded on the build.
	ImageID       string
	BuildID       int64
	ProjectID     int64
	Commit        string
	EngineVersion string
}

// Labels returns the dev.golemengine.deploy.* labels the build's image
// carries: the same set BuildSpec.Labels gave docker build.
func (r BuildRef) Labels(instance string) map[string]string {
	return buildLabels(instance, r.ProjectID, r.Commit, r.BuildID, r.EngineVersion)
}

func buildLabels(instance string, projectID int64, commit string, buildID int64, engineVersion string) map[string]string {
	return map[string]string{
		LabelManaged:       ManagedValue,
		LabelInstance:      instance,
		LabelProjectID:     strconv.FormatInt(projectID, 10),
		LabelCommit:        commit,
		LabelBuildID:       strconv.FormatInt(buildID, 10),
		LabelEngineVersion: engineVersion,
	}
}

// matchBuildLabels reports whether labels carry exactly the
// dev.golemengine.deploy.* labels in want: every one with its value, and no
// other label in the namespace. Both the build (BuiltImage) and every
// server start (BuildImage) verify an image with it.
func matchBuildLabels(labels, want map[string]string) bool {
	for k, v := range labels {
		if strings.HasPrefix(k, LabelPrefix) && want[k] != v {
			return false
		}
	}
	for k, v := range want {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// hostPathOK accepts an absolute, clean host path without control
// characters (it is a single argv element; the CLI never parses it as
// options because it is always a flag value or follows "--").
func hostPathOK(p string) bool {
	if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.HasPrefix(p, "-") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Args validates the spec and returns the "docker build" arguments.
func (s BuildSpec) Args(instance string) ([]string, error) {
	if !ValidInstanceID(instance) {
		return nil, invalid("instance", "invalid instance id")
	}
	for _, p := range []string{s.ContextDir, s.Dockerfile, s.StaticDir, s.IIDFile, s.MetadataFile} {
		if !hostPathOK(p) {
			return nil, invalid("build", "build paths must be absolute and clean")
		}
	}
	switch {
	case !buildTagNameRE.MatchString(s.ProjectName):
		return nil, invalid("build", "invalid project name")
	case s.ProjectID <= 0 || s.BuildID <= 0:
		return nil, invalid("build", "invalid project or build id")
	case !commitRE.MatchString(s.Commit):
		return nil, invalid("build", "invalid commit")
	case !engineVersionRE.MatchString(s.EngineVersion):
		return nil, invalid("build", "invalid engine version")
	}
	labels := s.Labels(instance)
	args := []string{"build",
		"--file", s.Dockerfile,
		"--tag", s.Tag(),
	}
	for _, k := range []string{LabelManaged, LabelInstance, LabelProjectID, LabelCommit, LabelBuildID, LabelEngineVersion} {
		args = append(args, "--label", k+"="+labels[k])
	}
	args = append(args,
		"--iidfile", s.IIDFile,
		"--metadata-file", s.MetadataFile,
		"--progress", "plain",
		"--provenance=false",
		"--sbom=false",
		"--load",
		"--build-context", "static="+s.StaticDir,
		"--", s.ContextDir,
	)
	return args, nil
}

// Build runs "docker build" for spec, streaming the CLI's output to out.
// Cancelling ctx kills the CLI, which cancels the build. Errors are
// *CmdError values; their Detail is never set from build output (which is
// in out), so they are safe to log.
func (c *Client) Build(ctx context.Context, spec BuildSpec, out io.Writer) error {
	if err := c.needInstance(); err != nil {
		return err
	}
	args, err := spec.Args(c.instance)
	if err != nil {
		return err
	}
	head := &capBuffer{max: maxStderr}
	c.logger.Info("docker build", "project_id", spec.ProjectID, "build_id", spec.BuildID, "tag", spec.Tag())
	err = c.execEnv(ctx, 0, args, BuildEnv, nil, out, io.MultiWriter(out, head))
	if err == nil {
		return nil
	}
	var ce *CmdError
	if errors.As(err, &ce) && ce.Kind == nil {
		// stderr went to out; classify from its beginning, keep no detail.
		if k := classify(ctx, args, ce.cause, head.buf.String()); errors.As(k, &ce) {
			ce.Detail = ""
		}
		return ce
	}
	return err
}

// ErrLabelMismatch: a built image does not carry exactly the labels the
// build set (for example a Dockerfile LABEL in the daemon's namespace).
var ErrLabelMismatch = errors.New("docker: built image labels do not match the build")

// maxMetadataBytes caps the build metadata file.
const maxMetadataBytes = 1 << 20

// BuiltImage finds the image a build produced and verifies it. Docker's
// image id is the config digest with the classic image store (what
// --iidfile holds) and the manifest digest with the containerd image store
// (containerimage.digest in the metadata file), so both are tried, each
// only as an exact full id. The image must carry exactly spec's
// dev.golemengine.deploy.* labels (ErrLabelMismatch otherwise, with the
// image returned so the caller can remove it).
func (c *Client) BuiltImage(ctx context.Context, spec BuildSpec) (Image, error) {
	if err := c.needInstance(); err != nil {
		return Image{}, err
	}
	var candidates []string
	if b, err := readCapped(spec.IIDFile, 1024); err == nil {
		candidates = append(candidates, strings.TrimSpace(string(b)))
	}
	if b, err := readCapped(spec.MetadataFile, maxMetadataBytes); err == nil {
		var md struct {
			Digest string `json:"containerimage.digest"`
			Config string `json:"containerimage.config.digest"`
		}
		if json.Unmarshal(b, &md) == nil {
			candidates = append(candidates, md.Digest, md.Config)
		}
	}
	for _, id := range candidates {
		if !imageIDRE.MatchString(id) {
			continue
		}
		list, err := c.inspectImages(ctx, []string{id})
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return Image{}, err
		}
		if len(list) != 1 || list[0].ID != id {
			continue
		}
		img := list[0]
		if !matchBuildLabels(img.Labels, spec.Labels(c.instance)) {
			return img, ErrLabelMismatch
		}
		return img, nil
	}
	return Image{}, &CmdError{Op: "image inspect", Kind: ErrNotFound, Detail: "the built image was not found"}
}

func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("file too large")
	}
	return b, nil
}

// ImageInUse reports whether any container (managed or not) was created
// from the image id.
func (c *Client) ImageInUse(ctx context.Context, id string) (bool, error) {
	if !imageIDRE.MatchString(id) {
		return false, invalid("id", "invalid image id")
	}
	return c.containersUsingImage(ctx, id)
}
