package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Server is a stored server definition. Env is opaque ciphertext produced by
// internal/secrets with purpose secrets.ServerEnvPurpose(ID) over a JSON
// object; the store never interprets it. Port is nil only while unassigned.
// BuildID is the build whose image the server runs (nil for rows created
// before servers referenced builds; they cannot start). ContainerID is the
// container the daemon created for the server ("" when none); it is the only
// way the daemon finds the server's container. AdminPort and AdminToken
// (sealed with secrets.ServerAdminTokenPurpose(ID)) belong to that container
// and are recorded and cleared with it.
type Server struct {
	ID             int64
	Name           string
	BuildID        *int64
	Port           *int
	Env            []byte `json:"-"`
	DesiredState   string
	ContainerID    string
	AllowedOrigins []string
	AdminPort      int
	AdminToken     []byte `json:"-"`
	PendingLaunch  string `json:"-"`
	StartComplete  bool
	// DataVolume is the Docker volume holding the server's data directory
	// ("" until the first start and after a reset); only server jobs write
	// it. DataFingerprint is the schema fingerprint of that saved data as
	// far as the daemon knows ("" when unknown).
	DataVolume      string
	DataFingerprint string
	// SnapshotIntervalSeconds is the game's autosave period (0: off).
	// BackupIntervalHours (0: off) and BackupKeep are the backup schedule.
	SnapshotIntervalSeconds int
	BackupIntervalHours     int
	BackupKeep              int
	// BackupFailedAt is when the server's last scheduled backup failed
	// (nil after a success or when none failed).
	BackupFailedAt *time.Time
	// Inspector is the server's Inspector switch (off by default).
	Inspector bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Defaults of the saved data settings of a new server.
const (
	DefaultSnapshotIntervalSeconds = 300
	DefaultBackupKeep              = 7
)

// ErrStale is returned by UpdateServer when the stored env changed since the
// caller read it (a concurrent update won); the caller re-reads and retries.
var ErrStale = errors.New("store: server changed concurrently")

const serverCols = `id, name, build_id, port, env, desired_state, container_id, allowed_origins, admin_port, admin_token, pending_launch, start_complete, data_volume, data_fingerprint, snapshot_interval_seconds, backup_interval_hours, backup_keep, backup_failed_at, inspector, created_at, updated_at`

func scanServer(r scanner) (Server, error) {
	var sv Server
	var buildID, port, adminPort sql.NullInt64
	var containerID sql.NullString
	var origins, created, updated string
	var backupFailed sql.NullString
	if err := r.Scan(&sv.ID, &sv.Name, &buildID, &port, &sv.Env, &sv.DesiredState, &containerID, &origins, &adminPort, &sv.AdminToken, &sv.PendingLaunch, &sv.StartComplete,
		&sv.DataVolume, &sv.DataFingerprint, &sv.SnapshotIntervalSeconds, &sv.BackupIntervalHours, &sv.BackupKeep, &backupFailed, &sv.Inspector, &created, &updated); err != nil {
		return Server{}, err
	}
	sv.BuildID = int64Ptr(buildID)
	if port.Valid {
		p := int(port.Int64)
		sv.Port = &p
	}
	sv.ContainerID = containerID.String
	sv.AdminPort = int(adminPort.Int64)
	if err := json.Unmarshal([]byte(origins), &sv.AllowedOrigins); err != nil {
		return Server{}, fmt.Errorf("decode allowed origins of server %d: %w", sv.ID, err)
	}
	if sv.AllowedOrigins == nil {
		sv.AllowedOrigins = []string{}
	}
	var err error
	if sv.CreatedAt, err = parseTime(created); err != nil {
		return Server{}, err
	}
	if sv.UpdatedAt, err = parseTime(updated); err != nil {
		return Server{}, err
	}
	if backupFailed.Valid {
		t, err := parseTime(backupFailed.String)
		if err != nil {
			return Server{}, err
		}
		sv.BackupFailedAt = &t
	}
	return sv, nil
}

func intPtrArg(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func originsArg(origins []string) (string, error) {
	if origins == nil {
		origins = []string{}
	}
	b, err := json.Marshal(origins)
	return string(b), err
}

func insertServer(ctx context.Context, tx *sql.Tx, sv Server, now string) (Server, error) {
	origins, err := originsArg(sv.AllowedOrigins)
	if err != nil {
		return Server{}, err
	}
	if sv.BackupKeep == 0 {
		sv.BackupKeep = DefaultBackupKeep
	}
	return scanServer(tx.QueryRowContext(ctx,
		`INSERT INTO servers (name, build_id, port, env, desired_state, allowed_origins, snapshot_interval_seconds, backup_interval_hours, backup_keep, inspector, created_at, updated_at)
		 VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING `+serverCols,
		sv.Name, nullInt64(sv.BuildID), intPtrArg(sv.Port), sv.DesiredState, origins, sv.SnapshotIntervalSeconds, sv.BackupIntervalHours, sv.BackupKeep, sv.Inspector, now, now))
}

// CreateServer inserts sv (ID, ContainerID, admin and data fields and
// timestamps ignored; empty DesiredState becomes "stopped", BackupKeep 0
// becomes DefaultBackupKeep; SnapshotIntervalSeconds is stored as given, so
// callers pass DefaultSnapshotIntervalSeconds for the default). When seal is non-nil it is
// called with the new id in the same transaction and its result stored as
// env. ErrConflict means the name or port is taken.
func (s *Store) CreateServer(ctx context.Context, sv Server, seal SealFunc) (Server, error) {
	if sv.DesiredState == "" {
		sv.DesiredState = "stopped"
	}
	var out Server
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = insertServer(ctx, tx, sv, formatTime(s.nowUTC()))
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
			created, err := insertServer(ctx, tx, sv, now)
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

// ListServerIDs returns the server ids for certificate maintenance.
func (s *Store) ListServerIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM servers ORDER BY id")
	if err != nil {
		return nil, mapErr("list server ids", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, mapErr("list server ids", err)
		}
		ids = append(ids, id)
	}
	return ids, mapErr("list server ids", rows.Err())
}

// CountServers returns the number of servers.
func (s *Store) CountServers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM servers`).Scan(&n)
	return n, mapErr("count servers", err)
}

// ServerUpdate lists the columns UpdateServer changes; nil/false fields
// are left alone. A port change requires the inspected PrevContainerID,
// and an env change requires the ciphertext it was derived from.
type ServerUpdate struct {
	BuildID         *int64
	Port            *int
	SetOrigins      bool
	AllowedOrigins  []string
	SetEnv          bool
	Env             []byte // sealed for the server's id
	PrevContainerID string // container checked for a port change
	PrevEnv         []byte // the ciphertext the new Env was derived from
	// SnapshotIntervalSeconds, BackupIntervalHours and BackupKeep change
	// the saved data settings.
	SnapshotIntervalSeconds *int
	BackupIntervalHours     *int
	BackupKeep              *int
	// Inspector changes the server's Inspector switch.
	Inspector *bool
}

// UpdateServer applies u to server id (UpdatedAt is set to now) and returns
// the stored row. It never writes desired_state (SetServerDesiredState) or
// the container columns (SetServerContainer), which jobs change
// concurrently. ErrConflict means the port is taken. ErrStale means the
// container or the env changed meanwhile.
func (s *Store) UpdateServer(ctx context.Context, id int64, u ServerUpdate) (Server, error) {
	var out Server
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var current Server
		if u.Port != nil || u.BuildID != nil {
			var err error
			current, err = scanServer(tx.QueryRowContext(ctx, "SELECT "+serverCols+" FROM servers WHERE id = ?", id))
			if err != nil {
				return mapErr("update server", err)
			}
			if err := checkServerJob(ctx, tx, id); err != nil {
				return err
			}
		}
		if u.Port != nil && current.ContainerID != u.PrevContainerID {
			return ErrStale
		}
		if u.BuildID != nil {
			var projectID int64
			err := tx.QueryRowContext(ctx, "SELECT project_id FROM builds WHERE id = ? AND status = 'success' AND image_id <> ''", *u.BuildID).Scan(&projectID)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrMissingReference
			}
			if err != nil {
				return mapErr("read new server build", err)
			}
			if current.BuildID != nil {
				var currentProjectID int64
				if err := tx.QueryRowContext(ctx, "SELECT project_id FROM builds WHERE id = ?", *current.BuildID).Scan(&currentProjectID); err != nil {
					return mapErr("read current server build", err)
				}
				if currentProjectID != projectID {
					return ErrServerProject
				}
			} else if len(current.Env) != 0 && (!u.SetEnv || len(u.Env) != 0) {
				// Migration 0006 cleared the pre-build project link. An
				// operator can assign the first build after clearing the
				// old environment, whose project can no longer be proved.
				return ErrLegacyServerEnv
			}
		}
		var err error
		out, err = s.updateServer(ctx, tx, id, u)
		return err
	})
	return out, err
}

// These errors protect a server's project, legacy environment and active jobs.
var (
	ErrServerProject   = errors.New("store: server project cannot change")
	ErrLegacyServerEnv = errors.New("store: legacy server environment has no associated project")
	ErrServerJob       = errors.New("store: server job in progress")
)

// CheckServerJob reports an active job early, before inspecting Docker for
// a port change. UpdateServer also checks within its write transaction.
func (s *Store) CheckServerJob(ctx context.Context, id int64) error {
	return checkServerJob(ctx, s.db, id)
}

func checkServerJob(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) error {
	var active bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM jobs WHERE subject_type = 'server' AND subject_id = ? AND status IN ('queued', 'running'))", strconv.FormatInt(id, 10)).Scan(&active)
	if err != nil {
		return mapErr("check server job", err)
	}
	if active {
		return ErrServerJob
	}
	return nil
}

func (s *Store) updateServer(ctx context.Context, tx *sql.Tx, id int64, u ServerUpdate) (Server, error) {
	sets := []string{"updated_at = ?"}
	args := []any{formatTime(s.nowUTC())}
	if u.BuildID != nil {
		sets = append(sets, "build_id = ?")
		args = append(args, *u.BuildID)
	}
	if u.Port != nil {
		sets = append(sets, "port = ?")
		args = append(args, *u.Port)
	}
	for _, c := range []struct {
		col string
		v   *int
	}{{"snapshot_interval_seconds", u.SnapshotIntervalSeconds}, {"backup_interval_hours", u.BackupIntervalHours}, {"backup_keep", u.BackupKeep}} {
		if c.v != nil {
			sets = append(sets, c.col+" = ?")
			args = append(args, *c.v)
		}
	}
	if u.Inspector != nil {
		sets = append(sets, "inspector = ?")
		args = append(args, *u.Inspector)
	}
	if u.SetOrigins {
		origins, err := originsArg(u.AllowedOrigins)
		if err != nil {
			return Server{}, err
		}
		sets = append(sets, "allowed_origins = ?")
		args = append(args, origins)
	}
	where := "id = ?"
	whereArgs := []any{id}
	if u.SetEnv {
		sets = append(sets, "env = ?")
		args = append(args, blobArg(u.Env))
		where += " AND env IS ?"
		whereArgs = append(whereArgs, blobArg(u.PrevEnv))
	}
	out, err := scanServer(tx.QueryRowContext(ctx,
		`UPDATE servers SET `+strings.Join(sets, ", ")+` WHERE `+where+` RETURNING `+serverCols,
		append(args, whereArgs...)...))
	if err == nil {
		return out, nil
	}
	err = mapErr("update server", err)
	if errors.Is(err, ErrNotFound) && u.SetEnv {
		if _, gerr := scanServer(tx.QueryRowContext(ctx, "SELECT "+serverCols+" FROM servers WHERE id = ?", id)); gerr == nil {
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
// id together with its admin endpoint: the loopback host port (0 when not
// known yet) and the sealed per-container admin token. Only the server's job
// (serialised per server) calls it.
func (s *Store) SetServerContainer(ctx context.Context, id int64, containerID string, adminPort int, adminToken []byte) error {
	if containerID == "" {
		return errors.New("set server container: empty container id")
	}
	err := s.durableWrite(ctx, `UPDATE servers SET container_id = ?, admin_port = ?, admin_token = ?, pending_launch = '', start_complete = 0 WHERE id = ?`,
		containerID, nullPort(adminPort), blobArg(adminToken), id)
	return mapErr("set server container", err)
}

// SetServerAdminPort updates the recorded admin host port of server id while
// its recorded container is still containerID (Docker can assign another
// port when the container is restarted).
func (s *Store) SetServerAdminPort(ctx context.Context, id int64, containerID string, adminPort int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE servers SET admin_port = ? WHERE id = ? AND container_id = ?`,
		nullPort(adminPort), id, containerID)
	return mapErr("set server admin port", err)
}

func nullPort(p int) any {
	if p <= 0 {
		return nil
	}
	return p
}

// ClearServerContainer forgets the recorded container (and its admin
// endpoint) of server id if it is still containerID (so a stale caller
// cannot clear a newer record).
func (s *Store) ClearServerContainer(ctx context.Context, id int64, containerID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE servers SET container_id = NULL, admin_port = NULL, admin_token = NULL, start_complete = 0 WHERE id = ? AND container_id = ?`,
		id, containerID)
	return mapErr("clear server container", err)
}

// SetServerPendingLaunch records the identity for a Docker create before
// issuing it, so a lost reply does not orphan the resulting container.
func (s *Store) SetServerPendingLaunch(ctx context.Context, id int64, launch string) error {
	err := s.durableWrite(ctx, "UPDATE servers SET pending_launch = ? WHERE id = ? AND container_id IS NULL", launch, id)
	return mapErr("record server launch", err)
}

// CompleteServerStart commits the health-gated start after the restart policy.
func (s *Store) CompleteServerStart(ctx context.Context, id int64, containerID string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE servers SET start_complete = 1 WHERE id = ? AND container_id = ?", id, containerID)
	if err != nil {
		return mapErr("complete server start", err)
	}
	return checkAffected("complete server start", res)
}

// DeleteServer deletes server id.
func (s *Store) DeleteServer(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM servers WHERE id = ?`, id)
	if err != nil {
		return mapErr("delete server", err)
	}
	return checkAffected("delete server", res)
}

// LatestSuccessfulBuild returns the newest successful build of project
// projectID whose commit is commit (any commit when commit is empty), or
// ErrNotFound.
func (s *Store) LatestSuccessfulBuild(ctx context.Context, projectID int64, commit string) (Build, error) {
	q := `SELECT ` + buildCols + ` FROM builds WHERE project_id = ? AND status = 'success' AND image_id <> ''`
	args := []any{projectID}
	if commit != "" {
		q += ` AND commit_hash = ?`
		args = append(args, commit)
	}
	b, err := scanBuild(s.db.QueryRowContext(ctx, q+` ORDER BY id DESC LIMIT 1`, args...))
	if err != nil {
		return Build{}, mapErr("latest successful build", err)
	}
	return b, nil
}

// BuildsByImage returns, for every image id recorded on a successful build,
// the ids of those builds (sorted).
func (s *Store) BuildsByImage(ctx context.Context) (map[string][]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT image_id, id FROM builds WHERE status = 'success' AND image_id <> '' ORDER BY id`)
	if err != nil {
		return nil, mapErr("list build images", err)
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var img string
		var id int64
		if err := rows.Scan(&img, &id); err != nil {
			return nil, mapErr("list build images", err)
		}
		out[img] = append(out[img], id)
	}
	return out, mapErr("list build images", rows.Err())
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
