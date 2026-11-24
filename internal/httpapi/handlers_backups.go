package httpapi

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Saved data and backups (see docs/BACKUPS.md). The service does the work;
// these handlers decode, check what can be checked before a job starts and
// start the job.

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	var serverID *int64
	if v := r.URL.Query().Get("server_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return badRequest("server_id must be a positive integer")
		}
		serverID = &n
	}
	list, err := s.servers.ListBackups(r.Context(), serverID)
	if err != nil {
		return serversError("backup", err)
	}
	writeJSON(w, http.StatusOK, list)
	return nil
}

func (s *Server) handleGetBackup(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	b, err := s.servers.GetBackup(r.Context(), id)
	if err != nil {
		return serversError("backup", err)
	}
	writeJSON(w, http.StatusOK, servers.BackupView(b))
	return nil
}

// backupFileName is the download name of a backup: only characters that
// need no quoting anywhere.
func backupFileName(b store.Backup) string {
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return -1
	}, b.ServerName)
	if name == "" {
		name = "server"
	}
	if len(name) > 63 {
		name = name[:63]
	}
	return name + "-backup-" + strconv.FormatInt(b.ID, 10) + "-" + b.CreatedAt.UTC().Format("20060102T150405Z") + ".tar.gz"
}

// handleDownloadBackup returns a backup's archive (Range and conditional
// requests through http.ServeContent; the ETag is the archive's SHA-256).
func (s *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	f, _, b, err := s.servers.OpenBackup(r.Context(), id)
	if err != nil {
		return serversError("backup", err)
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", "application/gzip")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": backupFileName(b)}))
	h.Set("ETag", `"`+b.SHA256+`"`)
	http.ServeContent(&deadlineResponseWriter{ResponseWriter: w, rc: http.NewResponseController(w)}, r, "", b.CreatedAt, f)
	return nil
}

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := s.servers.DeleteBackup(r.Context(), id); err != nil {
		return serversError("backup", err)
	}
	s.logger.Info("backup deleted", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID, "backup_id", id)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// serverForJob checks that server id exists and has no active job.
func (s *Server) serverForJob(r *http.Request, id int64) error {
	if _, err := s.store.GetServer(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		return notFound("server")
	} else if err != nil {
		return internal(err)
	}
	if _, busy := s.jobs.ActiveFor(jobs.Subject{Type: string(apitypes.SubjectTypeServer), ID: jobs.SubjectID(id)}); busy {
		return conflict(apitypes.ErrorCodeJobInProgress, "a job is active for this server; wait for it or cancel it")
	}
	return nil
}

func creatorOf(r *http.Request) *int64 {
	if id := identityFrom(r); id != nil {
		v := id.Account.ID
		return &v
	}
	return nil
}

// handleCreateBackup starts a server.backup job. The body is optional.
func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req apitypes.CreateBackupRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			return err
		}
	}
	note := strings.TrimSpace(req.Note)
	if err := servers.ValidateNote(note); err != nil {
		return serversError("backup", err)
	}
	if err := s.serverForJob(r, id); err != nil {
		return err
	}
	o := servers.BackupOptions{Kind: store.BackupManual, Note: note, CreatedBy: creatorOf(r)}
	job, err := s.startServerJob(r, apitypes.JobKindServerBackup, id, func(ctx context.Context, id int64, log io.Writer) error {
		return s.servers.Backup(ctx, id, o, log)
	})
	if err != nil {
		return err
	}
	s.logger.Info("server backup started", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID, "server_id", id, "job_id", job.ID)
	writeJSON(w, http.StatusAccepted, job)
	return nil
}

// handleRestoreServer checks a restore (backup, archive checksum, build and
// image, schema rule) and starts a restricted server.restore job, which
// checks it again.
func (s *Server) handleRestoreServer(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req apitypes.RestoreServerRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if req.BackupID <= 0 {
		return invalidField("backup_id", "backup_id is required (the id of a backup)")
	}
	if err := s.serverForJob(r, id); err != nil {
		return err
	}
	o := servers.RestoreOptions{BackupID: req.BackupID, Force: req.Force, SkipSafetyBackup: req.SkipSafetyBackup, CreatedBy: creatorOf(r)}
	if err := s.servers.CheckRestore(r.Context(), id, o); err != nil {
		return serversError("server", err)
	}
	job, err := s.startServerJob(r, apitypes.JobKindServerRestore, id, func(ctx context.Context, id int64, log io.Writer) error {
		return s.servers.Restore(ctx, id, o, log)
	})
	if err != nil {
		return err
	}
	s.logger.Info("server restore started", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID, "server_id", id,
		"backup_id", req.BackupID, "force", req.Force, "skip_safety_backup", req.SkipSafetyBackup, "job_id", job.ID)
	writeJSON(w, http.StatusAccepted, job)
	return nil
}

// handleResetServerData starts a restricted server.data_reset job. The
// body is optional.
func (s *Server) handleResetServerData(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req apitypes.ResetServerDataRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			return err
		}
	}
	if err := s.serverForJob(r, id); err != nil {
		return err
	}
	o := servers.ResetOptions{SkipSafetyBackup: req.SkipSafetyBackup, CreatedBy: creatorOf(r)}
	job, err := s.startServerJob(r, apitypes.JobKindServerDataReset, id, func(ctx context.Context, id int64, log io.Writer) error {
		return s.servers.ResetData(ctx, id, o, log)
	})
	if err != nil {
		return err
	}
	s.logger.Info("server data reset started", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID, "server_id", id,
		"skip_safety_backup", req.SkipSafetyBackup, "job_id", job.ID)
	writeJSON(w, http.StatusAccepted, job)
	return nil
}
