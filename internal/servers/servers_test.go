package servers

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func sanitize(prefix string, chunks ...string) string {
	var out bytes.Buffer
	s := newSanitizer(&out, prefix)
	for _, c := range chunks {
		_, _ = s.Write([]byte(c))
	}
	_ = s.Close()
	return out.String()
}

// D9: control sequences, forged lines, broken UTF-8 (also across chunk
// boundaries) and over-long lines.
func TestD9_Sanitizer(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"ansi and osc", []string{"\x1b[31mred\x1b[0m \x1b]0;title\x07 bell\x07"}, `\x1b[31mred\x1b[0m \x1b]0;title\x07 bell\x07` + "\n"},
		{"carriage return overwrite", []string{"ok\rFAKE daemon line\n"}, `ok\x0dFAKE daemon line` + "\n"},
		{"c1 and separators", []string{"a\u0085b\u2028c\u202ed\u009be"}, `a\u0085b\u2028c\u202ed\u009be` + "\n"},
		{"tab and del", []string{"a\tb\x7fc\x00"}, "a\tb" + `\x7fc\x00` + "\n"},
		{"invalid utf8", []string{"a\xffb\xc3"}, "a\uFFFDb\uFFFD\n"},
		{"utf8 split across writes", []string{"caf\xc3", "\xa9 ", "\xe6\x97", "\xa5\n"}, "café 日\n"},
		{"line split across writes", []string{"hel", "lo\nwor", "ld"}, "hello\nworld\n"},
		{"empty lines kept", []string{"\n\n"}, "\n\n"},
	}
	for _, c := range cases {
		if got := sanitize("", c.chunks...); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// A line pretending to be a daemon message is visibly container output.
	got := sanitize(JobLogPrefix, "server \"arena\" is running\nerror: fake\n")
	if got != "container | server \"arena\" is running\ncontainer | error: fake\n" {
		t.Fatalf("%q", got)
	}
	// Long lines are cut with a marker, also mid-character, and the rest
	// of the line is dropped (memory stays bounded).
	long := strings.Repeat("é", MaxLogLine) // 2 bytes each
	got = sanitize("", long[:len(long)-1], "tail\nnext\n")
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], TruncatedMarker) || lines[1] != "next" {
		t.Fatalf("lines %d %q", len(lines), lines[len(lines)-1])
	}
	body := strings.TrimSuffix(lines[0], TruncatedMarker)
	if len(body) > MaxLogLine || !utf8.ValidString(body) || strings.ContainsRune(body, utf8.RuneError) {
		t.Fatalf("cut line: %d bytes, valid %v", len(body), utf8.ValidString(body))
	}
	s := newSanitizer(&bytes.Buffer{}, "")
	for i := 0; i < 1000; i++ {
		_, _ = s.Write(bytes.Repeat([]byte("x"), 64<<10))
	}
	if len(s.line) > MaxLogLine+utf8.UTFMax {
		t.Fatalf("buffer grew to %d bytes", len(s.line))
	}
}

// D8: every line of every env value of at least 4 bytes is redacted
// wherever it appears; shorter pieces are left alone.
func TestD8_Redact(t *testing.T) {
	env := map[string]string{
		"A":      "first-secret",
		"LEGACY": "line-one\nline-two\r\nxy", // stored before multi-line values were refused
		"SHORT":  "abc",
		"NESTED": "first-secret-plus",
	}
	out := string(redact([]byte("a first-secret-plus b first-secret c line-two d line-one e xy abc\n"), env))
	if out != "a [redacted] b [redacted] c [redacted] d [redacted] e xy abc\n" {
		t.Fatalf("%q", out)
	}
}

func TestMapState(t *testing.T) {
	for in, want := range map[string]apitypes.ServerState{
		"created": apitypes.ServerStateCreated, "running": apitypes.ServerStateRunning, "restarting": apitypes.ServerStateRestarting,
		"paused": apitypes.ServerStatePaused, "dead": apitypes.ServerStateDead, "exited": apitypes.ServerStateExited,
		"removing": apitypes.ServerStateExited, "": apitypes.ServerStateExited,
	} {
		if got := mapState(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

// The view is built from the recorded container only, and restart_required
// follows the config hash.
func TestViewAndContainersIndex(t *testing.T) {
	fx := newFixture(t, Config{})
	svc, f, st := fx.svc, fx.f, fx.st
	ctx := context.Background()
	b := fx.build(t, 1, commitA)
	p := 7001
	sv, err := st.CreateServer(ctx, store.Server{Name: "a", BuildID: &b.ID, Port: &p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	other := 7002
	sv2, _ := st.CreateServer(ctx, store.Server{Name: "b", BuildID: &b.ID, Port: &other}, nil)
	params, _ := svc.params(ctx)
	recorded := f.AddContainer(dockertest.Container{Name: "golem-deploy-a", Image: b.ImageID,
		Labels: dockertest.OurLabels(docker.LabelServerID, "1", docker.LabelConfigHash, svc.configHash(sv, &b, params))})
	impostor := f.AddContainer(dockertest.Container{Name: "x", Image: b.ImageID, Labels: dockertest.OurLabels(docker.LabelServerID, "2")})
	if err := st.SetServerContainer(ctx, sv.ID, recorded, 0, nil); err != nil {
		t.Fatal(err)
	}
	// sv2 records a container that belongs to server 1: not accepted.
	if err := st.SetServerContainer(ctx, sv2.ID, recorded, 0, nil); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.ListServers(ctx)
	cts, _ := svc.docker.ManagedContainers(ctx)
	idx := svc.containers(rows, cts)
	if idx[sv.ID] == nil || idx[sv.ID].ID != recorded || idx[sv2.ID] != nil {
		t.Fatalf("index %+v", idx)
	}
	if _, logged := svc.foreign.Load(impostor); !logged {
		t.Fatal("impostor not logged")
	}
	list, err := svc.List(ctx)
	if err != nil || list[0].State != apitypes.ServerStateRunning || list[0].RestartRequired || list[1].State != apitypes.ServerStateMissing {
		t.Fatalf("%+v %v", list, err)
	}
	if v := list[0]; *v.BuildID != b.ID || *v.ProjectID != b.ProjectID || v.Image != b.ImageID || v.Commit != commitA ||
		v.EngineVersion != "v0.4.0-rc.1" || v.PublicURL != "https://play.example.com:7001/" {
		t.Fatalf("%+v", v)
	}
	// A definition change makes the running container stale.
	if _, err := svc.Update(ctx, sv.ID, apitypes.UpdateServerRequest{EnvSet: map[string]string{"A": "1"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Get(ctx, sv.ID); !got.RestartRequired {
		t.Fatalf("%+v", got)
	}
}

func TestPortAllowedAndFieldNames(t *testing.T) {
	fx := newFixture(t, Config{Ports: ports.Range{Lo: 6000, Hi: 6010}, PanelPort: 6005})
	svc := fx.svc
	for p, ok := range map[int]bool{6001: true, 6000: false /* browser-unsafe */, 6005: false /* panel */, 6011: false} {
		if svc.portAllowed(p) != ok {
			t.Errorf("port %d", p)
		}
	}
	err := validateEnv("servers[2].env", map[string]string{"K": "a\nb"})
	if fe, ok := err.(*FieldError); !ok || fe.Field != "servers[2].env.K" {
		t.Fatalf("%v", err)
	}
	err = validateEnv("env", map[string]string{"GOLEM_X": "1"})
	if fe, ok := err.(*FieldError); !ok || fe.Field != "env" {
		t.Fatalf("%v", err)
	}
	if !equalFold("Mode", "MODE") || equalFold("a", "b") || equalFold("a", "ab") {
		t.Fatal("equalFold")
	}
}

// Start records the container id, applies the restart policy only once the
// game reports ready, and Delete clears everything.
func TestStartRecordsContainer(t *testing.T) {
	fx := newFixture(t, Config{})
	svc, f, st := fx.svc, fx.f, fx.st
	ctx := context.Background()
	b := fx.build(t, 1, commitA)
	created, err := svc.Create(ctx, apitypes.CreateServerRequest{Name: "a", BuildID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if err := svc.Start(WithJobID(ctx, 42), created.ID, &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	row, _ := st.GetServer(ctx, created.ID)
	ct, ok := f.Container(row.ContainerID)
	if !ok || ct.RestartPolicy != docker.RestartPolicy || row.DesiredState != "running" {
		t.Fatalf("row %+v container %+v", row, ct)
	}
	// A restart replaces the recorded container.
	if err := svc.Restart(ctx, created.ID, &log); err != nil {
		t.Fatal(err)
	}
	row2, _ := st.GetServer(ctx, created.ID)
	if row2.ContainerID == row.ContainerID || len(f.Containers()) != 1 {
		t.Fatalf("restart: %q -> %q, %d containers", row.ContainerID, row2.ContainerID, len(f.Containers()))
	}
	if err := svc.Delete(ctx, created.ID, &log); err != nil {
		t.Fatal(err)
	}
	if len(f.Containers()) != 0 {
		t.Fatal("container left")
	}
}
