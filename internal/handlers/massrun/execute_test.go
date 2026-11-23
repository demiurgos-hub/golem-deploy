package massrun

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/api"
	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/handlers/docker/container"
)

func setupMassRunDB(t *testing.T) {
	t.Helper()
	if err := database.InitMassRun(filepath.Join(t.TempDir(), "massrun.db")); err != nil {
		t.Fatalf("init massrun db: %v", err)
	}
	t.Cleanup(func() { _ = database.CloseMassRun() })
}

func addPreset(t *testing.T, name, containerName string) {
	t.Helper()
	if err := database.AddMassRunConfig(&database.MassRunConfig{
		Name:          name,
		ContainerName: containerName,
		Port:          "7777",
		Label:         "default",
		EnvVars:       map[string]string{},
	}); err != nil {
		t.Fatalf("add preset %q: %v", name, err)
	}
}

func postExecute(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/massrun/execute", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	ExecuteHandler(rr, req)
	return rr
}

func TestExecuteMissingImage(t *testing.T) {
	setupMassRunDB(t)

	for _, body := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"image_name":""}`),
		[]byte(`{"image_name":"   "}`),
	} {
		rr := postExecute(t, body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status %d: %s", body, rr.Code, rr.Body.String())
		}
		if !bytes.Contains(rr.Body.Bytes(), []byte("image_name")) {
			t.Fatalf("body %s: expected image_name detail: %s", body, rr.Body.String())
		}
	}
}

func TestExecuteEmptyList(t *testing.T) {
	setupMassRunDB(t)

	rr := postExecute(t, []byte(`{"image_name":"game:latest"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("No mass run configurations found")) {
		t.Fatalf("expected empty-list detail: %s", rr.Body.String())
	}
}

func TestExecuteMixedSuccessFailure(t *testing.T) {
	setupMassRunDB(t)
	addPreset(t, "ok-preset", "ok-container")
	addPreset(t, "fail-preset", "fail-container")

	orig := runFromRequest
	t.Cleanup(func() { runFromRequest = orig })

	var seen []string
	runFromRequest = func(req api.RunContainerRequest) (*container.RunResult, error) {
		seen = append(seen, req.ContainerName)
		if req.ImageName != "game:latest" {
			return nil, fmt.Errorf("unexpected image %q", req.ImageName)
		}
		if req.ContainerName == "fail-container" {
			return nil, fmt.Errorf("boom")
		}
		return &container.RunResult{
			ContainerID:   "cid",
			ContainerName: req.ContainerName,
			ImageName:     req.ImageName,
		}, nil
	}

	rr := postExecute(t, []byte(`{"image_name":"game:latest"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}

	var resp api.ExecuteMassRunResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "partial" {
		t.Fatalf("status %q", resp.Status)
	}
	if resp.Started != 1 {
		t.Fatalf("started %d", resp.Started)
	}
	if len(resp.Failed) != 1 || resp.Failed[0].Name != "fail-preset" || resp.Failed[0].Detail != "boom" {
		t.Fatalf("failed %+v", resp.Failed)
	}
	if len(seen) != 2 {
		t.Fatalf("ran %d presets, want 2: %v", len(seen), seen)
	}
}
