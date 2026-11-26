package httpapi

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
)

func TestServerProfileRoute(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1)
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: img.ID, Env: map[string]string{"K": envMarker}})
	if !s.Profiling || s.ProfilingActive {
		t.Fatalf("new server: %+v", s)
	}
	path := fmt.Sprintf("/api/v1/servers/%d/profile", s.ID)
	errBody(t, d.call("GET", path+"?kind=heap", "", nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
	errBody(t, d.call("GET", path+"?kind=heap", d.devTok, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	errBody(t, d.call("GET", "/api/v1/servers/999/profile?kind=heap", d.devTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	for q, field := range map[string]string{
		"":                       "kind",
		"?kind=block":            "kind",
		"?kind=cpu&seconds=61":   "seconds",
		"?kind=cpu&seconds=x":    "seconds",
		"?kind=heap&seconds=5":   "seconds",
		"?kind=trace&seconds=16": "seconds",
	} {
		if e := errBody(t, d.call("GET", path+q, d.devTok, nil), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed); e.Field != field {
			t.Errorf("%q: field %q, want %q", q, e.Field, field)
		}
	}
	d.start(s.ID)
	if got := decode[apitypes.Server](t, d.call("GET", fmt.Sprintf("/api/v1/servers/%d", s.ID), d.devTok, nil)); !got.ProfilingActive {
		t.Fatalf("started server: %+v", got)
	}

	want := []byte{0x1f, 0x8b, 0x08, 0, 'h', 'e', 'a', 'p'}
	d.gameAdmin.Set(func(s *gameadmintest.Server) { s.ProfileBody = want })
	rr := d.call("GET", path+"?kind=cpu&seconds=2", d.devTok, nil)
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), want) {
		t.Fatalf("%d % x", rr.Code, rr.Body.Bytes())
	}
	h := rr.Header()
	if h.Get("Content-Type") != "application/octet-stream" || h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(h.Get("Content-Disposition"), `attachment; filename=arena-cpu-`) || !strings.HasSuffix(h.Get("Content-Disposition"), ".pb.gz") {
		t.Fatalf("headers %v", h)
	}
	if !strings.Contains(d.logs.String(), "server profile captured") {
		t.Fatal("the capture is not logged")
	}

	d.gameAdmin.Set(func(s *gameadmintest.Server) { s.ProfileStatus = http.StatusConflict })
	errBody(t, d.call("GET", path+"?kind=trace", d.devTok, nil), http.StatusConflict, apitypes.ErrorCodeProfileInProgress)
	d.gameAdmin.Set(func(s *gameadmintest.Server) { s.ProfileStatus = http.StatusNotFound })
	errBody(t, d.call("GET", path+"?kind=heap", d.devTok, nil), http.StatusConflict, apitypes.ErrorCodeProfilingDisabled)
	d.gameAdmin.Set(func(s *gameadmintest.Server) { s.ProfileStatus = http.StatusServiceUnavailable })
	errBody(t, d.call("GET", path+"?kind=heap", d.devTok, nil), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	d.gameAdmin.Set(func(s *gameadmintest.Server) { s.ProfileStatus, s.ProfileContentType = 0, "text/html" })
	errBody(t, d.call("GET", path+"?kind=heap", d.devTok, nil), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	d.gameAdmin.Set(func(s *gameadmintest.Server) { s.ProfileContentType = "" })

	// The switch off refuses at once, even while the old container runs.
	off := false
	rr = d.call("PATCH", fmt.Sprintf("/api/v1/servers/%d", s.ID), d.devTok, apitypes.UpdateServerRequest{Profiling: &off})
	if got := decode[apitypes.Server](t, rr); got.Profiling || !got.ProfilingActive || !got.RestartRequired {
		t.Fatalf("after turning it off: %+v", got)
	}
	errBody(t, d.call("GET", path+"?kind=heap", d.devTok, nil), http.StatusConflict, apitypes.ErrorCodeProfilingDisabled)
	d.assertNoMarker()
	d.assertNoAdminToken()
}

// A game that puts its admin token into a profile does not get it out.
func TestServerProfileRouteRefusesTheToken(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1)
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: img.ID})
	d.start(s.ID)
	var token string
	for _, c := range d.fake.Containers() {
		token = c.Env["GOLEM_ADMIN_TOKEN"]
	}
	d.gameAdmin.Set(func(s *gameadmintest.Server) { s.ProfileBody = []byte("leak:" + token) })
	errBody(t, d.call("GET", fmt.Sprintf("/api/v1/servers/%d/profile?kind=trace&seconds=1", s.ID), d.devTok, nil),
		http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	d.assertNoAdminToken()
}
