package servers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// ExportFormatVersion is the ServerExport format this daemon writes and
// accepts.
const ExportFormatVersion = 1

// MaxImportServers bounds one import document. With MaxEnvBytes of env per
// server the largest possible document stays below MaxImportBytes even
// when every env byte needs a six-byte JSON escape.
const MaxImportServers = 128

// MaxImportBytes is the request body limit of the import route.
const MaxImportBytes = 64 << 20

// Export returns every server definition including env values in plain
// text (admin only).
func (s *Service) Export(ctx context.Context) (apitypes.ServerExport, error) {
	rows, err := s.store.ListServers(ctx)
	if err != nil {
		return apitypes.ServerExport{}, err
	}
	projects := map[int64]string{}
	out := apitypes.ServerExport{FormatVersion: ExportFormatVersion, Servers: make([]apitypes.ServerSpec, 0, len(rows))}
	for _, sv := range rows {
		env, err := s.openEnv(sv)
		if err != nil {
			return apitypes.ServerExport{}, err
		}
		spec := apitypes.ServerSpec{Name: sv.Name, Image: sv.Image, Port: sv.Port, DesiredState: apitypes.DesiredState(sv.DesiredState)}
		if len(env) > 0 {
			spec.Env = env
		}
		if sv.ProjectID != nil {
			name, ok := projects[*sv.ProjectID]
			if !ok {
				p, err := s.store.GetProject(ctx, *sv.ProjectID)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return apitypes.ServerExport{}, err
				}
				name = p.Name
				projects[*sv.ProjectID] = name
			}
			spec.Project = name
		}
		out.Servers = append(out.Servers, spec)
	}
	return out, nil
}

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
	// Resolve and verify every image before taking the write lock (each is
	// a docker call).
	images := make([]string, len(doc.Servers))
	for i, sp := range doc.Servers {
		var err error
		if images[i], err = s.resolveImage(ctx, fmt.Sprintf("servers[%d].image", i), sp.Image); err != nil {
			return nil, err
		}
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
		row := store.Server{Name: sp.Name, DesiredState: "stopped"}
		if sp.Project != "" {
			pid, ok := projectIDs[sp.Project]
			if !ok {
				p, err := s.store.GetProjectByName(ctx, strings.ToLower(sp.Project))
				if errors.Is(err, store.ErrNotFound) {
					return nil, fieldErr(f("project"), "project %q does not exist", sp.Project)
				}
				if err != nil {
					return nil, err
				}
				pid = p.ID
				projectIDs[sp.Project] = pid
			}
			row.ProjectID = &pid
		}
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
		row.Image = images[i]
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
	if err != nil {
		return nil, err
	}
	out := make([]apitypes.Server, 0, len(created))
	for _, sv := range created {
		v, err := s.view(sv, nil, true)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	s.logger.Info("servers imported", "count", len(out))
	return out, nil
}
