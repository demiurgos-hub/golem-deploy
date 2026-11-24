package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/git"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/staticfiles"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// GitClient is what the project handlers need from *git.Runner; tests
// inject a fake.
type GitClient interface {
	Clone(ctx context.Context, remote git.Remote, dir string, log io.Writer) (git.Checkout, error)
	Pull(ctx context.Context, remote git.Remote, dir string, log io.Writer) (git.Checkout, error)
	SwitchBranch(ctx context.Context, remote git.Remote, dir string, log io.Writer) (git.Checkout, error)
	ListRemoteBranches(ctx context.Context, remote git.Remote) ([]string, error)
}

// Remote branch listing limits: GET .../branches is synchronous, so it has a
// short timeout, results are cached per project and concurrent listings
// are bounded.
const (
	branchListTimeout  = 30 * time.Second
	branchCacheTTL     = 30 * time.Second
	branchErrorTTL     = 10 * time.Second
	maxBranchListings  = 4
	finalUpdateTimeout = 10 * time.Second
)

// projectState is the project handlers' share of the Server.
type projectState struct {
	git     GitClient
	repoDir string
	tmpDir  string
	// mu serialises the decision to start a project job or delete a
	// project with the job start or deletion itself, so a delete can never
	// race with a job starting on the same project.
	mu       sync.Mutex
	addonsMu sync.Mutex // serializes static references with static file mutations
	branches branchCache
}

func newProjectState(d Deps) *projectState {
	return &projectState{
		git:      d.Git,
		repoDir:  d.RepoDir,
		tmpDir:   d.TempDir,
		branches: branchCache{entries: map[int64]*branchEntry{}, sem: make(chan struct{}, maxBranchListings)},
	}
}

var projectNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func toAPIProject(p store.Project) apitypes.Project {
	addons := make([]apitypes.StaticAddon, 0, len(p.StaticAddons))
	for _, a := range p.StaticAddons {
		addons = append(addons, apitypes.StaticAddon{Name: a.Name, Target: a.Target})
	}
	return apitypes.Project{
		StaticAddons:          addons,
		AllowCustomDockerfile: p.AllowCustomDockerfile,
		ID:                    p.ID,
		Name:                  p.Name,
		RepoURL:               p.RepoURL,
		Branch:                p.Branch,
		AuthType:              apitypes.ProjectAuthType(p.AuthType),
		AuthUsername:          p.AuthUsername,
		HasSecret:             len(p.AuthSecret) > 0,
		LastCommit:            p.LastCommit,
		LastPulledAt:          utcPtr(p.LastPulledAt),
		Status:                apitypes.ProjectStatus(p.Status),
		CreatedAt:             p.CreatedAt.UTC(),
		UpdatedAt:             p.UpdatedAt.UTC(),
	}
}

func (s *Server) loadProject(r *http.Request) (store.Project, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return store.Project{}, err
	}
	p, err := s.store.GetProject(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Project{}, notFound("project")
	}
	if err != nil {
		return store.Project{}, internal(err)
	}
	return p, nil
}

// parsedAuth is validated credential input.
type parsedAuth struct {
	method   git.AuthMethod
	username string
	secret   []byte // password or normalised private key; nil for none
}

func (a parsedAuth) typeName() string { return a.method.String() }

// parseAuth validates the ProjectAuth union against the URL kind.
func parseAuth(a apitypes.ProjectAuth, kind git.Kind) (parsedAuth, error) {
	switch a.Type {
	case apitypes.ProjectAuthTypeNone:
		switch {
		case a.Username != "":
			return parsedAuth{}, invalidField("auth.username", `username is not allowed with auth type "none"`)
		case a.Password != "":
			return parsedAuth{}, invalidField("auth.password", `password is not allowed with auth type "none"`)
		case a.PrivateKey != "":
			return parsedAuth{}, invalidField("auth.private_key", `private_key is not allowed with auth type "none"`)
		}
		// SSH remotes need a key of their own (ssh would otherwise offer
		// the daemon user's keys).
		if err := git.CheckAuthKind(git.AuthNone, kind); err != nil {
			return parsedAuth{}, invalidField("auth.type", err.Error())
		}
		return parsedAuth{method: git.AuthNone}, nil
	case apitypes.ProjectAuthTypeHTTPS:
		if a.PrivateKey != "" {
			return parsedAuth{}, invalidField("auth.private_key", `private_key is not allowed with auth type "https"`)
		}
		if err := git.CheckAuthKind(git.AuthHTTPS, kind); err != nil {
			return parsedAuth{}, invalidField("auth.type", err.Error())
		}
		if err := git.ValidateUsername(a.Username); err != nil {
			return parsedAuth{}, invalidField("auth.username", err.Error())
		}
		if err := git.ValidatePassword(a.Password); err != nil {
			return parsedAuth{}, invalidField("auth.password", err.Error())
		}
		return parsedAuth{method: git.AuthHTTPS, username: a.Username, secret: []byte(a.Password)}, nil
	case apitypes.ProjectAuthTypeSSH:
		switch {
		case a.Username != "":
			return parsedAuth{}, invalidField("auth.username", `username is not allowed with auth type "ssh"; put the SSH user in the repository URL`)
		case a.Password != "":
			return parsedAuth{}, invalidField("auth.password", `password is not allowed with auth type "ssh"`)
		}
		if err := git.CheckAuthKind(git.AuthSSH, kind); err != nil {
			return parsedAuth{}, invalidField("auth.type", err.Error())
		}
		key, err := git.PrepareSSHKey(a.PrivateKey)
		if err != nil {
			return parsedAuth{}, invalidField("auth.private_key", err.Error())
		}
		return parsedAuth{method: git.AuthSSH, secret: key}, nil
	case "":
		return parsedAuth{}, invalidField("auth.type", `auth.type is required ("none", "https" or "ssh")`)
	}
	return parsedAuth{}, invalidField("auth.type", `auth.type must be "none", "https" or "ssh"`)
}

func authMethodOf(t string) git.AuthMethod {
	switch t {
	case string(apitypes.ProjectAuthTypeHTTPS):
		return git.AuthHTTPS
	case string(apitypes.ProjectAuthTypeSSH):
		return git.AuthSSH
	}
	return git.AuthNone
}

// sealAuth encrypts a secret for project id; nil stays nil.
func (s *Server) sealAuth(id int64, secret []byte) ([]byte, error) {
	if secret == nil {
		return nil, nil
	}
	return s.secrets.Seal(secrets.ProjectAuthPurpose(id), secret)
}

// remoteFor builds the git remote of p, decrypting its credentials.
func (s *Server) remoteFor(p store.Project) (git.Remote, error) {
	r := git.Remote{URL: p.RepoURL, Auth: git.Auth{Method: authMethodOf(p.AuthType)}}
	if r.Auth.Method == git.AuthNone {
		return r, nil
	}
	if len(p.AuthSecret) == 0 {
		return git.Remote{}, errors.New("project credentials are missing")
	}
	pt, err := s.secrets.Open(secrets.ProjectAuthPurpose(p.ID), p.AuthSecret)
	if err != nil {
		return git.Remote{}, err
	}
	switch r.Auth.Method {
	case git.AuthHTTPS:
		r.Auth.Username = p.AuthUsername
		r.Auth.Password = string(pt)
	case git.AuthSSH:
		r.Auth.PrivateKey = pt
	}
	return r, nil
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) error {
	list, err := s.store.ListProjects(r.Context())
	if err != nil {
		return internal(err)
	}
	out := apitypes.ProjectList{Items: make([]apitypes.Project, 0, len(list))}
	for _, p := range list {
		out.Items = append(out.Items, toAPIProject(p))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, toAPIProject(p))
	return nil
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) error {
	var req apitypes.CreateProjectRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if !projectNameRE.MatchString(req.Name) {
		return invalidField("name", "name must be 1-63 characters of a-z, 0-9 and '-', starting with a letter or digit")
	}
	kind, err := git.ValidateRemoteURL(req.RepoURL)
	if err != nil {
		return invalidField("repo_url", err.Error())
	}
	if req.Branch != "" {
		if err := git.ValidateBranch(req.Branch); err != nil {
			return invalidField("branch", err.Error())
		}
	}
	auth, err := parseAuth(req.Auth, kind)
	if err != nil {
		return err
	}
	if s.proj.git == nil {
		return newError(http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable, "git is not available on the host")
	}

	s.proj.mu.Lock()
	defer s.proj.mu.Unlock()
	var seal store.SealFunc
	if auth.secret != nil {
		seal = func(id int64) ([]byte, error) { return s.sealAuth(id, auth.secret) }
	}
	p, err := s.store.CreateProject(r.Context(), store.Project{
		Name:         req.Name,
		RepoURL:      req.RepoURL,
		Branch:       req.Branch,
		AuthType:     auth.typeName(),
		AuthUsername: auth.username,
		Status:       string(apitypes.ProjectStatusCloning),
	}, seal)
	if errors.Is(err, store.ErrConflict) {
		return conflict(apitypes.ErrorCodeNameTaken, "a project with that name already exists")
	}
	if err != nil {
		return internal(err)
	}
	job, err := s.startJob(r, apitypes.JobKindProjectClone, apitypes.SubjectTypeProject, p.ID, s.projectJob(p.ID, opClone, p.Branch))
	if err != nil {
		_ = s.store.SetProjectStatus(context.WithoutCancel(r.Context()), p.ID, string(apitypes.ProjectStatusError))
		return err
	}
	s.logger.Info("project created", "request_id", requestIDFrom(r.Context()), "by", identityFrom(r).Account.ID,
		"project_id", p.ID, "name", p.Name, "auth_type", p.AuthType, "job_id", job.ID)
	writeJSON(w, http.StatusCreated, apitypes.CreateProjectResponse{Project: toAPIProject(p), Job: job})
	return nil
}

// handleUpdateProject changes the remote URL and/or credentials. It never
// runs git: a new URL is written to the checkout's origin remote by the
// next pull or branch switch, which then hard-resets to whatever the new
// remote has (no re-clone). Stored credentials are only kept for a new URL
// on the same scheme, host and port; otherwise auth must be sent too.
func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r)
	if err != nil {
		return err
	}
	var req apitypes.UpdateProjectRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	url := p.RepoURL
	if req.RepoURL != nil {
		url = *req.RepoURL
	}
	kind, err := git.ValidateRemoteURL(url)
	if err != nil {
		return invalidField("repo_url", err.Error())
	}
	upd := store.ProjectRemoteUpdate{AllowCustomDockerfile: req.AllowCustomDockerfile}
	if req.RepoURL != nil {
		upd.RepoURL = &url
	}
	if req.Auth != nil {
		auth, err := parseAuth(*req.Auth, kind)
		if err != nil {
			return err
		}
		sealed, err := s.sealAuth(p.ID, auth.secret)
		if err != nil {
			return internal(err)
		}
		upd.Auth = &store.ProjectAuthUpdate{Type: auth.typeName(), Username: auth.username, Secret: sealed}
	} else if req.RepoURL != nil && !git.SameOrigin(p.RepoURL, url) {
		return invalidField("auth", "the new repository URL has a different scheme, host or port, and the stored credentials "+
			`were set for the previous one; send auth in the same request (new credentials, or {"type":"none"} for a public https repository)`)
	} else if err := git.CheckAuthKind(authMethodOf(p.AuthType), kind); err != nil {
		return invalidField("repo_url", err.Error()+"; send new auth together with the URL")
	}
	if req.StaticAddons != nil {
		s.proj.addonsMu.Lock()
		defer s.proj.addonsMu.Unlock()
		addons, err := s.checkAddons(*req.StaticAddons)
		if err != nil {
			return err
		}
		upd.StaticAddons = &addons
	}
	if upd.RepoURL == nil && upd.Auth == nil && upd.StaticAddons == nil && upd.AllowCustomDockerfile == nil {
		writeJSON(w, http.StatusOK, toAPIProject(p))
		return nil
	}
	p, err = s.store.UpdateProjectRemote(r.Context(), p.ID, upd)
	if errors.Is(err, store.ErrNotFound) {
		return notFound("project")
	}
	if err != nil {
		return internal(err)
	}
	s.proj.branches.forget(p.ID)
	s.logger.Info("project updated", "request_id", requestIDFrom(r.Context()), "by", identityFrom(r).Account.ID,
		"project_id", p.ID, "url_changed", upd.RepoURL != nil, "auth_changed", upd.Auth != nil, "auth_type", p.AuthType,
		"addons_changed", upd.StaticAddons != nil)
	writeJSON(w, http.StatusOK, toAPIProject(p))
	return nil
}

// checkAddons validates static add-ons for PATCH: names and targets
// (build.ValidateAddons) and that every file exists in the static store as
// a ZIP archive.
func (s *Server) checkAddons(in []apitypes.StaticAddon) ([]store.StaticAddon, error) {
	list := make([]build.Addon, 0, len(in))
	for _, a := range in {
		list = append(list, build.Addon{Name: a.Name, Target: a.Target})
	}
	norm, err := build.ValidateAddons(list)
	var fe *build.FieldError
	if errors.As(err, &fe) {
		return nil, invalidField(fe.Field, fe.Msg)
	}
	if err != nil {
		return nil, internal(err)
	}
	out := make([]store.StaticAddon, 0, len(norm))
	if len(norm) > 0 {
		if s.static == nil {
			return nil, newError(http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable, "the static file store is not available")
		}
		files, err := s.static.List()
		if err != nil {
			return nil, internal(err)
		}
		zips := map[string]bool{}
		for _, f := range files {
			zips[staticfiles.NameKey(f.Name)] = f.IsZip
		}
		for i, a := range norm {
			isZip, ok := zips[staticfiles.NameKey(a.Name)]
			switch {
			case !ok:
				return nil, invalidField(fmt.Sprintf("static_addons[%d].name", i), "no static file has this name")
			case !isZip:
				return nil, invalidField(fmt.Sprintf("static_addons[%d].name", i), "the static file is not a ZIP archive")
			}
			out = append(out, store.StaticAddon{Name: a.Name, Target: a.Target})
		}
	}
	return out, nil
}

// handleDeleteProject commits the database deletion before removing the
// checkout and build images. Startup reconciliation retries leftovers.
func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) error {
	s.proj.mu.Lock()
	defer s.proj.mu.Unlock()
	p, err := s.loadProject(r)
	if err != nil {
		return err
	}
	if _, busy := s.jobs.ActiveFor(jobs.Subject{Type: string(apitypes.SubjectTypeProject), ID: jobs.SubjectID(p.ID)}); busy {
		return conflict(apitypes.ErrorCodeJobInProgress, "a job is running for this project; wait for it or cancel it")
	}
	if err := s.projectBuildsInUse(r.Context(), p.ID); err != nil {
		return err
	}
	if s.builds != nil {
		// The project's build images go with it, so the same rule as for
		// deleting a build applies: refused while a container uses one.
		switch err := s.builds.CheckProjectImages(r.Context(), p.ID); {
		case errors.Is(err, build.ErrImageInUse):
			return imageInUseConflict(err, "an image of this project's builds")
		case errors.Is(err, docker.ErrUnavailable):
			return dockerUnavailable(err)
		case err != nil:
			return internal(err)
		}
	}
	s.proj.addonsMu.Lock()
	err = s.store.DeleteProjectUnused(r.Context(), p.ID)
	s.proj.addonsMu.Unlock()
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			return notFound("project")
		case errors.Is(err, store.ErrInUse):
			if e := s.projectBuildsInUse(r.Context(), p.ID); e != nil {
				return e
			}
			return conflict(apitypes.ErrorCodeConflict, "servers are configured with builds of this project")
		}
		return internal(err)
	}
	if s.proj.repoDir != "" {
		if err := git.RemoveCheckout(s.proj.repoDir, p.ID, s.proj.tmpDir); err != nil {
			s.logger.Warn("could not fully remove project checkout; cleanup is retried at startup", "project_id", p.ID)
		}
	}
	s.proj.branches.forget(p.ID)
	s.removeProjectImages()
	s.logger.Info("project deleted", "request_id", requestIDFrom(r.Context()), "by", identityFrom(r).Account.ID, "project_id", p.ID, "name", p.Name)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// needsClone reports whether p has no usable checkout, in which case pull
// and branch switch start a clone instead.
func (s *Server) needsClone(p store.Project) bool {
	return !git.HasCheckout(git.CheckoutDir(s.proj.repoDir, p.ID)) || p.Branch == "" || p.LastCommit == ""
}

func (s *Server) handlePullProject(w http.ResponseWriter, r *http.Request) error {
	if s.proj.git == nil {
		return newError(http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable, "git is not available on the host")
	}
	s.proj.mu.Lock()
	defer s.proj.mu.Unlock()
	p, err := s.loadProject(r)
	if err != nil {
		return err
	}
	kind, op := apitypes.JobKindProjectPull, opPull
	if s.needsClone(p) {
		kind, op = apitypes.JobKindProjectClone, opClone
	}
	job, err := s.startJob(r, kind, apitypes.SubjectTypeProject, p.ID, s.projectJob(p.ID, op, p.Branch))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, job)
	return nil
}

func (s *Server) handleSwitchBranch(w http.ResponseWriter, r *http.Request) error {
	if s.proj.git == nil {
		return newError(http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable, "git is not available on the host")
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req apitypes.SwitchBranchRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if err := git.ValidateBranch(req.Branch); err != nil {
		return invalidField("branch", err.Error())
	}
	s.proj.mu.Lock()
	defer s.proj.mu.Unlock()
	p, err := s.store.GetProject(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return notFound("project")
	}
	if err != nil {
		return internal(err)
	}
	kind, op := apitypes.JobKindProjectBranch, opBranch
	if s.needsClone(p) {
		// No checkout yet (e.g. the first clone failed because of a wrong
		// branch): clone the requested branch.
		kind, op = apitypes.JobKindProjectClone, opClone
	}
	job, err := s.startJob(r, kind, apitypes.SubjectTypeProject, p.ID, s.projectJob(p.ID, op, req.Branch))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, job)
	return nil
}

func (s *Server) handleListBranches(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r)
	if err != nil {
		return err
	}
	if s.proj.git == nil {
		return newError(http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable, "git is not available on the host")
	}
	items, err := s.proj.branches.get(r.Context(), p, s.now, func(ctx context.Context) ([]string, error) {
		remote, err := s.remoteFor(p)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, branchListTimeout)
		defer cancel()
		return s.proj.git.ListRemoteBranches(ctx, remote)
	})
	if err != nil {
		if r.Context().Err() != nil {
			return r.Context().Err()
		}
		s.logger.Info("list remote branches failed", "request_id", requestIDFrom(r.Context()), "project_id", p.ID, "err", err.Error())
		msg := git.SafeMessage(err)
		var ge *git.Error
		if !errors.As(err, &ge) {
			msg = "could not list the remote's branches"
		}
		return newError(http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable, msg)
	}
	writeJSON(w, http.StatusOK, apitypes.BranchList{Current: p.Branch, Items: items})
	return nil
}

// projectOp is what a project job does.
type projectOp int

const (
	opClone projectOp = iota
	opPull
	opBranch
)

// projectJob returns the job body for op on project id. branch is the
// branch to clone (empty: remote default) or switch to; pull uses the
// project's stored branch. The job owns the project's status while it runs:
// it sets the transient status, and on exit idle (success), the previous
// status (failure that left the checkout untouched) or error.
func (s *Server) projectJob(id int64, op projectOp, branch string) jobs.Func {
	return func(ctx context.Context, log io.Writer) error {
		p, err := s.store.GetProject(ctx, id)
		if err != nil {
			return jobs.Fail("the project no longer exists", err)
		}
		prev := p.Status
		if err := s.store.SetProjectStatus(ctx, id, string(transientStatus(op))); err != nil {
			return jobs.Fail("the project no longer exists", err)
		}
		fail := func(msg string, unchanged bool, cause error) error {
			status := string(apitypes.ProjectStatusError)
			if unchanged && op != opClone && prev != string(apitypes.ProjectStatusCloning) {
				status = prev
			}
			s.finalProjectStatus(id, status)
			return jobs.Fail(msg, cause)
		}

		remote, err := s.remoteFor(p)
		if err != nil {
			return fail("the stored credentials could not be decrypted; set them again", true, err)
		}
		dir := git.CheckoutDir(s.proj.repoDir, id)
		var co git.Checkout
		switch op {
		case opClone:
			remote.Branch = branch
			fmt.Fprintf(log, "Cloning %s (%s) for project %s\n", p.RepoURL, branchLabel(branch), p.Name)
			// Whatever is at the checkout path is a leftover (failed
			// earlier clone, inconsistent state or an orphan of a deleted
			// project with the same id).
			if err := git.RemoveCheckout(s.proj.repoDir, id, s.proj.tmpDir); err != nil && !errors.Is(err, git.ErrRemoveIncomplete) {
				return fail("could not remove the previous checkout", false, err)
			}
			co, err = s.proj.git.Clone(ctx, remote, dir, log)
		case opPull:
			remote.Branch = p.Branch
			fmt.Fprintf(log, "Pulling %s (branch %s) for project %s\n", p.RepoURL, p.Branch, p.Name)
			co, err = s.proj.git.Pull(ctx, remote, dir, log)
		case opBranch:
			remote.Branch = branch
			fmt.Fprintf(log, "Switching project %s from branch %s to %s\n", p.Name, p.Branch, branch)
			co, err = s.proj.git.SwitchBranch(ctx, remote, dir, log)
		}
		if err != nil {
			return fail(git.SafeMessage(err), git.IsUnchanged(err), err)
		}
		now := s.now().UTC()
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalUpdateTimeout)
		defer cancel()
		if _, err := s.store.SetProjectCheckout(uctx, id, store.ProjectCheckout{
			Branch: co.Branch, LastCommit: co.Commit, LastPulledAt: &now, Status: string(apitypes.ProjectStatusIdle),
		}); err != nil {
			return jobs.Fail("the checkout succeeded but the project could not be updated", err)
		}
		s.proj.branches.forget(id)
		return nil
	}
}

func transientStatus(op projectOp) apitypes.ProjectStatus {
	switch op {
	case opPull:
		return apitypes.ProjectStatusPulling
	case opBranch:
		return apitypes.ProjectStatusSwitching
	}
	return apitypes.ProjectStatusCloning
}

func branchLabel(b string) string {
	if b == "" {
		return "default branch"
	}
	return "branch " + b
}

// finalProjectStatus records a job's final status even when the job's
// context was cancelled.
func (s *Server) finalProjectStatus(id int64, status string) {
	ctx, cancel := context.WithTimeout(context.Background(), finalUpdateTimeout)
	defer cancel()
	if err := s.store.SetProjectStatus(ctx, id, status); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("set project status failed", "project_id", id, "status", status, "err", err)
	}
}

// branchCache caches remote branch listings per project, shares one
// in-flight listing between concurrent requests and bounds how many
// listings run at once.
type branchCache struct {
	mu      sync.Mutex
	entries map[int64]*branchEntry
	sem     chan struct{}
}

type branchEntry struct {
	key   string // remote URL and update time of the project
	ready chan struct{}
	at    time.Time
	items []string
	err   error
}

func (c *branchCache) forget(id int64) {
	c.mu.Lock()
	delete(c.entries, id)
	c.mu.Unlock()
}

func (c *branchCache) get(ctx context.Context, p store.Project, now func() time.Time, fetch func(context.Context) ([]string, error)) ([]string, error) {
	key := p.RepoURL + "\x00" + p.UpdatedAt.UTC().Format(time.RFC3339Nano)
	c.mu.Lock()
	e := c.entries[p.ID]
	if e != nil && e.key == key {
		select {
		case <-e.ready:
			ttl := branchCacheTTL
			if e.err != nil {
				ttl = branchErrorTTL
			}
			if now().Sub(e.at) < ttl {
				c.mu.Unlock()
				return e.items, e.err
			}
		default:
			// In flight: wait for it.
			c.mu.Unlock()
			select {
			case <-e.ready:
				return e.items, e.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	e = &branchEntry{key: key, ready: make(chan struct{})}
	c.entries[p.ID] = e
	c.mu.Unlock()

	select {
	case c.sem <- struct{}{}:
		e.items, e.err = fetch(ctx)
		<-c.sem
	case <-ctx.Done():
		e.err = ctx.Err()
	}
	e.at = now()
	close(e.ready)
	if ctx.Err() != nil {
		// This request went away; do not cache its cancellation.
		c.mu.Lock()
		if c.entries[p.ID] == e {
			delete(c.entries, p.ID)
		}
		c.mu.Unlock()
	}
	return e.items, e.err
}
