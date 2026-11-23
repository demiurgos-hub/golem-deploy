package database

import (
	"errors"
	"path/filepath"
	"testing"
)

func setupRepoJobDB(t *testing.T) {
	t.Helper()
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = Close() })
}

func TestRepoJobCreateCompleteGet(t *testing.T) {
	setupRepoJobDB(t)

	repoID, err := AddRepository(&Repository{
		Name:     "job-repo",
		URL:      "file:///no-such-golem-deploy-repo.git",
		Branch:   "main",
		AuthType: "none",
		Status:   "idle",
	})
	if err != nil {
		t.Fatalf("add repository: %v", err)
	}

	jobID, err := CreateRepoJob(int(repoID), RepoJobKindPull)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if jobID <= 0 {
		t.Fatalf("expected positive job id, got %d", jobID)
	}

	running, err := GetRepoJob(jobID)
	if err != nil {
		t.Fatalf("get running job: %v", err)
	}
	if running.RepoID != int(repoID) || running.Kind != RepoJobKindPull || running.Status != RepoJobStatusRunning {
		t.Fatalf("running job: %+v", running)
	}
	if running.FinishedAt != "" {
		t.Fatalf("running job should not have finished_at: %q", running.FinishedAt)
	}
	if running.CreatedAt == "" {
		t.Fatal("running job missing created_at")
	}

	if err := CompleteRepoJob(jobID, RepoJobStatusError, "clone failed: no such repo"); err != nil {
		t.Fatalf("complete job: %v", err)
	}

	done, err := GetRepoJob(jobID)
	if err != nil {
		t.Fatalf("get completed job: %v", err)
	}
	if done.Status != RepoJobStatusError || done.Detail != "clone failed: no such repo" {
		t.Fatalf("completed job: %+v", done)
	}
	if done.FinishedAt == "" {
		t.Fatal("completed job missing finished_at")
	}

	successID, err := CreateRepoJob(int(repoID), RepoJobKindBranch)
	if err != nil {
		t.Fatalf("create success job: %v", err)
	}
	if err := CompleteRepoJob(successID, RepoJobStatusSuccess, ""); err != nil {
		t.Fatalf("complete success job: %v", err)
	}
	success, err := GetRepoJob(successID)
	if err != nil {
		t.Fatalf("get success job: %v", err)
	}
	if success.Status != RepoJobStatusSuccess || success.Detail != "" {
		t.Fatalf("success job: %+v", success)
	}
}

func TestGetRepoJobUnknownID(t *testing.T) {
	setupRepoJobDB(t)

	_, err := GetRepoJob(99999)
	if !errors.Is(err, ErrRepoJobNotFound) {
		t.Fatalf("expected ErrRepoJobNotFound, got %v", err)
	}
}
