package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandlerWithoutBuiltClient(t *testing.T) {
	h := newHandler(fstest.MapFS{".gitkeep": &fstest.MapFile{}})

	for _, path := range []string{"/", "/containers", "/.gitkeep"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status %d, want 503", path, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "npm run build") {
			t.Fatalf("%s: unexpected body %q", path, rr.Body.String())
		}
	}
}

func TestHandlerServesIndexAndFallback(t *testing.T) {
	h := newHandler(fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<html>app</html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	})

	cases := map[string]string{
		"/":              "<html>app</html>",
		"/containers":    "<html>app</html>",
		"/assets/app.js": "console.log(1)",
	}
	for path, want := range cases {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("%s: body %q does not contain %q", path, rr.Body.String(), want)
		}
	}
}
