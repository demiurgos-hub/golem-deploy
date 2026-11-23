package repo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/api"
	"github.com/demiurgos-hub/golem-deploy/internal/database"
)

func setupRepoHandlerDB(t *testing.T) {
	t.Helper()
	if err := database.Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
}

func TestJobHandlerUnknownID(t *testing.T) {
	setupRepoHandlerDB(t)

	req := httptest.NewRequest(http.MethodGet, "/repo/job?job_id=404", nil)
	rr := httptest.NewRecorder()
	JobHandler(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
}

func TestJobHandlerMissingAndInvalidID(t *testing.T) {
	setupRepoHandlerDB(t)

	req := httptest.NewRequest(http.MethodGet, "/repo/job", nil)
	rr := httptest.NewRecorder()
	JobHandler(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing id status %d: %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/repo/job?job_id=nope", nil)
	rr = httptest.NewRecorder()
	JobHandler(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status %d: %s", rr.Code, rr.Body.String())
	}
}

func TestJobHandlerReturnsCreatedJob(t *testing.T) {
	setupRepoHandlerDB(t)

	repoID, err := database.AddRepository(&database.Repository{
		Name:     "poll-repo",
		URL:      "file:///no-such-golem-deploy-repo.git",
		Branch:   "main",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("add repository: %v", err)
	}

	jobID, err := database.CreateRepoJob(int(repoID), database.RepoJobKindBranch)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/repo/job?job_id=%d", jobID), nil)
	rr := httptest.NewRecorder()
	JobHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}

	var resp api.RepoJobResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "success" || resp.Job.ID != jobID || resp.Job.Status != database.RepoJobStatusRunning {
		t.Fatalf("response: %+v", resp)
	}
	if resp.Job.Kind != database.RepoJobKindBranch || resp.Job.RepoID != int(repoID) {
		t.Fatalf("job fields: %+v", resp.Job)
	}
}

func TestPullStartReturnsJobIDAndCompletes(t *testing.T) {
	setupRepoHandlerDB(t)

	repoID, err := database.AddRepository(&database.Repository{
		Name:     "start-repo",
		URL:      "file:///no-such-golem-deploy-repo.git",
		Branch:   "main",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("add repository: %v", err)
	}

	body, _ := json.Marshal(map[string]int{"repo_id": int(repoID)})
	req := httptest.NewRequest(http.MethodPost, "/repo/pull", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	PullHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("pull status %d: %s", rr.Code, rr.Body.String())
	}

	var start map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	jobIDVal, ok := start["job_id"]
	if !ok {
		t.Fatalf("missing job_id: %v", start)
	}
	jobID := int(jobIDVal.(float64))
	if jobID <= 0 {
		t.Fatalf("invalid job_id: %v", jobIDVal)
	}

	deadline := time.Now().Add(10 * time.Second)
	var job *database.RepoJob
	for time.Now().Before(deadline) {
		job, err = database.GetRepoJob(jobID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if job.Status != database.RepoJobStatusRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if job == nil || job.Status == database.RepoJobStatusRunning {
		t.Fatal("job did not leave running")
	}
	if job.Status != database.RepoJobStatusError {
		t.Fatalf("expected error job, got %+v", job)
	}
	if job.Detail == "" {
		t.Fatal("expected terminal error detail")
	}

	if err := database.UpdateRepositoryStatus(int(repoID), "pulling"); err != nil {
		t.Fatal(err)
	}
	conflict := httptest.NewRequest(http.MethodPost, "/repo/pull", bytes.NewReader(body))
	conflict.Header.Set("Content-Type", "application/json")
	conflictRR := httptest.NewRecorder()
	PullHandler(conflictRR, conflict)
	if conflictRR.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", conflictRR.Code, conflictRR.Body.String())
	}
}
