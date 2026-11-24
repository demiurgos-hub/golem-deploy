package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Server data columns. Only server jobs (serialised per server) write them.

// SetServerDataVolume records the data volume a start job just created for
// server id. clearFingerprint forgets the recorded schema fingerprint (the
// previous volume is gone, so is the data it described).
func (s *Store) SetServerDataVolume(ctx context.Context, id int64, volume string, clearFingerprint bool) error {
	q := `UPDATE servers SET data_volume = ? WHERE id = ?`
	if clearFingerprint {
		q = `UPDATE servers SET data_volume = ?, data_fingerprint = '' WHERE id = ?`
	}
	err := s.durableWrite(ctx, q, volume, id)
	return mapErr("set server data volume", err)
}

// SwitchServerData replaces server id's data volume and its recorded
// fingerprint in one write, if the row still names oldVolume (ErrStale
// otherwise). A restore switches to a fresh volume this way, a reset to no
// volume ("").
func (s *Store) SwitchServerData(ctx context.Context, id int64, oldVolume, newVolume, fingerprint string) error {
	err := s.durableWrite(ctx, `UPDATE servers SET data_volume = ?, data_fingerprint = ? WHERE id = ? AND data_volume = ?`,
		newVolume, fingerprint, id, oldVolume)
	if errors.Is(err, ErrNotFound) {
		if _, gerr := s.GetServer(ctx, id); gerr == nil {
			return ErrStale
		}
	}
	return mapErr("switch server data", err)
}

// SetServerDataFingerprint records the schema fingerprint the game reported
// for server id's saved data, if the row still names volume.
func (s *Store) SetServerDataFingerprint(ctx context.Context, id int64, volume, fingerprint string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE servers SET data_fingerprint = ? WHERE id = ? AND data_volume = ?`, fingerprint, id, volume)
	return mapErr("set server data fingerprint", err)
}

// Backup is a stored backup (an archive in the backups directory).
type Backup struct {
	ID                int64
	Kind              string
	File              string
	ServerID          *int64
	ServerName        string
	ProjectID         *int64
	ProjectName       string
	BuildID           *int64
	Commit            string
	EngineVersion     string
	SchemaFingerprint string
	Live              bool
	Note              string
	SizeBytes         int64
	ContentBytes      int64
	FileCount         int
	SHA256            string
	JobID             *int64
	CreatedBy         *int64
	// CreatedByUsername is read with the row; nil when CreatedBy is nil.
	CreatedByUsername *string
	CreatedAt         time.Time
}

// Backup kinds.
const (
	BackupManual     = "manual"
	BackupScheduled  = "scheduled"
	BackupPreRestore = "pre_restore"
	BackupPreReset   = "pre_reset"
)

const (
	backupCols = `backups.id, backups.kind, backups.file, backups.server_id, backups.server_name, backups.project_id, backups.project_name, backups.build_id,
		backups.commit_hash, backups.engine_version, backups.schema_fingerprint, backups.live, backups.note, backups.size_bytes, backups.content_bytes,
		backups.file_count, backups.sha256, backups.job_id, backups.created_by, backups.created_at, accounts.username`
	backupFrom = ` FROM backups LEFT JOIN accounts ON accounts.id = backups.created_by`
)

func scanBackup(r scanner) (Backup, error) {
	var b Backup
	var serverID, projectID, buildID, jobID, createdBy sql.NullInt64
	var created string
	var creator sql.NullString
	if err := r.Scan(&b.ID, &b.Kind, &b.File, &serverID, &b.ServerName, &projectID, &b.ProjectName, &buildID, &b.Commit, &b.EngineVersion,
		&b.SchemaFingerprint, &b.Live, &b.Note, &b.SizeBytes, &b.ContentBytes, &b.FileCount, &b.SHA256, &jobID, &createdBy, &created, &creator); err != nil {
		return Backup{}, err
	}
	b.ServerID, b.ProjectID, b.BuildID, b.JobID, b.CreatedBy = int64Ptr(serverID), int64Ptr(projectID), int64Ptr(buildID), int64Ptr(jobID), int64Ptr(createdBy)
	if creator.Valid && b.CreatedBy != nil {
		b.CreatedByUsername = &creator.String
	}
	var err error
	b.CreatedAt, err = parseTime(created)
	return b, err
}

// CreateBackup records a complete backup whose file is already in place.
// ID, CreatedByUsername and CreatedAt are ignored. References to rows that
// were deleted meanwhile are stored as NULL rather than failing.
func (s *Store) CreateBackup(ctx context.Context, b Backup) (Backup, error) {
	var out Backup
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		refs := []struct {
			table string
			id    **int64
		}{{"servers", &b.ServerID}, {"projects", &b.ProjectID}, {"builds", &b.BuildID}, {"jobs", &b.JobID}, {"accounts", &b.CreatedBy}}
		for _, r := range refs {
			if *r.id == nil {
				continue
			}
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+r.table+` WHERE id = ?)`, **r.id).Scan(&exists); err != nil {
				return mapErr("create backup", err)
			}
			if !exists {
				*r.id = nil
			}
		}
		var id int64
		err := tx.QueryRowContext(ctx,
			`INSERT INTO backups (kind, file, server_id, server_name, project_id, project_name, build_id, commit_hash, engine_version,
			 schema_fingerprint, live, note, size_bytes, content_bytes, file_count, sha256, job_id, created_by, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			b.Kind, b.File, nullInt64(b.ServerID), b.ServerName, nullInt64(b.ProjectID), b.ProjectName, nullInt64(b.BuildID), b.Commit, b.EngineVersion,
			b.SchemaFingerprint, b.Live, b.Note, b.SizeBytes, b.ContentBytes, b.FileCount, b.SHA256, nullInt64(b.JobID), nullInt64(b.CreatedBy),
			formatTime(s.nowUTC())).Scan(&id)
		if err != nil {
			return mapErr("create backup", err)
		}
		out, err = scanBackup(tx.QueryRowContext(ctx, `SELECT `+backupCols+backupFrom+` WHERE backups.id = ?`, id))
		return mapErr("create backup", err)
	})
	return out, err
}

// GetBackup returns backup id, or ErrNotFound.
func (s *Store) GetBackup(ctx context.Context, id int64) (Backup, error) {
	b, err := scanBackup(s.db.QueryRowContext(ctx, `SELECT `+backupCols+backupFrom+` WHERE backups.id = ?`, id))
	if err != nil {
		return Backup{}, mapErr("get backup", err)
	}
	return b, nil
}

// BackupFilter narrows ListBackups.
type BackupFilter struct {
	// ServerID selects the backups taken from one server.
	ServerID *int64
	// Kind selects one kind.
	Kind string
}

// ListBackups returns the matching backups, newest first.
func (s *Store) ListBackups(ctx context.Context, f BackupFilter) ([]Backup, error) {
	q := `SELECT ` + backupCols + backupFrom + ` WHERE 1=1`
	var args []any
	if f.ServerID != nil {
		q += ` AND backups.server_id = ?`
		args = append(args, *f.ServerID)
	}
	if f.Kind != "" {
		q += ` AND backups.kind = ?`
		args = append(args, f.Kind)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY backups.created_at DESC, backups.id DESC`, args...)
	if err != nil {
		return nil, mapErr("list backups", err)
	}
	defer rows.Close()
	out := []Backup{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, mapErr("list backups", err)
		}
		out = append(out, b)
	}
	return out, mapErr("list backups", rows.Err())
}

// BackupFiles returns the file names of every backup (startup cleanup).
func (s *Store) BackupFiles(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT file FROM backups`)
	if err != nil {
		return nil, mapErr("list backup files", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, mapErr("list backup files", err)
		}
		out[f] = true
	}
	return out, mapErr("list backup files", rows.Err())
}

// BackupTotalBytes returns the size of every stored backup.
func (s *Store) BackupTotalBytes(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM backups`).Scan(&n)
	return n, mapErr("sum backup sizes", err)
}

// LastBackups returns, per server id, when its newest backup was taken.
func (s *Store) LastBackups(ctx context.Context) (map[int64]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT server_id, MAX(created_at) FROM backups WHERE server_id IS NOT NULL GROUP BY server_id`)
	if err != nil {
		return nil, mapErr("last backups", err)
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var id int64
		var at string
		if err := rows.Scan(&id, &at); err != nil {
			return nil, mapErr("last backups", err)
		}
		t, err := parseTime(at)
		if err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, mapErr("last backups", rows.Err())
}

// DeleteBackup deletes the row of backup id and returns it (the caller
// removes the file afterwards; a file left by a crash in between is removed
// at the next start). ErrNotFound when there is no such backup.
func (s *Store) DeleteBackup(ctx context.Context, id int64) (Backup, error) {
	var out Backup
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = scanBackup(tx.QueryRowContext(ctx, `SELECT `+backupCols+backupFrom+` WHERE backups.id = ?`, id))
		if err != nil {
			return mapErr("delete backup", err)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM backups WHERE id = ?`, id)
		if err != nil {
			return mapErr("delete backup", err)
		}
		return checkAffected("delete backup", res)
	})
	return out, err
}

// VolumeHelper is a recorded helper container (see the volume_helpers
// table).
type VolumeHelper struct {
	LaunchID    string
	ServerID    int64
	ContainerID string
	CreatedAt   time.Time
}

// RecordVolumeHelper records the identity of a helper container before it
// is created (synced, so a crash right after the create can be recovered).
func (s *Store) RecordVolumeHelper(ctx context.Context, launchID string, serverID int64) error {
	return mapErr("record volume helper", s.durableWrite(ctx,
		`INSERT INTO volume_helpers (launch_id, server_id, created_at) VALUES (?, ?, ?)`, launchID, serverID, formatTime(s.nowUTC())))
}

// SetVolumeHelperContainer records the container id of a created helper.
func (s *Store) SetVolumeHelperContainer(ctx context.Context, launchID, containerID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE volume_helpers SET container_id = ? WHERE launch_id = ?`, containerID, launchID)
	return mapErr("set volume helper container", err)
}

// DeleteVolumeHelper forgets a helper once its container is gone.
func (s *Store) DeleteVolumeHelper(ctx context.Context, launchID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM volume_helpers WHERE launch_id = ?`, launchID)
	return mapErr("delete volume helper", err)
}

// ListVolumeHelpers returns every recorded helper.
func (s *Store) ListVolumeHelpers(ctx context.Context) ([]VolumeHelper, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT launch_id, server_id, container_id, created_at FROM volume_helpers ORDER BY created_at`)
	if err != nil {
		return nil, mapErr("list volume helpers", err)
	}
	defer rows.Close()
	var out []VolumeHelper
	for rows.Next() {
		var h VolumeHelper
		var created string
		if err := rows.Scan(&h.LaunchID, &h.ServerID, &h.ContainerID, &created); err != nil {
			return nil, mapErr("list volume helpers", err)
		}
		if h.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, mapErr("list volume helpers", rows.Err())
}
