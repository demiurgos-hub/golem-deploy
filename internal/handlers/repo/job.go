package repo

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/demiurgos-hub/golem-deploy/internal/api"
	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// JobHandler returns a pollable repo job by id.
func JobHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		utils.ErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobIDStr := r.URL.Query().Get("job_id")
	if jobIDStr == "" {
		utils.ErrorResponse(w, "Job ID is required", http.StatusBadRequest)
		return
	}

	jobID, err := strconv.Atoi(jobIDStr)
	if err != nil || jobID <= 0 {
		utils.ErrorResponse(w, "Invalid job ID", http.StatusBadRequest)
		return
	}

	job, err := database.GetRepoJob(jobID)
	if err != nil {
		if errors.Is(err, database.ErrRepoJobNotFound) {
			utils.ErrorResponse(w, "Job not found", http.StatusNotFound)
			return
		}
		utils.ErrorResponse(w, fmt.Sprintf("Failed to get job: %v", err), http.StatusInternalServerError)
		return
	}

	utils.WriteJSON(w, http.StatusOK, api.RepoJobResponse{
		Status: "success",
		Job: api.RepoJob{
			ID:         job.ID,
			RepoID:     job.RepoID,
			Kind:       job.Kind,
			Status:     job.Status,
			Detail:     job.Detail,
			CreatedAt:  job.CreatedAt,
			FinishedAt: job.FinishedAt,
		},
	})
}

// launchRepoJob persists a running job before starting work. If the goroutine
// never starts, the job is marked error. previousStatus is restored when insert fails.
func launchRepoJob(repoID int, kind, previousStatus string, work func() (string, error)) (int, error) {
	jobID, err := database.CreateRepoJob(repoID, kind)
	if err != nil {
		_ = database.UpdateRepositoryStatus(repoID, previousStatus)
		return 0, err
	}

	started := false
	defer func() {
		if !started {
			_ = database.CompleteRepoJob(jobID, database.RepoJobStatusError, "failed to start job")
		}
	}()

	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				_ = database.UpdateRepositoryStatus(repoID, "error")
				_ = database.CompleteRepoJob(jobID, database.RepoJobStatusError, fmt.Sprintf("%v", rec))
			}
		}()

		detail, workErr := work()
		if workErr != nil {
			_ = database.CompleteRepoJob(jobID, database.RepoJobStatusError, workErr.Error())
			return
		}
		_ = database.CompleteRepoJob(jobID, database.RepoJobStatusSuccess, detail)
	}()
	started = true
	return jobID, nil
}
