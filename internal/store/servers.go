package store

import (
	"context"
	"database/sql"
	"time"
)

// Server is a stored server definition. Env is opaque ciphertext produced by
// internal/secrets with purpose secrets.ServerEnvPurpose(ID) over a JSON
// object; the store never interprets it. Port is nil only while unassigned.
type Server struct {
	ID           int64
	Name         string
	ProjectID    *int64
	Image        string
	Port         *int
	Env          []byte `json:"-"`
	DesiredState string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const serverCols = `id, name, project_id, image, port, env, desired_state, created_at, updated_at`

func scanServer(r scanner) (Server, error) {
	var sv Server
	var projectID, port sql.NullInt64
	var created, updated string
	if err := r.Scan(&sv.ID, &sv.Name, &projectID, &sv.Image, &port, &sv.Env, &sv.DesiredState, &created, &updated); err != nil {
		return Server{}, err
	}
	sv.ProjectID = int64Ptr(projectID)
	if port.Valid {
		p := int(port.Int64)
		sv.Port = &p
	}
	var err error
	if sv.CreatedAt, err = parseTime(created); err != nil {
		return Server{}, err
	}
	if sv.UpdatedAt, err = parseTime(updated); err != nil {
		return Server{}, err
	}
	return sv, nil
}

func intPtrArg(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// CreateServer inserts sv (ID and timestamps ignored; empty DesiredState
// becomes "stopped"). When seal is non-nil it is called with the new id in
// the same transaction and its result stored as env. ErrConflict means the
// name or port is taken.
func (s *Store) CreateServer(ctx context.Context, sv Server, seal SealFunc) (Server, error) {
	if sv.DesiredState == "" {
		sv.DesiredState = "stopped"
	}
	var out Server
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := formatTime(s.nowUTC())
		var err error
		out, err = scanServer(tx.QueryRowContext(ctx,
			`INSERT INTO servers (name, project_id, image, port, env, desired_state, created_at, updated_at)
			 VALUES (?, ?, ?, ?, NULL, ?, ?, ?) RETURNING `+serverCols,
			sv.Name, nullInt64(sv.ProjectID), sv.Image, intPtrArg(sv.Port), sv.DesiredState, now, now))
		if err != nil {
			return mapErr("create server", err)
		}
		if seal == nil {
			return nil
		}
		ct, err := seal(out.ID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE servers SET env = ? WHERE id = ?`, blobArg(ct), out.ID); err != nil {
			return mapErr("create server", err)
		}
		out.Env = ct
		return nil
	})
	return out, err
}

// CreateServers inserts every server of list in one transaction: either all
// are created or none (ErrConflict when a name or port is taken). seal, when
// non-nil, is called with each server's index and new id inside the
// transaction and its result stored as that server's env.
func (s *Store) CreateServers(ctx context.Context, list []Server, seal func(i int, id int64) ([]byte, error)) ([]Server, error) {
	out := make([]Server, 0, len(list))
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := formatTime(s.nowUTC())
		for i, sv := range list {
			if sv.DesiredState == "" {
				sv.DesiredState = "stopped"
			}
			created, err := scanServer(tx.QueryRowContext(ctx,
				`INSERT INTO servers (name, project_id, image, port, env, desired_state, created_at, updated_at)
				 VALUES (?, ?, ?, ?, NULL, ?, ?, ?) RETURNING `+serverCols,
				sv.Name, nullInt64(sv.ProjectID), sv.Image, intPtrArg(sv.Port), sv.DesiredState, now, now))
			if err != nil {
				return mapErr("create servers", err)
			}
			if seal != nil {
				ct, err := seal(i, created.ID)
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE servers SET env = ? WHERE id = ?`, blobArg(ct), created.ID); err != nil {
					return mapErr("create servers", err)
				}
				created.Env = ct
			}
			out = append(out, created)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetServer returns server id, or ErrNotFound.
func (s *Store) GetServer(ctx context.Context, id int64) (Server, error) {
	sv, err := scanServer(s.db.QueryRowContext(ctx, `SELECT `+serverCols+` FROM servers WHERE id = ?`, id))
	if err != nil {
		return Server{}, mapErr("get server", err)
	}
	return sv, nil
}

// ListServers returns all servers ordered by name.
func (s *Store) ListServers(ctx context.Context) ([]Server, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+serverCols+` FROM servers ORDER BY name`)
	if err != nil {
		return nil, mapErr("list servers", err)
	}
	defer rows.Close()
	out := []Server{}
	for rows.Next() {
		sv, err := scanServer(rows)
		if err != nil {
			return nil, mapErr("list servers", err)
		}
		out = append(out, sv)
	}
	return out, mapErr("list servers", rows.Err())
}

// CountServers returns the number of servers.
func (s *Store) CountServers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM servers`).Scan(&n)
	return n, mapErr("count servers", err)
}

// UpdateServer overwrites every mutable column of server sv.ID (Name,
// ProjectID and CreatedAt are not changed; UpdatedAt is set to now) and
// returns the stored row. Env must already be sealed for sv.ID.
func (s *Store) UpdateServer(ctx context.Context, sv Server) (Server, error) {
	out, err := scanServer(s.db.QueryRowContext(ctx,
		`UPDATE servers SET image = ?, port = ?, env = ?, desired_state = ?, updated_at = ?
		 WHERE id = ? RETURNING `+serverCols,
		sv.Image, intPtrArg(sv.Port), blobArg(sv.Env), sv.DesiredState, formatTime(s.nowUTC()), sv.ID))
	if err != nil {
		return Server{}, mapErr("update server", err)
	}
	return out, nil
}

// SetServerDesiredState sets only desired_state (and updated_at) of server
// id, so it never overwrites a concurrent change of other columns.
func (s *Store) SetServerDesiredState(ctx context.Context, id int64, state string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE servers SET desired_state = ?, updated_at = ? WHERE id = ? AND desired_state <> ?`,
		state, formatTime(s.nowUTC()), id, state)
	if err != nil {
		return mapErr("set server desired state", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.GetServer(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// DeleteServer deletes server id.
func (s *Store) DeleteServer(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM servers WHERE id = ?`, id)
	if err != nil {
		return mapErr("delete server", err)
	}
	return checkAffected("delete server", res)
}
