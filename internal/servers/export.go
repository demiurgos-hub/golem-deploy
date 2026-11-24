package servers

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// ExportFormatVersion is the ServerExport format this daemon writes and
// accepts (2: servers reference a build by project and commit).
const ExportFormatVersion = 2

// MaxImportServers bounds one import document. With MaxEnvBytes of env per
// server the largest possible document stays below MaxImportBytes even
// when every env byte needs a six-byte JSON escape.
const MaxImportServers = 128

// MaxImportBytes is the request body limit of the import route.
const MaxImportBytes = 64 << 20

// Export returns every server definition including env values in plain
// text (admin only). A server's build is written as its project's name and
// commit; a server without a build is exported with an empty project (it
// cannot be imported until one is chosen).
func (s *Service) Export(ctx context.Context) (apitypes.ServerExport, error) {
	rows, err := s.store.ListServers(ctx)
	if err != nil {
		return apitypes.ServerExport{}, err
	}
	projects := map[int64]string{}
	bc := s.builds()
	out := apitypes.ServerExport{FormatVersion: ExportFormatVersion, Servers: make([]apitypes.ServerSpec, 0, len(rows))}
	for _, sv := range rows {
		env, err := s.openEnv(sv)
		if err != nil {
			return apitypes.ServerExport{}, err
		}
		spec := apitypes.ServerSpec{Name: sv.Name, Port: sv.Port, DesiredState: apitypes.DesiredState(sv.DesiredState)}
		if len(env) > 0 {
			spec.Env = env
		}
		if len(sv.AllowedOrigins) > 0 {
			spec.AllowedOrigins = append([]string{}, sv.AllowedOrigins...)
		}
		b, err := bc.get(ctx, sv)
		if err != nil {
			return apitypes.ServerExport{}, err
		}
		if b != nil {
			name, ok := projects[b.ProjectID]
			if !ok {
				p, err := s.store.GetProject(ctx, b.ProjectID)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return apitypes.ServerExport{}, err
				}
				name = p.Name
				projects[b.ProjectID] = name
			}
			spec.Project, spec.Commit = name, b.Commit
		}
		out.Servers = append(out.Servers, spec)
	}
	return out, nil
}

// commitRE matches a full commit hash (SHA-1 or SHA-256 repositories).
var commitRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// Import validates the whole document and then creates every server in one
// transaction (all or nothing). Imported servers are always created
// stopped; nothing is started. Field names in errors are
// "servers[i].<field>".
func (s *Service) Import(ctx context.Context, doc apitypes.ServerExport) ([]apitypes.Server, error) {
	if doc.FormatVersion != ExportFormatVersion {
		return nil, fieldErr("format_version", "format_version must be %d", ExportFormatVersion)
	}
	if len(doc.Servers) == 0 {
		return nil, fieldErr("servers", "the document contains no servers")
	}
	if len(doc.Servers) > MaxImportServers {
		return nil, fieldErr("servers", "at most %d servers can be imported at once", MaxImportServers)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	existing, err := s.store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	takenNames := map[string]bool{}
	used := map[int]bool{}
	for _, r := range existing {
		takenNames[r.Name] = true
		if r.Port != nil {
			used[*r.Port] = true
		}
	}
	docNames := map[string]bool{}
	docPorts := map[int]bool{}
	rows := make([]store.Server, len(doc.Servers))
	envs := make([]map[string]string, len(doc.Servers))
	projectIDs := map[string]int64{}
	type buildKey struct {
		project int64
		commit  string
	}
	buildIDs := map[buildKey]int64{}

	// Pass 1: everything except auto-assigned ports.
	for i, sp := range doc.Servers {
		f := func(name string) string { return fmt.Sprintf("servers[%d].%s", i, name) }
		if err := docker.ValidateServerName(sp.Name); err != nil {
			return nil, fieldErr(f("name"), "name must be 1-63 characters of a-z, 0-9 and '-', starting with a letter or digit")
		}
		if docNames[sp.Name] {
			return nil, fieldErr(f("name"), "duplicate server name %q in the document", sp.Name)
		}
		docNames[sp.Name] = true
		if takenNames[sp.Name] {
			return nil, fmt.Errorf("%w: %s", ErrNameTaken, sp.Name)
		}
		switch sp.DesiredState {
		case "", apitypes.DesiredStateRunning, apitypes.DesiredStateStopped:
		default:
			return nil, fieldErr(f("desired_state"), `desired_state must be "running" or "stopped"`)
		}
		if err := validateEnv(f("env"), sp.Env); err != nil {
			return nil, err
		}
		origins, err := validateOrigins(f("allowed_origins"), sp.AllowedOrigins)
		if err != nil {
			return nil, err
		}
		row := store.Server{Name: sp.Name, DesiredState: "stopped", AllowedOrigins: origins}
		if sp.Project == "" {
			return nil, fieldErr(f("project"), "project is required (the server runs a build of that project)")
		}
		if sp.Commit != "" && !commitRE.MatchString(sp.Commit) {
			return nil, fieldErr(f("commit"), "commit must be a full lower-case commit hash")
		}
		pid, ok := projectIDs[sp.Project]
		if !ok {
			p, err := s.store.GetProjectByName(ctx, strings.ToLower(sp.Project))
			if errors.Is(err, store.ErrNotFound) {
				return nil, fieldErr(f("project"), "project %q does not exist", clipName(sp.Project))
			}
			if err != nil {
				return nil, err
			}
			pid = p.ID
			projectIDs[sp.Project] = pid
		}
		key := buildKey{pid, sp.Commit}
		bid, ok := buildIDs[key]
		if !ok {
			b, err := s.store.LatestSuccessfulBuild(ctx, pid, sp.Commit)
			switch {
			case errors.Is(err, store.ErrNotFound) && sp.Commit == "":
				return nil, fieldErr(f("project"), "project %q has no successful build", clipName(sp.Project))
			case errors.Is(err, store.ErrNotFound):
				return nil, fieldErr(f("commit"), "project %q has no successful build of commit %s", clipName(sp.Project), sp.Commit)
			case err != nil:
				return nil, err
			}
			bid = b.ID
			buildIDs[key] = bid
		}
		row.BuildID = &bid
		if sp.Port != nil {
			p := *sp.Port
			if err := s.checkPort(f("port"), p); err != nil {
				return nil, err
			}
			if docPorts[p] {
				return nil, fieldErr(f("port"), "duplicate port %d in the document", p)
			}
			if used[p] {
				return nil, fmt.Errorf("%w: %d", ErrPortInUse, p)
			}
			docPorts[p] = true
			row.Port = &p
		}
		rows[i] = row
		envs[i] = sp.Env
	}
	// Pass 2: auto-assign the remaining ports.
	for p := range docPorts {
		used[p] = true
	}
	for i := range rows {
		if rows[i].Port != nil {
			continue
		}
		if s.cfg.PanelPort != 0 {
			used[s.cfg.PanelPort] = true
		}
		p, err := s.cfg.Ports.Lowest(used)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrPortInUse, err)
		}
		used[p] = true
		rows[i].Port = &p
	}

	created, err := s.store.CreateServers(ctx, rows, func(i int, id int64) ([]byte, error) { return s.sealEnv(id, envs[i]) })
	if errors.Is(err, store.ErrConflict) {
		return nil, ErrConflict
	}
	if errors.Is(err, store.ErrMissingReference) {
		return nil, errBuildGone
	}
	if err != nil {
		return nil, err
	}
	out := make([]apitypes.Server, 0, len(created))
	for _, sv := range created {
		v, err := s.viewOne(ctx, sv, nil, true)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	s.logger.Info("servers imported", "count", len(out))
	return out, nil
}

// clipName bounds a name from an import document echoed in a message.
func clipName(s string) string {
	out := []rune{}
	for _, r := range s {
		if len(out) == 64 {
			return string(out) + "..."
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || (r >= 0x200b && r <= 0x206f) {
			r = '?'
		}
		out = append(out, r)
	}
	return string(out)
}
