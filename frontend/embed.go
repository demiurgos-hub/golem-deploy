// Package frontend embeds the built React web client.
package frontend

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// dist always contains a committed .gitkeep placeholder so the package builds
// on a fresh clone; the real files are produced by `npm run build`.
//
//go:embed all:dist
var distFS embed.FS

const notBuiltMessage = "Golem Deploy web client not built; run npm run build in frontend/ and rebuild the binary.\n"

// Handler returns an http.Handler that serves the embedded frontend files.
// It acts as a SPA fallback: any path that doesn't match a real file
// returns index.html so client-side routing works. When the web client was
// not built before compiling, every request gets a plain-text explanation.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("frontend: " + err.Error())
	}
	return newHandler(sub)
}

func newHandler(sub fs.FS) http.Handler {
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(notBuiltMessage))
		})
	}

	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			fileServer.ServeHTTP(w, r)
			return
		}

		cleanPath := strings.TrimPrefix(path, "/")
		if f, err := sub.Open(cleanPath); err == nil {
			f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}

		// SPA fallback: serve index.html for unknown paths
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
