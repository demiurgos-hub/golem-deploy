package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Server is a stored server definition. Env is opaque ciphertext produced by
// internal/secrets with purpose secrets.ServerEnvPurpose(ID) over a JSON
// object; the store never interprets it. Port is nil only while unassigned.
// ContainerID is the container the daemon created for the server ("" when
// none); it is the only way the daemon finds the server's container.
type Server struct {
	ID           int64
	Name         string
	ProjectID    *int64
	Image        string
	Port         *int
	Env          []byte `json:"-"`
	DesiredState string
	ContainerID  string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ErrStale is returned by UpdateServer when the stored env changed since the
// caller read it (a concurrent update won); the caller re-reads and retries.
var ErrStale = errors.New("store: server changed concurrently")

const serverCols = `id, name, project_id, image, port, env, desired_state, container_id, created_at, updated_at`

func scanServer(r scanner) (Server, error) {
	var sv Server
	var projectID, port sql.NullInt64
	var containerID sql.NullString
	var created, updated string
	if err := r.Scan(&sv.ID, &sv.Name, &projectID, &sv.Image, &port, &sv.Env, &sv.DesiredState, &containerID, &created, &updated); err != nil {
		return Server{}, err
	}
	sv.ProjectID = int64Ptr(projectID)
	if port.Valid {
		p := int(port.Int64)
		sv.Port = &p
	}
	sv.ContainerID = containerID.String
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

// CreateServer inserts sv (ID, ContainerID and timestamps ignored; empty
// DesiredState becomes "stopped"). When seal is non-nil it is called with
// the new id in the same transaction and its result stored as env.
// ErrConflict means the name or port is taken.
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

// ServerUpdate lists the columns UpdateServer changes; nil/false fields are
// left alone. Image and Port are written as given (they do not depend on
// the previous row). Env is a read-modify-write of the sealed variables, so
// it is compare-and-swapped: it is written only while the stored ciphertext
// still equals PrevEnv, otherwise UpdateServer fails with ErrStale.
type ServerUpdate struct {
	Image   *string
	Port    *int
	SetEnv  bool
	Env     []byte // sealed for the server's id
	PrevEnv []byte // the ciphertext the new Env was derived from
}

// UpdateServer applies u to server id (UpdatedAt is set to now) and returns
// the stored row. It never writes desired_state (SetServerDesiredState) or
// container_id (SetServerContainer), which jobs change concurrently.
// ErrConflict means the port is taken, ErrStale that the env changed
// meanwhile.
func (s *Store) UpdateServer(ctx context.Context, id int64, u ServerUpdate) (Server, error) {
	sets := []string{"updated_at = ?"}
	args := []any{formatTime(s.nowUTC())}
	if u.Image != nil {
		sets = append(sets, "image = ?")
		args = append(args, *u.Image)
	}
	if u.Port != nil {
		sets = append(sets, "port = ?")
		args = append(args, *u.Port)
	}
	where := "id = ?"
	whereArgs := []any{id}
	if u.SetEnv {
		sets = append(sets, "env = ?")
		args = append(args, blobArg(u.Env))
		where += " AND env IS ?"
		whereArgs = append(whereArgs, blobArg(u.PrevEnv))
	}
	out, err := scanServer(s.db.QueryRowContext(ctx,
		`UPDATE servers SET `+strings.Join(sets, ", ")+` WHERE `+where+` RETURNING `+serverCols,
		append(args, whereArgs...)...))
	if err == nil {
		return out, nil
	}
	err = mapErr("update server", err)
	if errors.Is(err, ErrNotFound) && u.SetEnv {
		if _, gerr := s.GetServer(ctx, id); gerr == nil {
			return Server{}, ErrStale
		}
	}
	return Server{}, err
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

// SetServerContainer records the container the daemon created for server
// id. Only the server's job (serialised per server) calls it.
func (s *Store) SetServerContainer(ctx context.Context, id int64, containerID string) error {
	if containerID == "" {
		return errors.New("set server container: empty container id")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE servers SET container_id = ? WHERE id = ?`, containerID, id)
	if err != nil {
		return mapErr("set server container", err)
	}
	return checkAffected("set server container", res)
}

// ClearServerContainer forgets the recorded container of server id if it is
// still containerID (so a stale caller cannot clear a newer record).
func (s *Store) ClearServerContainer(ctx context.Context, id int64, containerID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE servers SET container_id = NULL WHERE id = ? AND container_id = ?`, id, containerID)
	return mapErr("clear server container", err)
}

// DeleteServer deletes server id.
func (s *Store) DeleteServer(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM servers WHERE id = ?`, id)
	if err != nil {
		return mapErr("delete server", err)
	}
	return checkAffected("delete server", res)
}

// SettingInstanceID is the settings key of the installation's instance id.
// It is internal: never returned by the API.
const SettingInstanceID = "instance_id"

var instanceIDRE = regexp.MustCompile(`^[a-f0-9]{32}$`)

// InstanceID returns this installation's random instance id (32 lower-case
// hex characters), generating and storing it on first use. Docker objects
// the daemon creates are labelled with it so that two installations sharing
// a Docker host, or objects left by a wiped data directory, never collide.
func (s *Store) InstanceID(ctx context.Context) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`,
		SettingInstanceID, hex.EncodeToString(b)); err != nil {
		return "", mapErr("create instance id", err)
	}
	v, ok, err := s.GetSetting(ctx, SettingInstanceID)
	if err != nil {
		return "", err
	}
	if !ok || !instanceIDRE.MatchString(v) {
		return "", fmt.Errorf("instance id in settings is malformed")
	}
	return v, nil
}
