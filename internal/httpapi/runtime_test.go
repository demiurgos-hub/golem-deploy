package httpapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func TestServerRuntimeRoute(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1)
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: img.ID, Env: map[string]string{"K": envMarker}})
	path := fmt.Sprintf("/api/v1/servers/%d/runtime", s.ID)
	errBody(t, d.call("GET", path, d.devTok, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	errBody(t, d.call("GET", "/api/v1/servers/999/runtime", d.devTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	d.start(s.ID)
	rt := decode[apitypes.ServerRuntime](t, d.call("GET", path, d.devTok, nil))
	if !rt.Ready || rt.Sessions != 3 || rt.Entities != 42 || rt.Tick.Tick != 250 || rt.EngineVersion != "v0.4.0-rc.1" || rt.SchemaFingerprint != "sf-0123456789abcdef" {
		t.Fatalf("%+v", rt)
	}
	// The developer API token works (developer route); hostile answers are
	// contained.
	d.gameAdmin.Set(func(s *gameadmintest.Server) { s.Mode = gameadmintest.Control })
	rr := d.call("GET", path, d.devTok, nil)
	rt = decode[apitypes.ServerRuntime](t, rr)
	body := rr.Body.String()
	if strings.Contains(body, `\u001b`) || strings.Contains(body, "\u202e") || strings.Contains(body, `\u202e`) || len(rt.GameName) > 128 || rt.Sessions != 0 {
		t.Fatalf("hostile answer leaked: %s", body)
	}
	for _, mode := range []string{gameadmintest.Huge, gameadmintest.Redirect, gameadmintest.HTML} {
		d.gameAdmin.Set(func(s *gameadmintest.Server) { s.Mode = mode })
		e := errBody(t, d.call("GET", path, d.devTok, nil), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
		if !strings.Contains(e.Message, "admin endpoint did not answer") || strings.Contains(e.Message, "169.254") {
			t.Fatalf("%s: %+v", mode, e)
		}
	}
	d.gameAdmin.Close()
	errBody(t, d.call("GET", path, d.devTok, nil), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	d.action(d.devTok, s.ID, "stop")
	errBody(t, d.call("GET", path, d.devTok, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	d.assertNoMarker()
	d.assertNoAdminToken()
}

// assertNoAdminToken checks that no admin token of any container appears in
// responses, the daemon log, job logs or docker argv.
func (d *dockerHarness) assertNoAdminToken() {
	d.t.Helper()
	for _, c := range d.fake.Containers() {
		tok := c.Env[servers.EnvAdminToken]
		if tok == "" {
			continue
		}
		if strings.Contains(d.bodies.String(), tok) || strings.Contains(d.logs.String(), tok) {
			d.t.Fatal("admin token in a response or the daemon log")
		}
		for _, call := range d.fake.Calls() {
			if strings.Contains(strings.Join(call.Args, " "), tok) || strings.Contains(strings.Join(call.Env, " "), tok) {
				d.t.Fatalf("admin token in docker argv or CLI env %q", call.Args)
			}
		}
		list, _ := d.st.ListJobs(context.Background(), store.JobFilter{Limit: 500})
		for _, j := range list {
			if strings.Contains(j.Detail, tok) || strings.Contains(d.jobLog(j.ID), tok) {
				d.t.Fatalf("admin token in job %d", j.ID)
			}
		}
	}
}

func testCertPEM(t *testing.T, host string, notAfter time.Time) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}))
}

func TestGameCertificateRoutes(t *testing.T) {
	d := newDockerHarness(t)
	got := decode[apitypes.GameCertificate](t, d.call("GET", "/api/v1/certificates/game", d.adminSs, nil))
	if got.Mode != apitypes.CertificateModeSelfSigned || !got.Certificate.SelfSigned || !got.Certificate.CoversGameHost || got.GameHost != "play.example.com" ||
		len(got.Certificate.FingerprintSHA256) != 64 || got.Certificate.NotAfter.Sub(got.Certificate.NotBefore) > 14*24*time.Hour {
		t.Fatalf("%+v", got)
	}
	errBody(t, d.call("GET", "/api/v1/certificates/game", d.devTok, nil), http.StatusForbidden, apitypes.ErrorCodeForbidden)

	certPEM, keyPEM := testCertPEM(t, "play.example.com", time.Now().Add(60*24*time.Hour))
	rr := d.call("PUT", "/api/v1/certificates/game", d.adminSs, apitypes.UpdateGameCertificateRequest{Mode: "custom", CertificatePEM: certPEM, PrivateKeyPEM: keyPEM})
	custom := decode[apitypes.GameCertificate](t, rr)
	if custom.Mode != apitypes.CertificateModeCustom || !custom.Certificate.CoversGameHost || custom.Certificate.FingerprintSHA256 == got.Certificate.FingerprintSHA256 {
		t.Fatalf("%+v", custom)
	}
	keyBody := strings.Split(keyPEM, "\n")[1]
	if strings.Contains(d.bodies.String(), keyBody) || strings.Contains(d.logs.String(), keyBody) {
		t.Fatal("private key echoed")
	}
	// Rejected uploads name the field, never the key.
	otherCert, otherKey := testCertPEM(t, "other.example.com", time.Now().Add(time.Hour))
	for _, tc := range []struct {
		req   apitypes.UpdateGameCertificateRequest
		field string
	}{
		{apitypes.UpdateGameCertificateRequest{Mode: "custom", CertificatePEM: certPEM, PrivateKeyPEM: otherKey}, "private_key_pem"},
		{apitypes.UpdateGameCertificateRequest{Mode: "custom", CertificatePEM: "junk", PrivateKeyPEM: keyPEM}, "certificate_pem"},
		{apitypes.UpdateGameCertificateRequest{Mode: "custom", CertificatePEM: otherCert}, "private_key_pem"},
		{apitypes.UpdateGameCertificateRequest{Mode: "self_signed", CertificatePEM: certPEM}, "certificate_pem"},
		{apitypes.UpdateGameCertificateRequest{Mode: "acme"}, "mode"},
	} {
		e := errBody(t, d.call("PUT", "/api/v1/certificates/game", d.adminSs, tc.req), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
		if e.Field != tc.field || strings.Contains(e.Message, "PRIVATE KEY") {
			t.Fatalf("%+v", e)
		}
	}
	if strings.Contains(d.bodies.String(), strings.Split(otherKey, "\n")[1]) {
		t.Fatal("rejected key echoed")
	}
	// The custom certificate is kept across a game host change (but no
	// longer covers it), and a mode switch marks running servers stale.
	img := d.managedImage(1)
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: img.ID})
	d.start(s.ID)
	if ct, _ := d.fake.Container("golem-deploy-arena"); ct.Env["GOLEM_TLS_PINNED"] != "0" {
		t.Fatalf("custom certificates are not pinned: %v", ct.Env)
	}
	set := decode[apitypes.Settings](t, d.call("PATCH", "/api/v1/settings", d.adminSs, apitypes.UpdateSettingsRequest{GameHost: ptr("game2.example.com")}))
	if set.GameHost != "game2.example.com" || set.EffectiveGameHost != "game2.example.com" {
		t.Fatalf("%+v", set)
	}
	now := decode[apitypes.GameCertificate](t, d.call("GET", "/api/v1/certificates/game", d.adminSs, nil))
	if now.Certificate.FingerprintSHA256 != custom.Certificate.FingerprintSHA256 || now.Certificate.CoversGameHost {
		t.Fatalf("%+v", now)
	}
	back := decode[apitypes.GameCertificate](t, d.call("PUT", "/api/v1/certificates/game", d.adminSs, apitypes.UpdateGameCertificateRequest{Mode: "self_signed"}))
	if back.Mode != apitypes.CertificateModeSelfSigned || !back.Certificate.CoversGameHost {
		t.Fatalf("%+v", back)
	}
	if got := d.getServer(d.devTok, s.ID); !got.RestartRequired {
		t.Fatal("mode switch did not mark the server restart_required")
	}
}

func TestGameHostSetting(t *testing.T) {
	d := newDockerHarness(t)
	set := decode[apitypes.Settings](t, d.call("GET", "/api/v1/settings", d.adminSs, nil))
	if set.GameHost != "" || set.EffectiveGameHost != "play.example.com" {
		t.Fatalf("%+v", set)
	}
	first := decode[apitypes.GameCertificate](t, d.call("GET", "/api/v1/certificates/game", d.adminSs, nil))
	for _, bad := range []string{"https://x.example", "x.example:7000", "x.example/a", "a b", "[::1]", "*.example.com"} {
		e := errBody(t, d.call("PATCH", "/api/v1/settings", d.adminSs, apitypes.UpdateSettingsRequest{GameHost: &bad}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
		if e.Field != "game_host" {
			t.Fatalf("%+v", e)
		}
	}
	set = decode[apitypes.Settings](t, d.call("PATCH", "/api/v1/settings", d.adminSs, apitypes.UpdateSettingsRequest{GameHost: ptr("Game.Example.COM.")}))
	if set.GameHost != "game.example.com" || set.EffectiveGameHost != "game.example.com" || set.RegistrationEnabled {
		t.Fatalf("%+v", set)
	}
	// The self-signed certificate follows the game host.
	regen := decode[apitypes.GameCertificate](t, d.call("GET", "/api/v1/certificates/game", d.adminSs, nil))
	if regen.Certificate.FingerprintSHA256 == first.Certificate.FingerprintSHA256 || !regen.Certificate.CoversGameHost || regen.GameHost != "game.example.com" {
		t.Fatalf("%+v", regen)
	}
	// Clearing it falls back to --public-host.
	set = decode[apitypes.Settings](t, d.call("PATCH", "/api/v1/settings", d.adminSs, apitypes.UpdateSettingsRequest{GameHost: ptr("")}))
	if set.GameHost != "" || set.EffectiveGameHost != "play.example.com" {
		t.Fatalf("%+v", set)
	}
	errBody(t, d.call("PATCH", "/api/v1/settings", d.devTok, apitypes.UpdateSettingsRequest{GameHost: ptr("x.example")}), http.StatusForbidden, apitypes.ErrorCodeForbidden)
}
