package httpapi

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// expectedRoutes is the access contract documented in docs/API.md. A route
// added, removed or re-levelled without updating this table fails the test.
var expectedRoutes = map[string]Access{
	"POST /api/v1/projects/{id}/builds": AccessDeveloper,
	"GET /api/v1/projects/{id}/builds":  AccessDeveloper,
	"GET /api/v1/builds":                AccessDeveloper,
	"GET /api/v1/builds/{id}":           AccessDeveloper,
	"DELETE /api/v1/builds/{id}":        AccessAdmin,
	"GET /api/v1/auth/options":          AccessPublic,
	"POST /api/v1/auth/login":           AccessPublic,
	"POST /api/v1/auth/register":        AccessPublic,
	"POST /api/v1/auth/logout":          AccessSession,
	"POST /api/v1/auth/refresh":         AccessSession,

	"GET /api/v1/status":     AccessDeveloper,
	"GET /api/v1/settings":   AccessAdmin,
	"PATCH /api/v1/settings": AccessAdmin,

	"GET /api/v1/account":                AccessDeveloper,
	"POST /api/v1/account/password":      AccessSession,
	"GET /api/v1/account/tokens":         AccessSession,
	"POST /api/v1/account/tokens":        AccessSession,
	"DELETE /api/v1/account/tokens/{id}": AccessSession,

	"GET /api/v1/accounts":         AccessAdmin,
	"POST /api/v1/accounts":        AccessAdmin,
	"PATCH /api/v1/accounts/{id}":  AccessAdmin,
	"DELETE /api/v1/accounts/{id}": AccessAdmin,

	"GET /api/v1/accounts/{id}/tokens":               AccessAdmin,
	"DELETE /api/v1/accounts/{id}/tokens/{token_id}": AccessAdmin,

	"GET /api/v1/jobs":              AccessDeveloper,
	"GET /api/v1/jobs/{id}":         AccessDeveloper,
	"POST /api/v1/jobs/{id}/cancel": AccessDeveloper,
	"GET /api/v1/jobs/{id}/log":     AccessDeveloper,

	"GET /api/v1/projects":               AccessDeveloper,
	"POST /api/v1/projects":              AccessAdmin,
	"GET /api/v1/projects/{id}":          AccessDeveloper,
	"PATCH /api/v1/projects/{id}":        AccessAdmin,
	"DELETE /api/v1/projects/{id}":       AccessAdmin,
	"POST /api/v1/projects/{id}/pull":    AccessDeveloper,
	"POST /api/v1/projects/{id}/branch":  AccessDeveloper,
	"GET /api/v1/projects/{id}/branches": AccessDeveloper,

	"GET /api/v1/servers":                   AccessDeveloper,
	"POST /api/v1/servers":                  AccessDeveloper,
	"GET /api/v1/servers/export":            AccessAdmin,
	"POST /api/v1/servers/import":           AccessAdmin,
	"GET /api/v1/servers/{id}":              AccessDeveloper,
	"PATCH /api/v1/servers/{id}":            AccessDeveloper,
	"DELETE /api/v1/servers/{id}":           AccessDeveloper,
	"POST /api/v1/servers/{id}/start":       AccessDeveloper,
	"POST /api/v1/servers/{id}/stop":        AccessDeveloper,
	"POST /api/v1/servers/{id}/restart":     AccessDeveloper,
	"GET /api/v1/servers/{id}/logs":         AccessDeveloper,
	"GET /api/v1/servers/{id}/stats":        AccessDeveloper,
	"GET /api/v1/servers/{id}/runtime":      AccessDeveloper,
	"GET /api/v1/servers/{id}/connectivity": AccessDeveloper,
	"POST /api/v1/servers/{id}/backups":     AccessDeveloper,
	"POST /api/v1/servers/{id}/restore":     AccessAdmin,
	"POST /api/v1/servers/{id}/data/reset":  AccessAdmin,
	"GET /api/v1/backups":                   AccessDeveloper,
	"GET /api/v1/backups/{id}":              AccessDeveloper,
	"GET /api/v1/backups/{id}/download":     AccessDeveloper,
	"DELETE /api/v1/backups/{id}":           AccessAdmin,
	"GET /api/v1/inspector":                 AccessDeveloper,
	"PATCH /api/v1/inspector":               AccessAdmin,
	"POST /api/v1/inspector/sessions":       AccessSession,
	"GET /api/v1/certificates/game":         AccessAdmin,
	"PUT /api/v1/certificates/game":         AccessAdmin,
	"GET /api/v1/certificates/panel":        AccessAdmin,
	"PUT /api/v1/certificates/panel":        AccessAdmin,
	"GET /api/v1/certificates/acme":         AccessAdmin,
	"PUT /api/v1/certificates/acme":         AccessAdmin,
	"POST /api/v1/certificates/acme/renew":  AccessAdmin,

	"GET /api/v1/images":         AccessDeveloper,
	"DELETE /api/v1/images/{id}": AccessAdmin,

	"GET /api/v1/static":           AccessDeveloper,
	"POST /api/v1/static":          AccessDeveloper,
	"GET /api/v1/static/{name}":    AccessDeveloper,
	"DELETE /api/v1/static/{name}": AccessDeveloper,

	"GET /api/v1/volumes":           AccessDeveloper,
	"DELETE /api/v1/volumes/{name}": AccessAdmin,
}

func TestEveryRouteHasExplicitAccess(t *testing.T) {
	h := newHarness(t)
	seen := map[string]bool{}
	patternRE := regexp.MustCompile(`^(GET|POST|PATCH|PUT|DELETE) /api/v1/[a-z0-9/{}_-]+$`)
	for _, rt := range h.srv.routes() {
		p := rt.pattern()
		if seen[p] {
			t.Errorf("duplicate route %s", p)
		}
		seen[p] = true
		if !patternRE.MatchString(p) {
			t.Errorf("route %q is not a method-qualified /api/v1 pattern", p)
		}
		if rt.access < AccessPublic || rt.access > AccessAdmin {
			t.Errorf("route %s has no access level", p)
		}
		want, ok := expectedRoutes[p]
		if !ok {
			t.Errorf("route %s (%s) missing from expectedRoutes", p, rt.access)
			continue
		}
		if want != rt.access {
			t.Errorf("route %s access %s, want %s", p, rt.access, want)
		}
	}
	for p := range expectedRoutes {
		if !seen[p] {
			t.Errorf("expected route %s is not registered", p)
		}
	}
	// Registering a route without a level is a programming error.
	defer func() {
		if recover() == nil {
			t.Error("register accepted a route without access level")
		}
	}()
	h.srv.register(route{method: "GET", path: "/x", handler: func(http.ResponseWriter, *http.Request) error { return nil }})
}

// TestRoleMatrix sends every route a request as each identity and checks the
// access layer's verdict. Handlers may still reject a request for other
// reasons (validation, not found, not implemented), so only the auth codes
// are asserted.
func TestRoleMatrix(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	dev := h.account("dev1", store.RoleDeveloper, true)
	adminToken := h.apiToken(admin, nil)
	devToken := h.apiToken(dev, nil)

	type ident struct {
		name  string
		token func() string
		// level is what this identity may reach.
		allowed map[Access]bool
		// denial is the code for routes it may not reach.
		denial func(Access) apitypes.ErrorCode
	}
	idents := []ident{
		{"anonymous", func() string { return "" },
			map[Access]bool{AccessPublic: true},
			func(Access) apitypes.ErrorCode { return apitypes.ErrorCodeUnauthenticated }},
		{"developer api token", func() string { return devToken },
			map[Access]bool{AccessPublic: true, AccessDeveloper: true},
			func(a Access) apitypes.ErrorCode {
				if a == AccessSession {
					return apitypes.ErrorCodeSessionRequired
				}
				return apitypes.ErrorCodeForbidden
			}},
		{"admin api token", func() string { return adminToken },
			map[Access]bool{AccessPublic: true, AccessDeveloper: true},
			func(a Access) apitypes.ErrorCode {
				if a == AccessSession {
					return apitypes.ErrorCodeSessionRequired
				}
				return apitypes.ErrorCodeForbidden
			}},
		{"developer session", func() string { return h.session(dev) },
			map[Access]bool{AccessPublic: true, AccessDeveloper: true, AccessSession: true},
			func(Access) apitypes.ErrorCode { return apitypes.ErrorCodeForbidden }},
		{"admin session", func() string { return h.session(admin) },
			map[Access]bool{AccessPublic: true, AccessDeveloper: true, AccessSession: true, AccessAdmin: true},
			nil},
	}
	authCodes := map[apitypes.ErrorCode]bool{apitypes.ErrorCodeUnauthenticated: true, apitypes.ErrorCodeForbidden: true, apitypes.ErrorCodeSessionRequired: true}
	pathFill := strings.NewReplacer("{id}", "999999", "{name}", "nope", "{token_id}", "999999")

	for _, rt := range h.srv.routes() {
		path := "/api/v1" + pathFill.Replace(rt.path)
		for _, id := range idents {
			t.Run(rt.method+" "+rt.path+" as "+id.name, func(t *testing.T) {
				rr := h.do(rt.method, path, id.token(), "{}")
				if id.allowed[rt.access] {
					if rr.Code == http.StatusUnauthorized || rr.Code == http.StatusForbidden {
						var code apitypes.ErrorCode
						if rr.Code >= 400 {
							code = decode[apitypes.ErrorResponse](t, rr).Error.Code
						}
						if authCodes[code] {
							t.Fatalf("allowed identity denied: %d %s", rr.Code, code)
						}
					}
					return
				}
				want := id.denial(rt.access)
				status := http.StatusForbidden
				if want == apitypes.ErrorCodeUnauthenticated {
					status = http.StatusUnauthorized
				}
				errBody(t, rr, status, want)
			})
		}
	}
}

func TestNotImplementedRoutesUseEnvelope(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	// A route of its own, so the test does not depend on which planned
	// routes are still unimplemented.
	h.srv.register(route{method: "GET", path: "/test-not-implemented", access: AccessDeveloper,
		handler: func(http.ResponseWriter, *http.Request) error { return errNotImplemented }})
	rr := h.do("GET", "/api/v1/test-not-implemented", h.session(admin), nil)
	errBody(t, rr, http.StatusNotImplemented, apitypes.ErrorCodeNotImplemented)
}
