// Package build turns a project's checkout into a managed Docker image.
//
// The repository is untrusted. The daemon only reads a few files from the
// checkout (golem-deploy.yaml, .dockerignore, golem.yaml, go.mod,
// package.json, a custom Dockerfile) through a contained, symlink-refusing,
// size-capped reader, and never writes to it. Repository code runs only
// inside "docker build": the generated Dockerfile, its .dockerignore and the
// static add-on staging live in a private directory below the data
// directory's tmp/ that is removed after the build. Every value placed in
// the Dockerfile or the docker argv passes a strict allowlist; free-form
// commands from golem-deploy.yaml are rendered only as JSON exec-form
// arrays.
package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/extract"
	"github.com/demiurgos-hub/golem-deploy/internal/git"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/logsafe"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Defaults and limits.
const (
	DefaultTimeout   = 30 * time.Minute
	DefaultRetention = 5
	// MaxActiveBuilds bounds build jobs that are queued or running across
	// the daemon (one builds, the others wait for the build slot).
	MaxActiveBuilds = 4
	// JobPool is the job runner pool build jobs run on, with
	// MaxActiveBuilds workers (jobs.Options.Pools): a waiting build holds a
	// worker of this pool only, never one of the default pool that server
	// and other project jobs use.
	JobPool = "build"
	// KeepFailed is how many failed or cancelled builds are kept per
	// project.
	KeepFailed = 20
	// TempPrefix prefixes the private build directories below tmp/.
	TempPrefix = "build-"
	// finalTimeout bounds the store and docker calls that record a build's
	// outcome after its context ended.
	finalTimeout = 30 * time.Second
)

// User-facing details of build outcomes.
const (
	DetailCancelled        = "cancelled"
	DetailNoCheckout       = "the project has no checkout yet; pull it first (or build with pull)"
	DetailCheckoutNotIdle  = "the project checkout is not idle; pull it first (or build with pull)"
	DetailCheckoutMismatch = "the checkout HEAD could not be verified against the recorded commit; pull the project first"
	DetailDockerDown       = "Docker is not reachable on the host"
	DetailBuildFailed      = "docker build failed; see the build log"
	DetailVanished         = "the build job ended unexpectedly; see the build log"
)

// ErrImageInUse means a build's image is used by a container. Errors that
// match it are *ImageInUseError values naming the servers concerned.
var ErrImageInUse = errors.New("build: the image is used by a container")

// ImageInUseError means containers still use a build's image, which
// therefore cannot be removed. ServerIDs are the servers whose recorded
// container it is (a server keeps its container, also when stopped, until
// it is started again, possibly with another build, or deleted); Others
// counts containers that are no server's.
type ImageInUseError struct {
	ServerIDs []int64
	Others    int
}

func (e *ImageInUseError) Error() string { return ErrImageInUse.Error() }

// Is makes errors.Is(err, ErrImageInUse) true.
func (e *ImageInUseError) Is(target error) bool { return target == ErrImageInUse }

// ErrTooManyBuilds means MaxActiveBuilds builds are already queued or
// running.
var ErrTooManyBuilds = errors.New("build: too many builds are queued")

// Docker is what the service needs from *docker.Client.
type Docker interface {
	Build(ctx context.Context, spec docker.BuildSpec, out io.Writer) error
	BuiltImage(ctx context.Context, spec docker.BuildSpec) (docker.Image, error)
	RemoveBuildOutput(ctx context.Context, spec docker.BuildSpec) error
	ContainersUsingImage(ctx context.Context, id string) ([]string, error)
	RemoveImage(ctx context.Context, id string) error
}

// Options configures New.
type Options struct {
	Store  *store.Store
	Docker Docker
	// InstanceID is the installation's instance id (labels, cache ids).
	InstanceID string
	// RepoDir holds the checkouts, TempDir the private build directories
	// (both below the data directory), StaticDir the static store.
	RepoDir   string
	TempDir   string
	StaticDir string
	// HeadCommit reads the checkout's actual HEAD through the hardened git runner.
	HeadCommit func(context.Context, string) (string, error)
	// Timeout bounds one build (default DefaultTimeout); Retention is how
	// many successful builds are kept per project (default
	// DefaultRetention).
	Timeout   time.Duration
	Retention int
	// AddonLimits bounds static add-on extraction (default
	// extract.DefaultLimits()).
	AddonLimits *extract.Limits
	Logger      *slog.Logger
}

// Service runs builds and manages their records and images. It is safe for
// concurrent use.
type Service struct {
	o Options
	// slot admits one build at a time across the daemon.
	slot chan struct{}
	// mu serialises retention and deletion.
	mu sync.Mutex

	activeMu sync.Mutex
	active   int
	watchers sync.WaitGroup
}

// New returns a Service.
func New(o Options) *Service {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Retention <= 0 {
		o.Retention = DefaultRetention
	}
	if o.AddonLimits == nil {
		lim := extract.DefaultLimits()
		o.AddonLimits = &lim
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &Service{o: o, slot: make(chan struct{}, 1)}
}

// Timeout returns the per-build time limit.
func (s *Service) Timeout() time.Duration { return s.o.Timeout }

// Reserve takes one of the MaxActiveBuilds build places for a build that
// is about to be started (ErrTooManyBuilds when none is free). Pass the
// release function to Watch once the build's job exists, or call it if the
// job could not be started.
func (s *Service) Reserve() (release func(), err error) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.active >= MaxActiveBuilds {
		return nil, ErrTooManyBuilds
	}
	s.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.activeMu.Lock()
			s.active--
			s.activeMu.Unlock()
		})
	}, nil
}

// Watch waits (in the background) until the job running build id is no
// longer active, then releases its reservation and, when the job ended
// without Run recording an outcome (cancelled while queued, a panic),
// records one so no build stays queued or running.
func (s *Service) Watch(id int64, done <-chan struct{}, release func()) {
	s.watchers.Go(func() {
		<-done
		release()
		ctx, cancel := context.WithTimeout(context.Background(), finalTimeout)
		defer cancel()
		b, err := s.o.Store.GetBuild(ctx, id)
		if err != nil || (b.Status != store.BuildStatusQueued && b.Status != store.BuildStatusRunning) {
			return
		}
		status, detail := store.BuildStatusError, DetailVanished
		if b.JobID != nil {
			if j, err := s.o.Store.GetJob(ctx, *b.JobID); err == nil && j.Status == store.JobCancelled {
				status, detail = store.BuildStatusCancelled, j.Detail
			}
		}
		if err := s.o.Store.FinishBuild(ctx, id, store.BuildResult{Status: status, Detail: detail}); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.o.Logger.Error("record build outcome failed", "build_id", id, "error", safeBuildError(err))
		}
	})
}

// WaitWatchers joins the final status writes. Call after handlers have
// drained so that no further watchers can be registered.
func (s *Service) WaitWatchers() { s.watchers.Wait() }

// run is one build in progress.
type run struct {
	s      *Service
	id     int64
	log    io.Writer
	engine string
}

func (r *run) finish(ctx context.Context, res store.BuildResult) error {
	if res.EngineVersion == "" {
		res.EngineVersion = r.engine
	}
	if err := r.s.o.Store.FinishBuild(ctx, r.id, res); err != nil {
		r.s.o.Logger.Error("record build outcome failed", "build_id", r.id, "error", safeBuildError(err))
		return jobs.Fail("the build result could not be recorded", nil)
	}
	return nil
}

// fail records a failed build. The job log and detail get the same safe
// message. Raw process errors and internal file errors never reach them.
func (r *run) fail(msg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), finalTimeout)
	defer cancel()
	if err := r.finish(ctx, store.BuildResult{Status: store.BuildStatusError, Detail: msg}); err != nil {
		return err
	}
	return jobs.Fail(msg, nil)
}

// cancelled records a cancelled build and returns the context error, so
// the job ends cancelled too.
func (r *run) cancelled(ctx context.Context) error {
	finalCtx, cancel := context.WithTimeout(context.Background(), finalTimeout)
	defer cancel()
	if err := r.finish(finalCtx, store.BuildResult{Status: store.BuildStatusCancelled, Detail: DetailCancelled}); err != nil {
		return err
	}
	return ctx.Err()
}

// Run is the body of a project.build job for build id. prepare, when not
// nil, runs first in the same job (the pull of POST .../builds with
// pull:true); its *jobs.SafeError message becomes the build's detail. Run
// always records the build's outcome.
func (s *Service) Run(ctx context.Context, id int64, log io.Writer, prepare jobs.Func) error {
	r := &run{s: s, id: id, log: log}
	b, err := s.o.Store.GetBuild(ctx, id)
	if err != nil {
		return jobs.Fail("the build record could not be read", nil)
	}
	if prepare != nil {
		if err := prepare(ctx, log); err != nil {
			if ctx.Err() != nil {
				return r.cancelled(ctx)
			}
			var se *jobs.SafeError
			msg := "updating the checkout failed"
			if errors.As(err, &se) && se.Msg != "" {
				msg = "updating the checkout failed: " + se.Msg
			}
			return r.fail(msg)
		}
		fmt.Fprintln(log)
	}
	p, err := s.o.Store.GetProject(ctx, b.ProjectID)
	if err != nil {
		if ctx.Err() != nil {
			return r.cancelled(ctx)
		}
		return r.fail("the project could not be read")
	}
	checkout := git.CheckoutDir(s.o.RepoDir, p.ID)
	if p.LastCommit == "" || !git.HasCheckout(checkout) {
		return r.fail(DetailNoCheckout)
	}
	if p.Status != "idle" {
		return r.fail(DetailCheckoutNotIdle)
	}
	fmt.Fprintf(log, "Build %d of project %s: branch %s, commit %s\n", id, p.Name, p.Branch, p.LastCommit)

	select {
	case s.slot <- struct{}{}:
	default:
		fmt.Fprintln(log, "Waiting for another build to finish (one build runs at a time)")
		select {
		case s.slot <- struct{}{}:
		case <-ctx.Done():
			return r.cancelled(ctx)
		}
	}
	defer func() { <-s.slot }()
	if err := s.o.Store.MarkBuildRunning(ctx, id, p.Branch, p.LastCommit); err != nil {
		if ctx.Err() != nil {
			return r.cancelled(ctx)
		}
		return r.fail("the build could not be started")
	}
	start := time.Now()
	bctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
	defer cancel()
	err = r.build(bctx, p, checkout)
	switch {
	case err == nil:
		fmt.Fprintf(log, "Build finished in %s\n", time.Since(start).Round(time.Second))
		s.retain(p.ID, log)
		return nil
	case ctx.Err() != nil:
		return r.cancelled(ctx)
	case errors.Is(bctx.Err(), context.DeadlineExceeded):
		return r.fail(fmt.Sprintf("the build exceeded the time limit of %s", s.o.Timeout))
	}
	var ue *UserError
	var se *jobs.SafeError
	switch {
	case errors.As(err, &ue):
		fmt.Fprintf(log, "error: %s\n", ue.Msg)
		return r.fail(ue.Msg)
	case errors.As(err, &se):
		return r.fail(se.Msg)
	}
	s.o.Logger.Error("build failed", "build_id", id, "project_id", p.ID, "error", safeBuildError(err))
	return r.fail("the build failed because of an internal error; see the daemon log")
}

// build inspects the checkout, prepares the private build directory, runs
// docker build and records the verified image.
func (r *run) build(ctx context.Context, p store.Project, checkout string) error {
	log := r.log
	if r.s.o.HeadCommit == nil {
		return userErr("git is not available to verify the project checkout")
	}
	head, err := r.s.o.HeadCommit(ctx, checkout)
	if err != nil || head != p.LastCommit {
		return userErr("%s", DetailCheckoutMismatch)
	}
	plan, err := Inspect(checkout, p.AllowCustomDockerfile)
	if err != nil {
		return err
	}
	r.engine = plan.EngineVersion
	for _, n := range plan.Notes {
		fmt.Fprintln(log, n)
	}
	fmt.Fprintf(log, "Golem Engine %s, Go %s\n", plan.EngineVersion, plan.GoVersion)

	if err := os.MkdirAll(r.s.o.TempDir, 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(r.s.o.TempDir, TempPrefix+strconv.FormatInt(r.id, 10)+"-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			r.s.o.Logger.Warn("could not remove build directory; it is swept at the next startup", "build_id", r.id, "error", safeBuildError(err))
		}
	}()
	staticDir := filepath.Join(dir, "static")
	dockerDir := filepath.Join(dir, "docker")
	for _, d := range []string{staticDir, dockerDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return err
		}
	}
	addons := make([]Addon, 0, len(p.StaticAddons))
	for _, a := range p.StaticAddons {
		addons = append(addons, Addon{Name: a.Name, Target: a.Target})
	}
	if len(addons) > 0 {
		if r.s.o.StaticDir == "" {
			return userErr("the project has static add-ons but the static store is not available")
		}
		n, err := stageAddons(ctx, addons, r.s.o.StaticDir, dir, staticDir, *r.s.o.AddonLimits)
		if err != nil {
			return err
		}
		fmt.Fprintf(log, "Static add-ons: %d (%s)\n", len(addons), byteSize(n))
	}

	var dockerfile []byte
	if plan.Custom {
		dockerfile = plan.CustomDockerfile
	} else {
		dockerfile, err = Dockerfile(plan, DockerfileParams{ProjectID: p.ID, InstanceID: r.s.o.InstanceID, Addons: len(addons) > 0})
		if err != nil {
			return err
		}
	}
	dfPath := filepath.Join(dockerDir, "Dockerfile")
	if err := os.WriteFile(dfPath, dockerfile, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(dfPath+".dockerignore", Dockerignore(plan.Dockerignore), 0o600); err != nil {
		return err
	}
	if !plan.Custom {
		fmt.Fprintln(log, "Generated Dockerfile:")
		out := logsafe.NewWriter(log, "  | ")
		_, _ = out.Write(dockerfile)
		_ = out.Close()
	}
	spec := docker.BuildSpec{
		// The context is the repository root (a game in a subdirectory may
		// use files next to it, e.g. a local module replacement).
		ContextDir:    checkout,
		Dockerfile:    dfPath,
		StaticDir:     staticDir,
		IIDFile:       filepath.Join(dir, "iid"),
		MetadataFile:  filepath.Join(dir, "metadata.json"),
		ProjectName:   p.Name,
		ProjectID:     p.ID,
		Commit:        p.LastCommit,
		BuildID:       r.id,
		EngineVersion: plan.EngineVersion,
	}
	export := store.BuildExport{
		BuildID: spec.BuildID, ProjectID: spec.ProjectID, ProjectName: spec.ProjectName,
		Commit: spec.Commit, EngineVersion: spec.EngineVersion,
	}
	if err := r.s.o.Store.RecordBuildExport(ctx, export); err != nil {
		return err
	}
	published := false
	defer func() {
		if published {
			return
		}
		// The build process tree has ended. Use a fresh context to check
		// the unique tag even when the job's context was cancelled.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), finalTimeout)
		defer cancel()
		r.s.removeExport(cleanupCtx, export)
	}()
	fmt.Fprintf(log, "\nRunning docker build (time limit %s)\n", r.s.o.Timeout)
	out := logsafe.NewWriter(log, "", redactions(dir, checkout, r.s.o.TempDir, r.s.o.RepoDir, r.s.o.StaticDir)...)
	err = r.s.o.Docker.Build(ctx, spec, out)
	_ = out.Close()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, docker.ErrUnavailable) {
			return jobs.Fail(DetailDockerDown, err)
		}
		if errors.Is(err, docker.ErrBuildConfig) {
			return jobs.Fail(docker.ErrBuildConfig.Error(), nil)
		}
		return jobs.Fail(DetailBuildFailed, err)
	}
	vctx, cancel := context.WithTimeout(ctx, finalTimeout)
	defer cancel()
	img, err := r.s.o.Docker.BuiltImage(vctx, spec)
	if errors.Is(err, docker.ErrLabelMismatch) {
		return userErr("the image sets labels in the %s namespace, which is reserved for golem-deploy; remove those LABEL instructions", strings.TrimSuffix(docker.LabelPrefix, "."))
	}
	if err != nil {
		if errors.Is(err, docker.ErrUnavailable) {
			return jobs.Fail(DetailDockerDown, err)
		}
		return jobs.Fail("the built image could not be found or verified", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.finish(ctx, store.BuildResult{Status: store.BuildStatusSuccess, ImageID: img.ID, ImageTag: spec.Tag(), EngineVersion: plan.EngineVersion,
		WebTransportPath: plan.WebTransportPath, WebSocketPath: plan.WebSocketPath, SchemaFingerprint: plan.SchemaFingerprint}); err != nil {
		return err
	}
	published = true
	fmt.Fprintf(log, "Built image %s (%s, %s)\n", img.ID, spec.Tag(), byteSize(img.SizeBytes))
	if plan.SchemaFingerprint != "" {
		fmt.Fprintf(log, "Schema fingerprint %s (from the committed generated code)\n", plan.SchemaFingerprint)
	}
	if plan.WebTransportPath != DefaultWebTransportPath || plan.WebSocketPath != DefaultWebSocketPath {
		fmt.Fprintf(log, "Transport paths: WebTransport %s, WebSocket %s\n", plan.WebTransportPath, plan.WebSocketPath)
	}
	return nil
}

// safeBuildError gives the daemon a diagnostic category without including
// CLI stderr or paths embedded in filesystem and database errors.
func safeBuildError(err error) string {
	var cmd *docker.CmdError
	if errors.As(err, &cmd) {
		return fmt.Sprintf("docker %s (exit %d)", cmd.Op, cmd.ExitCode)
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("%T: %s", err, errno)
	}
	return fmt.Sprintf("%T", err)
}

// redactions maps host paths that may appear in docker output to
// placeholders (longest first, both slash styles).
func redactions(buildDir, checkout, tmp, repos, static string) []string {
	pairs := []string{}
	add := func(p, repl string) {
		if p == "" {
			return
		}
		pairs = append(pairs, p, repl)
		if s := filepath.ToSlash(p); s != p {
			pairs = append(pairs, s, repl)
		}
	}
	add(buildDir, "<build-dir>")
	add(checkout, "<repository>")
	add(tmp, "<tmp>")
	add(repos, "<repositories>")
	add(static, "<static>")
	return pairs
}

func byteSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// retain applies the retention policy to project id after a successful
// build: successful builds beyond the newest Retention are deleted with
// their images unless a server is configured with them (or a container
// uses the image); failed and cancelled builds beyond the newest
// KeepFailed are deleted. Problems are logged, never fatal.
func (s *Service) retain(projectID int64, log io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	used, err := s.o.Store.ServerIDsByBuild(ctx)
	if err != nil {
		s.o.Logger.Warn("build retention skipped", "project_id", projectID, "error", safeBuildError(err))
		return
	}
	success, failed := 0, 0
	removed := 0
	for b, err := range s.o.Store.ProjectBuilds(ctx, projectID) {
		if err != nil {
			s.o.Logger.Warn("could not finish build retention", "project_id", projectID, "error", safeBuildError(err))
			break
		}
		switch b.Status {
		case store.BuildStatusSuccess:
			success++
			if success <= s.o.Retention || len(used[b.ID]) > 0 {
				continue
			}
			if err := s.deleteLocked(ctx, b); err != nil {
				s.o.Logger.Info("old build kept", "build_id", b.ID, "reason", safeBuildError(err))
				continue
			}
			removed++
		case store.BuildStatusError, store.BuildStatusCancelled:
			failed++
			if failed <= KeepFailed {
				continue
			}
			if _, err := s.o.Store.DeleteBuildUnused(ctx, b.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				s.o.Logger.Info("old failed build kept", "build_id", b.ID, "reason", safeBuildError(err))
				continue
			}
			removed++
		}
	}
	if removed > 0 {
		fmt.Fprintf(log, "Retention: removed %d old build(s) (keeping the newest %d successful builds and builds that servers use)\n", removed, s.o.Retention)
	}
}

// Delete removes a terminal build that no server is configured with,
// together with its image (unless another successful build shares the
// image). Errors: store.ErrNotFound, store.ErrActive, store.ErrInUse,
// *ImageInUseError, docker errors (ErrUnavailable).
//
// The image rules are the same everywhere (build deletion, retention,
// project deletion, DELETE /images): an image is in use while a successful
// build records it or any container was created from it. Deleting a build
// drops its own record; its image is then removed unless another build
// still records it, and the deletion is refused while a container uses it.
func (s *Service) Delete(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.o.Store.GetBuild(ctx, id)
	if err != nil {
		return err
	}
	return s.deleteLocked(ctx, b)
}

func (s *Service) deleteLocked(ctx context.Context, b store.Build) error {
	if b.Status == store.BuildStatusQueued || b.Status == store.BuildStatusRunning {
		return store.ErrActive
	}
	if ids, err := s.o.Store.BuildServerIDs(ctx, b.ID); err != nil {
		return err
	} else if len(ids) > 0 {
		return store.ErrInUse
	}
	if b.Status == store.BuildStatusSuccess && b.ImageID != "" {
		if err := s.imageUsers(ctx, b.ImageID); err != nil {
			return err
		}
	}
	if _, err := s.o.Store.DeleteBuildUnused(ctx, b.ID); err != nil {
		return err
	}
	if b.Status == store.BuildStatusSuccess && b.ImageID != "" {
		s.removeImage(ctx, b.ImageID)
	}
	return nil
}

// imageUsers returns an *ImageInUseError when containers use the image.
func (s *Service) imageUsers(ctx context.Context, imageID string) error {
	cids, err := s.o.Docker.ContainersUsingImage(ctx, imageID)
	if err != nil || len(cids) == 0 {
		return err
	}
	rows, err := s.o.Store.ListServers(ctx)
	if err != nil {
		return err
	}
	e := &ImageInUseError{ServerIDs: []int64{}}
	for _, cid := range cids {
		i := slices.IndexFunc(rows, func(r store.Server) bool { return r.ContainerID == cid })
		if i < 0 {
			e.Others++
			continue
		}
		e.ServerIDs = append(e.ServerIDs, rows[i].ID)
	}
	slices.Sort(e.ServerIDs)
	return e
}

// CheckProjectImages returns an *ImageInUseError when containers use an
// image of any build of the project about to be deleted.
func (s *Service) CheckProjectImages(ctx context.Context, projectID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all *ImageInUseError
	seen := map[string]bool{}
	for b, err := range s.o.Store.ProjectBuilds(ctx, projectID) {
		if err != nil {
			return err
		}
		if b.Status != store.BuildStatusSuccess || b.ImageID == "" || seen[b.ImageID] {
			continue
		}
		seen[b.ImageID] = true
		err = s.imageUsers(ctx, b.ImageID)
		var e *ImageInUseError
		switch {
		case errors.As(err, &e):
			if all == nil {
				all = &ImageInUseError{ServerIDs: []int64{}}
			}
			all.ServerIDs = append(all.ServerIDs, e.ServerIDs...)
			all.Others += e.Others
		case err != nil:
			return err
		}
	}
	if all == nil {
		return nil
	}
	slices.Sort(all.ServerIDs)
	all.ServerIDs = slices.Compact(all.ServerIDs)
	return all
}

// SweepTemp removes private build directories left by a previous daemon
// process (call at startup, before builds can run).
func SweepTemp(tmp string) error {
	entries, err := os.ReadDir(tmp)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list stale build directories: %s", safeBuildError(err))
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), TempPrefix) {
			if err := os.RemoveAll(filepath.Join(tmp, e.Name())); err != nil {
				errs = append(errs, fmt.Errorf("remove stale build directory: %s", safeBuildError(err)))
			}
		}
	}
	return errors.Join(errs...)
}
