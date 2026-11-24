package servers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// ScheduleInterval is how often the daemon checks the backup schedules.
const ScheduleInterval = time.Minute

// ScheduleRetryDelay is the least time between a failed scheduled backup of
// a server and the next attempt.
const ScheduleRetryDelay = 15 * time.Minute

// JobStarter starts body as a server.backup job for server id with no
// creator (the daemon passes the job id with WithJobID). A server that
// already has an active job returns an error, which the scheduler ignores.
type JobStarter func(id int64, body func(ctx context.Context, log io.Writer) error) error

// ScheduleBackups starts the scheduled backups that are due: for every
// server with a schedule whose container is running, when its newest
// backup of any kind is older than the interval (or it has none) and its
// last scheduled backup did not fail within ScheduleRetryDelay. It returns
// the ids of the servers it started a job for.
func (s *Service) ScheduleBackups(ctx context.Context, start JobStarter) ([]int64, error) {
	rows, err := s.store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	due := false
	for _, sv := range rows {
		if sv.BackupIntervalHours > 0 {
			due = true
			break
		}
	}
	if !due {
		return nil, nil
	}
	cts, err := s.docker.ManagedContainers(ctx)
	if err != nil {
		return nil, err
	}
	byServer := s.containers(rows, cts)
	last, err := s.store.LastBackups(ctx)
	if err != nil {
		return nil, err
	}
	now := s.cfg.Now()
	var started []int64
	for _, sv := range rows {
		if sv.BackupIntervalHours <= 0 {
			continue
		}
		if ct := byServer[sv.ID]; ct == nil || !adminRunning(*ct) {
			continue
		}
		if t, ok := last[sv.ID]; ok && now.Sub(t) < time.Duration(sv.BackupIntervalHours)*time.Hour {
			continue
		}
		s.schedMu.Lock()
		failed, ok := s.schedFailed[sv.ID]
		s.schedMu.Unlock()
		if ok && now.Sub(failed) < ScheduleRetryDelay {
			continue
		}
		id := sv.ID
		if err := start(id, func(ctx context.Context, log io.Writer) error { return s.scheduledBackup(ctx, id, log) }); err != nil {
			continue // busy, or the daemon is stopping
		}
		started = append(started, id)
	}
	return started, nil
}

// scheduledBackup takes a scheduled backup and applies the server's
// retention afterwards; a failure delays the next attempt.
func (s *Service) scheduledBackup(ctx context.Context, id int64, log io.Writer) error {
	fmt.Fprintln(log, "scheduled backup")
	err := s.Backup(ctx, id, BackupOptions{Kind: store.BackupScheduled}, log)
	s.schedMu.Lock()
	if err != nil && ctx.Err() == nil {
		s.schedFailed[id] = s.cfg.Now()
	} else {
		delete(s.schedFailed, id)
	}
	s.schedMu.Unlock()
	if err != nil {
		return err
	}
	s.applyRetention(ctx, id, log)
	return nil
}

// applyRetention deletes the server's oldest scheduled backups beyond its
// keep setting. Backups a running job reads are kept for now.
func (s *Service) applyRetention(ctx context.Context, id int64, log io.Writer) {
	sv, err := s.load(ctx, id)
	if err != nil {
		return
	}
	list, err := s.store.ListBackups(ctx, store.BackupFilter{ServerID: &id, Kind: store.BackupScheduled})
	if err != nil {
		s.logger.Warn("could not list scheduled backups for retention", "server_id", id, "err", err)
		return
	}
	keep := max(sv.BackupKeep, 1)
	for i := keep; i < len(list); i++ {
		b := list[i]
		err := s.DeleteBackup(context.WithoutCancel(ctx), b.ID)
		switch {
		case errors.Is(err, ErrBackupInUse):
			fmt.Fprintf(log, "kept scheduled backup %d beyond the retention of %d: a running job reads it\n", b.ID, keep)
		case err != nil && !errors.Is(err, ErrBackupNotFound):
			s.logger.Warn("could not delete a scheduled backup", "server_id", id, "backup_id", b.ID, "err", err)
		case err == nil:
			fmt.Fprintf(log, "deleted scheduled backup %d (the schedule keeps %d)\n", b.ID, keep)
		}
	}
}
