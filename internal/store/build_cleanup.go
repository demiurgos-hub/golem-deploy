package store

import (
	"context"
	"iter"
	"time"
)

// ProjectBuilds pages over a project's history by descending id. The
// cursor stays valid if the caller deletes builds from the current page.
func (s *Store) ProjectBuilds(ctx context.Context, projectID int64) iter.Seq2[Build, error] {
	return func(yield func(Build, error) bool) {
		filter := BuildFilter{ProjectID: &projectID, Limit: 500}
		for {
			page, err := s.ListBuilds(ctx, filter)
			if err != nil {
				yield(Build{}, err)
				return
			}
			for _, b := range page {
				if !yield(b, nil) {
					return
				}
			}
			if len(page) < filter.Limit {
				return
			}
			filter.BeforeID = page[len(page)-1].ID
		}
	}
}

func (s *Store) PendingBuildImages(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT image_id FROM build_image_cleanup ORDER BY image_id`)
	if err != nil {
		return nil, mapErr("list pending build images", err)
	}
	defer rows.Close()
	var images []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, mapErr("list pending build images", err)
		}
		images = append(images, id)
	}
	return images, mapErr("list pending build images", rows.Err())
}

func (s *Store) ClearBuildImageCleanup(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM build_image_cleanup WHERE image_id = ?`, id)
	return mapErr("clear build image cleanup", err)
}

// BuildExport identifies a potentially unpublished image by its tag and labels.
type BuildExport struct {
	BuildID, ProjectID                 int64
	ProjectName, Commit, EngineVersion string
	// EligibleSince is when the build finished, or the last durable time
	// available once its record has been deleted.
	EligibleSince time.Time
}

func (s *Store) RecordBuildExport(ctx context.Context, e BuildExport) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO build_exports
		(build_id, project_id, project_name, commit_hash, engine_version, recorded_at) VALUES (?, ?, ?, ?, ?, ?)`,
		e.BuildID, e.ProjectID, e.ProjectName, e.Commit, e.EngineVersion, formatTime(s.nowUTC()))
	return mapErr("record build export", err)
}

func (s *Store) PendingBuildExports(ctx context.Context) ([]BuildExport, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.build_id, e.project_id, e.project_name, e.commit_hash, e.engine_version,
		e.recorded_at FROM build_exports e LEFT JOIN builds b ON b.id = e.build_id
		WHERE b.id IS NULL OR b.status IN ('error', 'cancelled') ORDER BY e.build_id`)
	if err != nil {
		return nil, mapErr("list pending build exports", err)
	}
	defer rows.Close()
	var exports []BuildExport
	for rows.Next() {
		var e BuildExport
		var since string
		if err := rows.Scan(&e.BuildID, &e.ProjectID, &e.ProjectName, &e.Commit, &e.EngineVersion, &since); err != nil {
			return nil, mapErr("list pending build exports", err)
		}
		e.EligibleSince, err = parseTime(since)
		if err != nil {
			return nil, err
		}
		exports = append(exports, e)
	}
	return exports, mapErr("list pending build exports", rows.Err())
}

func (s *Store) ClearBuildExport(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM build_exports WHERE build_id = ?`, id)
	return mapErr("clear build export", err)
}
