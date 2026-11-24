package apitypes

import "time"

// BackupKind says why a backup was taken.
type BackupKind string

// Backup kinds. Only scheduled backups are removed by a server's retention
// setting; the others stay until an admin deletes them.
const (
	// BackupKindManual: POST /api/v1/servers/{id}/backups.
	BackupKindManual BackupKind = "manual"
	// BackupKindScheduled: taken by the server's backup schedule.
	BackupKindScheduled BackupKind = "scheduled"
	// BackupKindPreRestore and BackupKindPreReset are the safety backups the
	// daemon takes of a server's current data before a restore replaces it
	// or a reset discards it.
	BackupKindPreRestore BackupKind = "pre_restore"
	BackupKindPreReset   BackupKind = "pre_reset"
)

// Backup is a stored copy of a server's data directory (the entity snapshot
// plus whatever else the game keeps next to it). Backups outlive their
// server: after the server is deleted they can still be downloaded and
// restored into another server.
//
// A backup holds what the game wrote, so it is untrusted data. The daemon
// stores regular files and directories only (links, devices and pipes are
// left out) and never interprets the content beyond the snapshot header.
type Backup struct {
	ID   int64      `json:"id"`
	Kind BackupKind `json:"kind"`
	// ServerID is the server the backup was taken from; null once that
	// server was deleted. ServerName is its name at the time.
	ServerID   *int64 `json:"server_id" tstype:"number | null,required"`
	ServerName string `json:"server_name"`
	// ProjectID and ProjectName, BuildID, Commit and EngineVersion describe
	// the build the server was assigned when the backup was taken (ids are
	// null once the project or build was deleted).
	ProjectID     *int64 `json:"project_id" tstype:"number | null,required"`
	ProjectName   string `json:"project_name"`
	BuildID       *int64 `json:"build_id" tstype:"number | null,required"`
	Commit        string `json:"commit"`
	EngineVersion string `json:"engine_version"`
	// SchemaFingerprint is the schema fingerprint stored in the backup's
	// entity snapshot (world.snapshot); "" when the backup has no snapshot
	// or its header could not be read. A server can only load a snapshot
	// whose fingerprint equals its build's.
	SchemaFingerprint string `json:"schema_fingerprint"`
	// Live is true when the game was running and wrote a fresh snapshot for
	// this backup; false when the files were copied as they were (a stopped
	// server, or a game without the snapshot endpoint).
	Live bool `json:"live"`
	// Note is the operator's label (at most 200 characters).
	Note string `json:"note"`
	// SizeBytes is the size of the stored archive, ContentBytes the total
	// size of the files in it and FileCount their number.
	SizeBytes    int64 `json:"size_bytes"`
	ContentBytes int64 `json:"content_bytes"`
	FileCount    int   `json:"file_count"`
	// SHA256 is the lower-case hex SHA-256 of the archive
	// (GET /api/v1/backups/{id}/download returns exactly these bytes).
	SHA256 string `json:"sha256"`
	// JobID is the job that took the backup; null when that job record is
	// gone.
	JobID *int64 `json:"job_id" tstype:"number | null,required"`
	// CreatedBy is the account that asked for the backup; null for scheduled
	// backups or when that account was deleted.
	CreatedBy         *int64    `json:"created_by" tstype:"number | null,required"`
	CreatedByUsername *string   `json:"created_by_username" tstype:"string | null,required"`
	CreatedAt         time.Time `json:"created_at"`
}

// BackupList is the body of GET /api/v1/backups, newest first.
type BackupList struct {
	Items []Backup `json:"items"`
	// TotalBytes is the size of every stored backup of the installation
	// (not only of the listed ones). MaxContentBytes is the largest data
	// directory a backup accepts (--backup-max-mb).
	TotalBytes      int64 `json:"total_bytes"`
	MaxContentBytes int64 `json:"max_content_bytes"`
}

// CreateBackupRequest is the body of POST /api/v1/servers/{id}/backups. The
// body may be empty. Note is an optional single-line label of at most 200
// characters.
type CreateBackupRequest struct {
	Note string `json:"note,omitempty"`
}

// RestoreServerRequest is the body of POST /api/v1/servers/{id}/restore. The
// restore stops the server, takes a safety backup of its current data
// (kind "pre_restore"; skipped when there is none), replaces the data with
// the backup's and starts the server again if it was meant to run.
//
// Force is required when the backup's schema fingerprint and the
// fingerprint of the server's build are both known and differ (409
// schema_mismatch otherwise): such a server will not load the snapshot.
// SkipSafetyBackup restores even though the current data is not backed up
// first.
type RestoreServerRequest struct {
	BackupID         int64 `json:"backup_id"`
	Force            bool  `json:"force,omitempty"`
	SkipSafetyBackup bool  `json:"skip_safety_backup,omitempty"`
}

// ResetServerDataRequest is the body of POST /api/v1/servers/{id}/data/reset
// (the body may be empty). The reset stops the server, takes a safety
// backup of its data (kind "pre_reset"; skipped when there is none or with
// SkipSafetyBackup), discards the data and starts the server again with an
// empty world if it was meant to run.
type ResetServerDataRequest struct {
	SkipSafetyBackup bool `json:"skip_safety_backup,omitempty"`
}

// BackupSchedule is a server's automatic backup setting. IntervalHours 0
// turns it off; otherwise it is 1 to 720. A scheduled backup is taken while
// the server is running, IntervalHours after the server's previous backup of
// any kind. Keep (1 to 100) is how many scheduled backups of the server are
// kept; older ones are deleted after each scheduled backup.
type BackupSchedule struct {
	IntervalHours int `json:"interval_hours"`
	Keep          int `json:"keep"`
}

// ServerData describes a server's saved data: the directory the game keeps
// its entity snapshot in, on a Docker volume of its own.
type ServerData struct {
	// Volume is the Docker volume holding the data; "" until the server has
	// been started once (or after a reset).
	Volume string `json:"volume"`
	// SchemaFingerprint is the schema fingerprint of the saved world as far
	// as the daemon knows: what the game last reported when it became
	// ready, or the fingerprint of the backup restored since. "" when there
	// is no data or it is unknown.
	SchemaFingerprint string `json:"schema_fingerprint"`
	// SchemaMismatch is true when SchemaFingerprint and the fingerprint of
	// the server's build are both known and differ. Starting or restarting
	// the server is refused then, before its running container is touched:
	// assign a build with the saved schema, restore a matching backup or
	// reset the data.
	SchemaMismatch bool `json:"schema_mismatch"`
	// LastBackupAt is when the server's newest backup was taken; null
	// without one.
	LastBackupAt *time.Time `json:"last_backup_at" tstype:"string | null,required"`
}
