package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/backups"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// TestIntegrationBackupsAPI drives saved data through the API against a
// real Docker daemon (GOLEM_DEPLOY_DOCKER_TESTS=1) with the fake game,
// which keeps an engine-style snapshot in GOLEM_SNAPSHOT_DIR: the data
// volume survives a restart, a live backup asks the game for a snapshot, a
// restore lands in a fresh volume the game (uid 65532) loads and writes,
// a reset gives an empty world, and deleting the server removes its volume
// and keeps its backups.
func TestIntegrationBackupsAPI(t *testing.T) {
	if os.Getenv("GOLEM_DEPLOY_DOCKER_TESTS") != "1" {
		t.Skip("set GOLEM_DEPLOY_DOCKER_TESTS=1 to run tests against a real Docker daemon")
	}
	ctx := context.Background()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	sfx := hex.EncodeToString(b)
	_, netErr := exec.Command("docker", "network", "inspect", docker.NetworkName).Output()
	networkExisted := netErr == nil
	name := "gdm5-itbak-" + sfx
	t.Cleanup(func() {
		itestQuiet("rm", "--force", docker.ContainerName(name))
		itestRemoveVolumes(name)
		if !networkExisted {
			itestQuiet("network", "rm", docker.NetworkName)
		}
	})

	h := newHarness(t)
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	proj, err := h.st.CreateProject(ctx, store.Project{Name: "itbak", RepoURL: "https://example.com/itbak.git"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	build, _ := h.st.CreateBuild(ctx, store.NewBuild{ProjectID: proj.ID})
	commit := strings.Repeat("d", 40)
	_ = h.st.MarkBuildRunning(ctx, build.ID, "main", commit)
	tag := "golem-deploy/itbak-" + sfx + ":" + commit
	imageID := gameadmintest.BuildFakeGameImage(t, tag,
		docker.LabelManaged+"=true", docker.LabelInstance+"="+dockertest.IntegrationInstanceID,
		docker.LabelProjectID+"="+strconv.FormatInt(proj.ID, 10), docker.LabelCommit+"="+commit,
		docker.LabelBuildID+"="+strconv.FormatInt(build.ID, 10), docker.LabelEngineVersion+"=v0.4.0-rc.1")
	if err := h.st.FinishBuild(ctx, build.ID, store.BuildResult{Status: store.BuildStatusSuccess, ImageID: imageID, ImageTag: tag, EngineVersion: "v0.4.0-rc.1"}); err != nil {
		t.Fatal(err)
	}

	dc := docker.New(docker.Options{Logger: logger, InstanceID: dockertest.IntegrationInstanceID, TempDir: t.TempDir()})
	box, _ := secrets.New(bytes.Repeat([]byte{3}, secrets.KeySize))
	cm, err := certs.Open(certs.Options{Dir: filepath.Join(t.TempDir(), "certs", "game"), Settings: h.st, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	bs, err := backups.Open(filepath.Join(t.TempDir(), "backups"), backups.Options{})
	if err != nil {
		t.Fatal(err)
	}
	lo := 0
	for p := 7400; p < 7590; p += 10 {
		if ports.ProbeFree("127.0.0.1", p) == nil {
			lo = p
			break
		}
	}
	if lo == 0 {
		t.Fatal("no free port window")
	}
	svc := servers.New(h.st, box, dc, servers.Config{Ports: ports.Range{Lo: lo, Hi: lo + 9}, BindAddress: "127.0.0.1", StopTimeout: 5 * time.Second,
		StartTimeout: 20 * time.Second, DefaultGameHost: "localhost", Certs: cm, Backups: bs}, logger)
	runner := jobs.New(h.st, t.TempDir(), jobs.Options{Logger: logger})
	t.Cleanup(func() { _ = runner.Shutdown(context.Background()) })
	srv, err := New(Config{Listen: "127.0.0.1:0", PublicHosts: []string{"example.com"}}, Deps{Store: h.st, Sessions: h.sessions, Hasher: h.hasher,
		Secrets: box, Jobs: runner, Logger: logger, Docker: dc, Servers: svc, Certs: cm})
	if err != nil {
		t.Fatal(err)
	}
	h.srv, h.runner = srv, runner
	tok := h.session(h.account("admin1", store.RoleAdmin, true))
	wait := func(rr *httptest.ResponseRecorder) (store.Job, string) {
		t.Helper()
		if rr.Code != http.StatusAccepted {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
		job := decode[apitypes.Job](t, rr)
		select {
		case <-runner.Done(job.ID):
		case <-time.After(2 * time.Minute):
			t.Fatal("job timeout")
		}
		j, _ := h.st.GetJob(ctx, job.ID)
		log, _ := os.ReadFile(runner.LogPath(j.ID))
		if j.Status != store.JobSuccess {
			t.Fatalf("job %s %q\n%s", j.Status, j.Detail, log)
		}
		return j, string(log)
	}
	serverLog := func(id int64) string {
		t.Helper()
		rr := h.do("GET", fmt.Sprintf("/api/v1/servers/%d/logs?tail=200", id), tok, nil)
		return rr.Body.String()
	}
	get := func(id int64) apitypes.Server {
		return decode[apitypes.Server](t, h.do("GET", fmt.Sprintf("/api/v1/servers/%d", id), tok, nil))
	}
	volumeExists := func(v string) bool { return exec.Command("docker", "volume", "inspect", v).Run() == nil }

	rr := h.do("POST", "/api/v1/servers", tok, apitypes.CreateServerRequest{Name: name, BuildID: build.ID, Env: map[string]string{"FAKEGAME_POLL": "1s"}})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	sv := decode[apitypes.Server](t, rr)
	base := fmt.Sprintf("/api/v1/servers/%d", sv.ID)
	wait(h.do("POST", base+"/start", tok, nil))
	got := get(sv.ID)
	vol1 := got.Data.Volume
	if vol1 == "" || got.Data.SchemaFingerprint != "itest-fp-1" || !strings.Contains(serverLog(sv.ID), "no snapshot; starting an empty world") {
		t.Fatalf("%+v\n%s", got.Data, serverLog(sv.ID))
	}
	mounts := itestDocker(t, "", "container", "inspect", "--format", "{{range .Mounts}}{{.Type}} {{.Name}} {{.Destination}} {{.RW}};{{end}}", got.ContainerID)
	if !strings.Contains(mounts, "volume "+vol1+" /data true;") {
		t.Fatalf("mounts %q", mounts)
	}

	// The game saves when it stops and loads the snapshot when it starts.
	wait(h.do("POST", base+"/restart", tok, nil))
	if again := get(sv.ID); again.Data.Volume != vol1 || !strings.Contains(serverLog(sv.ID), "loaded snapshot itest-fp-1") {
		t.Fatalf("%+v\n%s", again.Data, serverLog(sv.ID))
	}

	// A live backup.
	_, log := wait(h.do("POST", base+"/backups", tok, apitypes.CreateBackupRequest{Note: "itest"}))
	list := decode[apitypes.BackupList](t, h.do("GET", "/api/v1/backups?server_id="+strconv.FormatInt(sv.ID, 10), tok, nil))
	if len(list.Items) != 1 || !list.Items[0].Live || list.Items[0].SchemaFingerprint != "itest-fp-1" || list.Items[0].FileCount != 2 {
		t.Fatalf("%+v\n%s", list, log)
	}
	bk := list.Items[0]
	if helpers := itestDocker(t, "", "ps", "--all", "--quiet", "--filter", "name=golem-deploy_helper_"); helpers != "" {
		t.Fatalf("helpers left: %s", helpers)
	}

	// Restore into a fresh volume; the game loads it and can write it.
	_, log = wait(h.do("POST", base+"/restore", tok, apitypes.RestoreServerRequest{BackupID: bk.ID}))
	restored := get(sv.ID)
	if restored.Data.Volume == vol1 || volumeExists(vol1) || restored.State != apitypes.ServerStateRunning {
		t.Fatalf("%+v\n%s", restored, log)
	}
	if out := serverLog(sv.ID); !strings.Contains(out, "loaded snapshot itest-fp-1") {
		t.Fatalf("server log:\n%s", out)
	}
	_, log = wait(h.do("POST", base+"/backups", tok, nil))
	if !strings.Contains(log, "the game wrote a fresh snapshot") {
		t.Fatalf("the game could not write the restored volume:\n%s", log)
	}

	// Reset: an empty world in a new volume.
	_, log = wait(h.do("POST", base+"/data/reset", tok, apitypes.ResetServerDataRequest{SkipSafetyBackup: true}))
	reset := get(sv.ID)
	if reset.Data.Volume == restored.Data.Volume || volumeExists(restored.Data.Volume) || !strings.Contains(serverLog(sv.ID), "no snapshot; starting an empty world") {
		t.Fatalf("%+v\n%s", reset.Data, log)
	}

	// Delete: the volume goes, the backups stay.
	rr = h.do("DELETE", base, tok, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
	if volumeExists(reset.Data.Volume) {
		t.Fatal("volume kept after delete")
	}
	all := decode[apitypes.BackupList](t, h.do("GET", "/api/v1/backups", tok, nil))
	if len(all.Items) != 3 || all.Items[0].ServerID != nil {
		t.Fatalf("%+v", all.Items)
	}
}
