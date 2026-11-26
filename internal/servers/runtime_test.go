package servers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

const (
	commitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commitB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	marker  = "ENV-MARKER-5e7a-41"
)

type fixture struct {
	svc     *Service
	f       *dockertest.Fake
	st      *store.Store
	admin   *gameadmintest.Server
	certs   *certs.Manager
	logs    *bytes.Buffer
	project store.Project
}

// newFixture returns a service on a fake docker CLI whose images publish
// the admin port on a fake admin endpoint expecting the token of the newest
// container.
func newFixture(t *testing.T, cfg Config) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := secrets.New(bytes.Repeat([]byte{7}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	f := dockertest.New()
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dc := docker.New(docker.Options{Runner: f, InstanceID: dockertest.InstanceID, TempDir: t.TempDir(), Logger: logger})
	cm, err := certs.Open(certs.Options{Dir: filepath.Join(t.TempDir(), "certs", "game"), Settings: st})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ports.Lo == 0 {
		cfg.Ports = ports.Range{Lo: 7000, Hi: 7009}
	}
	if cfg.DefaultGameHost == "" {
		cfg.DefaultGameHost = "play.example.com"
	}
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = 2 * time.Second
	}
	cfg.Certs = cm
	cfg.ReadyPoll = 5 * time.Millisecond
	cfg.ProbePort = func(string, int) error { return nil }
	admin := gameadmintest.New(t)
	admin.Set(func(s *gameadmintest.Server) { s.ExpectToken = func() string { return f.LatestEnv(EnvAdminToken) } })
	return &fixture{svc: New(st, box, dc, cfg, logger), f: f, st: st, admin: admin, certs: cm, logs: logs}
}

// build records a successful build of commit with image n, labelled like
// the build pipeline labels it.
func (fx *fixture) build(t *testing.T, n int, commit string) store.Build {
	t.Helper()
	ctx := context.Background()
	if fx.project.ID == 0 {
		p, err := fx.st.CreateProject(ctx, store.Project{Name: "game", RepoURL: "https://example.com/game.git"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		fx.project = p
	}
	b, err := fx.st.CreateBuild(ctx, store.NewBuild{ProjectID: fx.project.ID, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	img := dockertest.ImageID(n)
	if err := fx.st.MarkBuildRunning(ctx, b.ID, "main", commit); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.FinishBuild(ctx, b.ID, store.BuildResult{Status: store.BuildStatusSuccess, ImageID: img, ImageTag: "golem-deploy/game:" + commit,
		EngineVersion: "v0.4.0-rc.1"}); err != nil {
		t.Fatal(err)
	}
	b, _ = fx.st.GetBuild(ctx, b.ID)
	fx.f.AddImage(dockertest.Image{ID: img, Tags: []string{"golem-deploy/game:" + commit}, AssignPort: fx.admin.Port,
		Labels: dockertest.BuildLabels(buildRef(b))})
	return b
}

func TestEngineEnvContract(t *testing.T) {
	port := 7001
	sv := store.Server{ID: 3, Port: &port, AllowedOrigins: []string{"https://play.example.com:7001", "https://web.example.com"}}
	got := engineEnv(sv, nil, runtimeParams{gameHost: "play.example.com", certMode: certs.ModeSelfSigned,
		testOrigins: []string{"https://web.example.com", "https://panel.example.com"}}, "tok")
	want := map[string]string{
		"GOLEM_ADDR":                ":7777",
		"GOLEM_HTTP_ADDR":           ":7777",
		"GOLEM_TLS_CERT_FILE":       "/run/golem-deploy/tls/tls.crt",
		"GOLEM_TLS_KEY_FILE":        "/run/golem-deploy/tls/tls.key",
		"GOLEM_TLS_RELOAD_INTERVAL": "30s",
		"GOLEM_PUBLIC_URL":          "https://play.example.com:7001/wt",
		"GOLEM_WS_FALLBACK_URL":     "wss://play.example.com:7001/api/ws",
		"GOLEM_WT_ALLOWED_ORIGINS":  "https://play.example.com:7001,https://web.example.com,https://panel.example.com",
		"GOLEM_TLS_PINNED":          "1",
		"GOLEM_ADMIN_ADDR":          ":7778",
		"GOLEM_ADMIN_TOKEN":         "tok",
		"GOLEM_SNAPSHOT_DIR":        "/data",
		"GOLEM_SNAPSHOT_INTERVAL":   "0",
	}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
		if !strings.HasPrefix(k, docker.ReservedEnvPrefix) {
			t.Errorf("%s outside the reserved prefix", k)
		}
	}
	v6 := engineEnv(sv, nil, runtimeParams{gameHost: "2001:db8::7", certMode: certs.ModeCustom}, "")
	if v6["GOLEM_PUBLIC_URL"] != "https://[2001:db8::7]:7001/wt" || v6["GOLEM_TLS_PINNED"] != "0" ||
		!strings.HasPrefix(v6["GOLEM_WT_ALLOWED_ORIGINS"], "https://[2001:db8::7]:7001,") {
		t.Fatalf("%v", v6)
	}
	if _, ok := v6[EnvAdminToken]; ok {
		t.Fatal("empty token injected")
	}
	profiled := sv
	profiled.Profiling = true
	if on := engineEnv(profiled, nil, runtimeParams{gameHost: "play.example.com"}, ""); on[EnvAdminProfiling] != "1" || len(on) != len(v6)+1 {
		t.Fatalf("profiling env %v", on)
	}
	// A build that recorded other transport paths (golem-deploy.yaml) gets
	// URLs with them; a build recorded before paths existed the defaults.
	custom := engineEnv(sv, &store.Build{WebTransportPath: "/game/wt", WebSocketPath: "/game/ws"}, runtimeParams{gameHost: "play.example.com"}, "")
	if custom["GOLEM_PUBLIC_URL"] != "https://play.example.com:7001/game/wt" || custom["GOLEM_WS_FALLBACK_URL"] != "wss://play.example.com:7001/game/ws" {
		t.Fatalf("%v", custom)
	}
	legacy := engineEnv(sv, &store.Build{}, runtimeParams{gameHost: "play.example.com"}, "")
	if legacy["GOLEM_PUBLIC_URL"] != got["GOLEM_PUBLIC_URL"] || legacy["GOLEM_WS_FALLBACK_URL"] != got["GOLEM_WS_FALLBACK_URL"] {
		t.Fatalf("%v", legacy)
	}
	if publicURL("2001:db8::7", 7001) != "https://[2001:db8::7]:7001/" || publicURL("", 7001) != "" {
		t.Fatal("public url")
	}
}

func TestPanelOrigins(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		host string
		want string
	}{
		{"TLS default port", Config{PanelTLS: true, PanelPort: 443, PanelPublicHosts: []string{"panel.example", "panel.example"}}, "2001:db8::1",
			"https://panel.example"},
		{"TLS custom port", Config{PanelTLS: true, PanelPort: 8443}, "panel.example", "https://panel.example:8443"},
		{"development", Config{PanelLoopback: true, PanelPort: 8000}, "localhost", "http://localhost:8000,http://127.0.0.1:8000"},
		{"plaintext public listener", Config{PanelPort: 80, PanelPublicHosts: []string{"panel.example", "www.example"}}, "www.example",
			"http://panel.example,http://www.example,https://panel.example,https://www.example"},
		{"TLS proxy on loopback", Config{PanelLoopback: true, PanelPort: 8000, PanelPublicHosts: []string{"panel.example", "www.example"}}, "play.example",
			"http://panel.example:8000,http://www.example:8000,http://localhost:8000,http://127.0.0.1:8000,https://panel.example,https://www.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(tc.cfg.testOrigins(tc.host), ","); got != tc.want {
				t.Fatalf("%q, want %q", got, tc.want)
			}
		})
	}
}

// Everything that affects the container changes the config hash; the
// per-container admin token does not exist in it.
func TestConfigHash(t *testing.T) {
	fx := newFixture(t, Config{})
	svc := fx.svc
	port, port2 := 7001, 7002
	b := store.Build{ID: 1, ImageID: dockertest.ImageID(1)}
	b2 := store.Build{ID: 2, ImageID: dockertest.ImageID(1)}
	base := store.Server{ID: 1, Port: &port, Env: []byte("ct")}
	p := runtimeParams{gameHost: "play.example.com", certMode: certs.ModeSelfSigned, certDir: "/data/certs/game", groupAdd: []int{1000}}
	h := svc.configHash(base, &b, p)
	if h != svc.configHash(base, &b, p) {
		t.Fatal("not deterministic")
	}
	variants := map[string]func() string{
		"build":   func() string { return svc.configHash(base, &b2, p) },
		"nobuild": func() string { return svc.configHash(base, nil, p) },
		"port":    func() string { sv := base; sv.Port = &port2; return svc.configHash(sv, &b, p) },
		"env":     func() string { sv := base; sv.Env = []byte("ct2"); return svc.configHash(sv, &b, p) },
		"origins": func() string {
			sv := base
			sv.AllowedOrigins = []string{"https://web.example.com"}
			return svc.configHash(sv, &b, p)
		},
		"host": func() string { q := p; q.gameHost = "other.example.com"; return svc.configHash(base, &b, q) },
		"panel": func() string {
			q := p
			q.testOrigins = []string{"https://panel.example.com"}
			return svc.configHash(base, &b, q)
		},
		"mode":   func() string { q := p; q.certMode = certs.ModeCustom; return svc.configHash(base, &b, q) },
		"mount":  func() string { q := p; q.certDir = "/elsewhere"; return svc.configHash(base, &b, q) },
		"groups": func() string { q := p; q.groupAdd = nil; return svc.configHash(base, &b, q) },
		"profiling": func() string { sv := base; sv.Profiling = true; return svc.configHash(sv, &b, p) },
	}
	for name, fn := range variants {
		if fn() == h {
			t.Errorf("%s does not change the config hash", name)
		}
	}
}

func TestValidateOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://Play.Example.com":       "https://play.example.com",
		"https://play.example.com:443":   "https://play.example.com",
		"http://localhost:5173":          "http://localhost:5173",
		"http://10.0.0.5:80":             "http://10.0.0.5",
		"HTTPS://[2001:DB8::1]:8443":     "https://[2001:db8::1]:8443",
		"https://[2001:db8::1]":          "https://[2001:db8::1]",
		"https://web.example.com.:7001":  "https://web.example.com:7001",
		"https://xn--bcher-kva.example/": "",
	} {
		got, err := ValidateOrigin(in)
		if want == "" {
			if err == nil {
				t.Errorf("%q accepted as %q", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "play.example.com", "ftp://x.example", "https://user@x.example", "https://x.example/path",
		"https://x.example?q", "https://x.example#f", "https://x.example:", "https://x.example:0", "https://x.example:99999",
		"https://x.example:07001", "https://*.example.com", "https://a,b.example", "javascript:alert(1)", "https://",
		"https://x.example:7001 ", "null", "https://x_y.example"} {
		if got, err := ValidateOrigin(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
	got, err := validateOrigins("allowed_origins", []string{"https://a.example", "https://A.example:443"})
	if err != nil || !slices.Equal(got, []string{"https://a.example"}) {
		t.Fatalf("%v %v", got, err)
	}
	many := make([]string, MaxAllowedOrigins+1)
	for i := range many {
		many[i] = "https://a.example:" + string(rune('1'+i%9)) + "000"
	}
	if _, err := validateOrigins("allowed_origins", many); err == nil {
		t.Fatal("too many origins accepted")
	}
	var fe *FieldError
	if _, err := validateOrigins("allowed_origins", []string{"https://ok.example", "bad"}); !errors.As(err, &fe) || fe.Field != "allowed_origins[1]" {
		t.Fatalf("%v", err)
	}
}

func startJob(t *testing.T, fx *fixture, id int64) (string, error) {
	t.Helper()
	var log bytes.Buffer
	err := fx.svc.Start(WithJobID(context.Background(), 7), id, &log)
	return log.String(), err
}

func failDetail(err error) string {
	var fe *jobs.SafeError
	if errors.As(err, &fe) {
		return fe.Msg
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// The start succeeds only once the game reports ready; the restart policy
// is applied after that, the schema fingerprint lands on the build, and
// the engine contract and certificate mount reach the container while the
// admin token never appears in argv or logs.
func TestHealthGatedStart(t *testing.T) {
	fx := newFixture(t, Config{})
	ctx := context.Background()
	b := fx.build(t, 1, commitA)
	sv, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "arena", BuildID: b.ID, Env: map[string]string{"SECRET": marker},
		AllowedOrigins: []string{"https://web.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	fx.admin.Set(func(s *gameadmintest.Server) { s.NotReadyFor = 3 })
	log, err := startJob(t, fx, sv.ID)
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	row, _ := fx.st.GetServer(ctx, sv.ID)
	ct, _ := fx.f.Container(row.ContainerID)
	if ct.RestartPolicy != docker.RestartPolicy {
		t.Fatalf("restart policy %q", ct.RestartPolicy)
	}
	token := ct.Env[EnvAdminToken]
	if len(token) != 64 || row.AdminPort != 0 || !row.StartComplete || len(row.AdminToken) == 0 || bytes.Contains(row.AdminToken, []byte(token)) {
		t.Fatalf("admin record: port %d token %d bytes", row.AdminPort, len(row.AdminToken))
	}
	if ct.Env["SECRET"] != marker || ct.Env[EnvPublicURL] != "https://play.example.com:7000/wt" ||
		ct.Env[EnvAllowedOrigins] != "https://play.example.com:7000,https://web.example.com" || ct.Env[EnvTLSPinned] != "1" {
		t.Fatalf("container env %v", ct.Env)
	}
	if len(ct.Binds) != 1 || !strings.Contains(ct.Binds[0], "target=/run/golem-deploy/tls,readonly") ||
		!strings.Contains(ct.Binds[0], "source="+fx.certs.ServerDir(sv.ID)) {
		t.Fatalf("binds %v", ct.Binds)
	}
	if ct.Labels[docker.LabelBuildID] != itoa(b.ID) || ct.Labels[docker.LabelCommit] != commitA || ct.Labels[docker.LabelEngineVersion] != "v0.4.0-rc.1" {
		t.Fatalf("labels %v", ct.Labels)
	}
	if !slices.Contains(ct.Ports, "127.0.0.1::7778/tcp") {
		t.Fatalf("ports %v", ct.Ports)
	}
	auth, paths := fx.admin.Requests()
	if len(paths) < 5 || paths[len(paths)-1] != "/info" {
		t.Fatalf("admin calls %v", paths)
	}
	for _, a := range auth {
		if a != "Bearer "+token {
			t.Fatalf("auth %q", a)
		}
	}
	if got, _ := fx.st.GetBuild(ctx, b.ID); got.SchemaFingerprint != "sf-0123456789abcdef" {
		t.Fatalf("fingerprint %q", got.SchemaFingerprint)
	}
	for _, c := range fx.f.Calls() {
		if strings.Contains(strings.Join(c.Args, " "), token) || strings.Contains(strings.Join(c.Args, " "), marker) {
			t.Fatalf("secret in argv %v", c.Args)
		}
	}
	if strings.Contains(fx.logs.String(), token) || strings.Contains(log, token) || strings.Contains(fx.logs.String(), marker) {
		t.Fatal("secret in logs")
	}
	v, _ := fx.svc.Get(ctx, sv.ID)
	if v.RestartRequired || v.State != apitypes.ServerStateRunning {
		t.Fatalf("%+v", v)
	}
	// Changing the game host or allowed origins makes it stale.
	if _, err := fx.st.UpdateSettings(ctx, store.SettingsUpdate{GameHost: ptr("other.example.com")}); err != nil {
		t.Fatal(err)
	}
	if v, _ := fx.svc.Get(ctx, sv.ID); !v.RestartRequired || v.PublicURL != "https://other.example.com:7000/" {
		t.Fatalf("%+v", v)
	}
}

func ptr[T any](v T) *T { return &v }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestStartFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("never ready", func(t *testing.T) {
		fx := newFixture(t, Config{StartTimeout: 150 * time.Millisecond})
		b := fx.build(t, 1, commitA)
		sv, _ := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "a", BuildID: b.ID, Env: map[string]string{"K": marker}})
		fx.admin.Set(func(s *gameadmintest.Server) { s.NeverReady = true })
		fx.f.AddImage(dockertest.Image{ID: b.ImageID, AssignPort: fx.admin.Port, Labels: dockertest.BuildLabels(buildRef(b)),
			Logs: []string{"booting with " + marker}})
		log, err := startJob(t, fx, sv.ID)
		d := failDetail(err)
		if !strings.Contains(d, "did not report ready within 150ms") || !strings.Contains(d, "/api/v1/servers/1/logs") || strings.Contains(d, "never answered") {
			t.Fatalf("detail %q", d)
		}
		row, _ := fx.st.GetServer(ctx, sv.ID)
		ct, ok := fx.f.Container(row.ContainerID)
		if !ok || ct.State != "exited" || ct.RestartPolicy != "no" {
			t.Fatalf("container kept stopped without restart policy: %+v", ct)
		}
		if strings.Contains(log, marker) || !strings.Contains(log, "container | ") {
			t.Fatalf("job log %s", log)
		}
	})
	t.Run("admin never answers", func(t *testing.T) {
		fx := newFixture(t, Config{StartTimeout: 100 * time.Millisecond})
		b := fx.build(t, 1, commitA)
		ln, _ := net.Listen("tcp4", "127.0.0.1:0")
		closed := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		fx.f.AddImage(dockertest.Image{ID: b.ImageID, AssignPort: closed, Labels: dockertest.BuildLabels(buildRef(b))})
		sv, _ := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "a", BuildID: b.ID})
		_, err := startJob(t, fx, sv.ID)
		if d := failDetail(err); !strings.Contains(d, "admin endpoint never answered") || !strings.Contains(d, "Runtime.Host") {
			t.Fatalf("detail %q", d)
		}
	})
	t.Run("container exits", func(t *testing.T) {
		fx := newFixture(t, Config{})
		b := fx.build(t, 1, commitA)
		code := 3
		fx.f.AddImage(dockertest.Image{ID: b.ImageID, AssignPort: fx.admin.Port, Labels: dockertest.BuildLabels(buildRef(b)), ExitOnRun: &code})
		sv, _ := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "a", BuildID: b.ID})
		_, err := startJob(t, fx, sv.ID)
		if d := failDetail(err); !strings.Contains(d, "exited before the game became ready (exit code 3)") {
			t.Fatalf("detail %q", d)
		}
	})
	t.Run("hostile admin", func(t *testing.T) {
		fx := newFixture(t, Config{StartTimeout: 150 * time.Millisecond})
		b := fx.build(t, 1, commitA)
		fx.admin.Set(func(s *gameadmintest.Server) { s.Mode = gameadmintest.Redirect })
		sv, _ := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "a", BuildID: b.ID})
		log, err := startJob(t, fx, sv.ID)
		if d := failDetail(err); !strings.Contains(d, "did not report ready") || strings.Contains(d, "169.254") {
			t.Fatalf("detail %q", d)
		}
		if !strings.Contains(log, "outside the Golem Engine admin contract") || strings.Contains(log, "169.254") {
			t.Fatalf("log %s", log)
		}
	})
}

// A server runs only the image recorded on its successful build, and only
// while that image still carries this installation's labels for the build.
func TestImageTrust(t *testing.T) {
	ctx := context.Background()
	for name, labels := range map[string]func(b store.Build) map[string]string{
		"label only": func(store.Build) map[string]string { return dockertest.ManagedLabels() },
		"wrong instance": func(b store.Build) map[string]string {
			return dockertest.ManagedLabels(docker.LabelInstance, dockertest.IntegrationInstanceID, docker.LabelBuildID, itoa(b.ID))
		},
		"wrong build": func(b store.Build) map[string]string {
			r := buildRef(b)
			r.BuildID++
			return dockertest.BuildLabels(r)
		},
		"wrong commit": func(b store.Build) map[string]string {
			return dockertest.BuildLabels(buildRef(b), docker.LabelCommit, strings.Repeat("f", 40))
		},
		"extra namespace label": func(b store.Build) map[string]string {
			return dockertest.BuildLabels(buildRef(b), docker.LabelConfigHash, "x")
		},
		"no build": func(store.Build) map[string]string { return dockertest.OurLabels() },
	} {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t, Config{})
			b := fx.build(t, 1, commitA)
			fx.f.AddImage(dockertest.Image{ID: b.ImageID, AssignPort: fx.admin.Port, Labels: labels(b)})
			sv, _ := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "a", BuildID: b.ID})
			_, err := startJob(t, fx, sv.ID)
			if d := failDetail(err); !strings.Contains(d, "refusing to run it") {
				t.Fatalf("detail %q", d)
			}
			if len(fx.f.CallsTo("create")) != 0 {
				t.Fatal("docker run issued")
			}
		})
	}
	t.Run("image gone", func(t *testing.T) {
		fx := newFixture(t, Config{})
		b := fx.build(t, 1, commitA)
		sv, _ := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "a", BuildID: b.ID})
		fx.f.DeleteImage(b.ImageID)
		if _, err := startJob(t, fx, sv.ID); !strings.Contains(failDetail(err), "no longer exists") {
			t.Fatalf("%v", err)
		}
	})
}

func TestStartPreconditions(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, Config{})
	b := fx.build(t, 1, commitA)
	// Rows from before servers ran builds cannot start.
	p := 7005
	legacy, _ := fx.st.CreateServer(ctx, store.Server{Name: "legacy", Port: &p}, nil)
	if _, err := startJob(t, fx, legacy.ID); !strings.Contains(failDetail(err), "has no build") {
		t.Fatalf("%v", err)
	}
	if v, _ := fx.svc.Get(ctx, legacy.ID); v.BuildID != nil || v.ProjectID != nil || v.Image != "" {
		t.Fatalf("%+v", v)
	}
	// Only successful builds can be assigned.
	failed, _ := fx.st.CreateBuild(ctx, store.NewBuild{ProjectID: fx.project.ID})
	var fe *FieldError
	for _, id := range []int64{0, failed.ID, 999} {
		if _, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "x", BuildID: id}); !errors.As(err, &fe) || fe.Field != "build_id" {
			t.Fatalf("build %d: %v", id, err)
		}
		if _, err := fx.svc.Update(ctx, legacy.ID, apitypes.UpdateServerRequest{BuildID: &id}); !errors.As(err, &fe) || fe.Field != "build_id" {
			t.Fatalf("build %d: %v", id, err)
		}
	}
	// No game host anywhere: a clear failure before anything runs.
	nohost := newFixture(t, Config{})
	nohost.svc.cfg.DefaultGameHost = ""
	nb := nohost.build(t, 1, commitA)
	sv, _ := nohost.svc.Create(ctx, apitypes.CreateServerRequest{Name: "a", BuildID: nb.ID})
	if _, err := startJob(t, nohost, sv.ID); !strings.Contains(failDetail(err), "no game host is configured") {
		t.Fatalf("%v", err)
	}
	if len(nohost.f.CallsTo("create")) != 0 {
		t.Fatal("docker run issued")
	}
	_ = b
}

func TestRuntimeView(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, Config{})
	b := fx.build(t, 1, commitA)
	sv, _ := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "a", BuildID: b.ID})
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	if _, err := fx.svc.Runtime(ctx, sv.ID, now); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("got %v", err)
	}
	if _, err := startJob(t, fx, sv.ID); err != nil {
		t.Fatal(err)
	}
	rt, err := fx.svc.Runtime(ctx, sv.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if !rt.Ready || rt.Sessions != 3 || rt.Entities != 42 || rt.Tick.Rate != 20 || rt.EngineVersion != "v0.4.0-rc.1" ||
		rt.Transport != "webtransport" || rt.SchemaFingerprint != "sf-0123456789abcdef" || rt.GameName != "fake-game" || !rt.SampledAt.Equal(now) {
		t.Fatalf("%+v", rt)
	}
	// Hostile answers are bounded and sanitised.
	fx.admin.Set(func(s *gameadmintest.Server) { s.Mode = gameadmintest.Control })
	rt, err = fx.svc.Runtime(ctx, sv.ID, now)
	if err != nil || rt.Sessions != 0 || len(rt.GameName) > 128 || strings.ContainsAny(rt.GameName, "\x1b\u202e\x00") || rt.SchemaFingerprint != "" {
		t.Fatalf("%+v %v", rt, err)
	}
	fx.admin.Set(func(s *gameadmintest.Server) { s.Mode = gameadmintest.Huge })
	if _, err := fx.svc.Runtime(ctx, sv.ID, now); !errors.Is(err, ErrAdminUnavailable) {
		t.Fatalf("got %v", err)
	}
	fx.admin.Close()
	if _, err := fx.svc.Runtime(ctx, sv.ID, now); !errors.Is(err, ErrAdminUnavailable) {
		t.Fatalf("got %v", err)
	}
	var log bytes.Buffer
	if err := fx.svc.Stop(ctx, sv.ID, &log); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.Runtime(ctx, sv.ID, now); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("got %v", err)
	}
}

func TestExportImportResolvesBuilds(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, Config{})
	a1 := fx.build(t, 1, commitA)
	a2 := fx.build(t, 2, commitA)
	bb := fx.build(t, 3, commitB)
	if _, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "one", BuildID: a1.ID, Env: map[string]string{"K": "v"},
		AllowedOrigins: []string{"https://web.example.com"}}); err != nil {
		t.Fatal(err)
	}
	doc, err := fx.svc.Export(ctx)
	if err != nil || doc.FormatVersion != 2 || len(doc.Servers) != 1 {
		t.Fatalf("%+v %v", doc, err)
	}
	spec := doc.Servers[0]
	if spec.Project != "game" || spec.Commit != commitA || spec.Env["K"] != "v" || !slices.Equal(spec.AllowedOrigins, []string{"https://web.example.com"}) {
		t.Fatalf("%+v", spec)
	}
	// Import resolves the newest successful build of that commit.
	spec.Name, spec.Port = "two", nil
	created, err := fx.svc.Import(ctx, apitypes.ServerExport{FormatVersion: 2, Servers: []apitypes.ServerSpec{spec,
		{Name: "three", Project: "GAME", DesiredState: "running"}}})
	if err != nil {
		t.Fatal(err)
	}
	if *created[0].BuildID != a2.ID || *created[1].BuildID != bb.ID || created[1].DesiredState != "stopped" ||
		!slices.Equal(created[0].AllowedOrigins, spec.AllowedOrigins) {
		t.Fatalf("%+v", created)
	}
	var fe *FieldError
	for i, tc := range []struct {
		spec  apitypes.ServerSpec
		field string
	}{
		{apitypes.ServerSpec{Name: "x"}, "servers[0].project"},
		{apitypes.ServerSpec{Name: "x", Project: "nope"}, "servers[0].project"},
		{apitypes.ServerSpec{Name: "x", Project: "game", Commit: strings.Repeat("c", 40)}, "servers[0].commit"},
		{apitypes.ServerSpec{Name: "x", Project: "game", Commit: "abc"}, "servers[0].commit"},
		{apitypes.ServerSpec{Name: "x", Project: "game", AllowedOrigins: []string{"nope"}}, "servers[0].allowed_origins[0]"},
	} {
		if _, err := fx.svc.Import(ctx, apitypes.ServerExport{FormatVersion: 2, Servers: []apitypes.ServerSpec{tc.spec}}); !errors.As(err, &fe) || fe.Field != tc.field {
			t.Errorf("case %d: %v", i, err)
		}
	}
	if _, err := fx.svc.Import(ctx, apitypes.ServerExport{FormatVersion: 1, Servers: []apitypes.ServerSpec{spec}}); !errors.As(err, &fe) || fe.Field != "format_version" {
		t.Fatalf("%v", err)
	}
	// A project without any successful build fails cleanly.
	other, _ := fx.st.CreateProject(ctx, store.Project{Name: "empty", RepoURL: "https://example.com/e.git"}, nil)
	_ = other
	if _, err := fx.svc.Import(ctx, apitypes.ServerExport{FormatVersion: 2, Servers: []apitypes.ServerSpec{{Name: "x", Project: "empty"}}}); !errors.As(err, &fe) ||
		!strings.Contains(fe.Msg, "no successful build") {
		t.Fatalf("%v", err)
	}
}
